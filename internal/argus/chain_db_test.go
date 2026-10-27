package argus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The db step through the WHOLE executor path (claims, preflight, DBStep, chain.Run), with a fake
// querier behind dialPostgres. No database, no connection of any kind.

type argusFakeDB struct {
	mu     sync.Mutex
	rows   chain.DBRows
	err    error
	dials  int
	dialed []chain.DBConn
	calls  []string
	params [][]string
}

func (f *argusFakeDB) Query(_ context.Context, q string, p []string, _ time.Duration, limit int) (chain.DBRows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)
	f.params = append(f.params, append([]string(nil), p...))
	if f.err != nil {
		return chain.DBRows{}, f.err
	}
	out := f.rows
	if limit > 0 && len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
	}
	return out, nil
}
func (f *argusFakeDB) Close() error { return nil }

func withFakeDB(t *testing.T, f *argusFakeDB) {
	t.Helper()
	prev := dialPostgres
	dialPostgres = func(_ context.Context, c chain.DBConn) (chain.DBQuerier, error) {
		f.mu.Lock()
		f.dials++
		f.dialed = append(f.dialed, c)
		f.mu.Unlock()
		return f, nil
	}
	t.Cleanup(func() { dialPostgres = prev })
}

func dbConfig() *config.Config {
	c := &config.Config{}
	c.Targets.Database = &config.DBTarget{JDBCURL: "jdbc:postgresql://db.internal:5432/orders?sslmode=require",
		Username: "argus_ro", Password: "pw-SECRET-123"}
	c.Targets.DatabaseTargets = map[string]*config.DBTarget{
		"ledger": {JDBCURL: "jdbc:postgresql://ledger.internal/ledger", Username: "ro2", Password: "ledger-pw-2"},
		"legacy": {JDBCURL: "jdbc:oracle:thin:@h:1521:sid", Username: "u", Password: "other-pw-9"},
		"extra":  {JDBCURL: "jdbc:postgresql://h/db?connectTimeout=5", Username: "u", Password: "other-pw-8"},
	}
	return c
}

// dbChain returns a chain: an http step that counts its requests, then the given db step JSON.
func dbChain(t *testing.T, dbStep string, extraExpect string) (*scenario.Scenario, *int32, func()) {
	t.Helper()
	var hits int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"id":"o-1"}`))
	}))
	trig := `{"steps":[{"type":"http","name":"mk","method":"GET","url":"` + api.URL + `/o","save":{"oid":"id"}},` + dbStep + `]}`
	expect := "### Runnable\n- step mk: status=200\n" + extraExpect
	return scenario.Parse(chainRunMD("", trig, expect)), &hits, api.Close
}

const dbGoodStep = `{"type":"db","name":"q","query":"SELECT status FROM orders WHERE id = $1","params":["${saved.oid}"]}`
const dbGoodExpect = "- step q: body has rows.0.status equals PAID\n"

func TestDBChain_RunsEndToEnd_ParamFromASavedValue(t *testing.T) {
	f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"status"}, Rows: [][]any{{"PAID"}}}}
	withFakeDB(t, f)
	s, hits, stop := dbChain(t, dbGoodStep, dbGoodExpect)
	defer stop()
	res := runChainScenario(dbConfig(), s, "tr-db-e2e", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("want passed: %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if *hits != 1 || len(f.calls) != 1 {
		t.Fatalf("one http request then one query: %d / %d", *hits, len(f.calls))
	}
	if f.calls[0] != "SELECT status FROM orders WHERE id = $1" || len(f.params[0]) != 1 || f.params[0][0] != "o-1" {
		t.Fatalf("the saved id travels as a parameter, the text is the scenario's own: %q %q", f.calls[0], f.params[0])
	}
	if c := f.dialed[0]; c.Host != "db.internal" || c.Database != "orders" || c.User != "argus_ro" || c.SSLMode != "require" {
		t.Fatalf("connected to targets.database: %+v", c)
	}
	if got := res.Steps[1].AssertionsEnforced; len(got) != 1 {
		t.Fatalf("assertions_enforced: %v", got)
	}
}

func TestDBChain_TargetByName(t *testing.T) {
	f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"status"}, Rows: [][]any{{"PAID"}}}}
	withFakeDB(t, f)
	step := `{"type":"db","name":"q","target":"ledger","query":"SELECT status FROM orders WHERE id = $1","params":["${saved.oid}"]}`
	s, _, stop := dbChain(t, step, dbGoodExpect)
	defer stop()
	res := runChainScenario(dbConfig(), s, "tr-db-named", "testkit/ui")
	if res.Status != "passed" || f.dialed[0].Host != "ledger.internal" || f.dialed[0].User != "ro2" {
		t.Fatalf("`target` selects targets.database_targets.<name>: %q %+v", res.Status, f.dialed)
	}
}

func TestDBChain_RunIdIsFilledIntoTheText_AndEnvIsNeverSplicedIn(t *testing.T) {
	t.Setenv("DB_ENVVAL", "from-the-environment")
	f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"n"}, Rows: [][]any{{json.Number("1")}}}}
	withFakeDB(t, f)
	step := `{"type":"db","name":"q","query":"SELECT n FROM t WHERE tag = '${correlation_id}' AND cid8 = '${cid8}'"}`
	s, _, stop := dbChain(t, step, "- step q: body has row_count == 1\n")
	defer stop()
	res := runChainScenario(dbConfig(), s, "tr-db-cid", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("%q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if !strings.Contains(f.calls[0], "tag = 'tr-db-cid'") || strings.Contains(f.calls[0], "${") {
		t.Fatalf("the run's own id is placed in the text: %q", f.calls[0])
	}
	// an environment placeholder is refused, never filled in
	bad := `{"type":"db","name":"q","query":"SELECT n FROM t WHERE tag = '${DB_ENVVAL}'"}`
	s2, _, stop2 := dbChain(t, bad, "- step q: body has row_count == 1\n")
	defer stop2()
	res2 := runChainScenario(dbConfig(), s2, "tr-db-env", "testkit/ui")
	if res2.Status != "failed" || !strings.Contains(res2.Failure.Observed, "carries `${DB_ENVVAL}`, which nothing fills in") {
		t.Fatalf("an environment placeholder in the query is refused: %q %+v", res2.Status, res2.Failure)
	}
	if len(f.calls) != 1 {
		t.Fatal("the refused chain must not have queried")
	}
}

// Every preflight refusal: the SAME sentence the authoring validator uses (asserted against Validate for the
// shared rules), the chain fails as `failed` with the preflight lead, an http step placed BEFORE the db step has
// NOT fired (the requests the server counted), and no connection was opened.
func TestDBChain_PreflightRefusals_SameSentenceAsAuthoring_NothingFiredNothingDialled(t *testing.T) {
	cases := []struct {
		name     string
		step     string
		expect   string
		want     string
		authored bool // the authoring validator refuses it with the same sentence
	}{
		{"a write", `{"type":"db","name":"q","query":"DELETE FROM orders WHERE id = $1","params":["1"]}`, dbGoodExpect,
			"a db step's query must begin with `SELECT` or `WITH`", true},
		{"a second statement", `{"type":"db","name":"q","query":"SELECT 1 FROM t; DROP TABLE t"}`, dbGoodExpect,
			"the db step's query contains more than one statement", true},
		{"a writing CTE", `{"type":"db","name":"q","query":"WITH x AS (INSERT INTO t (a) VALUES (1) RETURNING a) SELECT a FROM x"}`, dbGoodExpect,
			"the db step's query contains `INSERT`", true},
		{"a saved value in the text", `{"type":"db","name":"q","query":"SELECT status FROM orders WHERE id = '${saved.oid}'"}`, dbGoodExpect,
			"must never be spliced into SQL text", true},
		{"param count", `{"type":"db","name":"q","query":"SELECT status FROM orders WHERE id = $1","params":["a","b"]}`, dbGoodExpect,
			"but `params` passes 2 value(s)", true},
		{"a stray field", `{"type":"db","name":"q","query":"SELECT status FROM orders","method":"GET"}`, dbGoodExpect,
			`this db step carries the field "method"`, true},
		{"a status claim", dbGoodStep, "- step q: status=200\n", "is a status claim - a db step has no HTTP status", true},
		{"max_rows out of range", `{"type":"db","name":"q","query":"SELECT status FROM orders","max_rows":5000}`, dbGoodExpect,
			"`max_rows` is 5000, outside 1..1000", true},
		{"timeout over the bound", `{"type":"db","name":"q","query":"SELECT status FROM orders","timeout":"2m"}`, dbGoodExpect,
			"more than the 30s bound on a db step", true},
		{"poll interval under 1s", `{"type":"db","name":"q","query":"SELECT status FROM orders","poll":{"timeout":"10s","interval":"100ms"}}`, dbGoodExpect,
			"which is under 1s - a check must not hammer a database", true},
		{"no claim", dbGoodStep, "", "needs at least one `body …` claim", false},
		{"an unknown target", `{"type":"db","name":"q","target":"nope","query":"SELECT status FROM orders"}`, dbGoodExpect,
			"nope", false},
		{"another dialect", `{"type":"db","name":"q","target":"legacy","query":"SELECT status FROM orders"}`, dbGoodExpect,
			"a db step reads PostgreSQL targets only", false},
		{"an unmapped URL parameter", `{"type":"db","name":"q","target":"extra","query":"SELECT status FROM orders"}`, dbGoodExpect,
			`the parameter "connectTimeout", which a db step does not map`, false},
		{"an unknown step type after the db step", `{"type":"sql","name":"q","query":"SELECT 1 FROM t"}`, dbGoodExpect,
			scenario.UnknownStepTypeProblem("sql"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"status"}, Rows: [][]any{{"PAID"}}}}
			withFakeDB(t, f)
			s, hits, stop := dbChain(t, c.step, c.expect)
			defer stop()
			res := runChainScenario(dbConfig(), s, "tr-db-pre", "testkit/ui")
			if res.Status != "failed" || res.Failure == nil || !strings.HasSuffix(res.Failure.Observed, "— preflight") {
				t.Fatalf("want a preflight failure, got %q %+v", res.Status, res.Failure)
			}
			if !strings.Contains(res.Failure.Observed, c.want) {
				t.Fatalf("want the sentence %q in %q", c.want, res.Failure.Observed)
			}
			if *hits != 0 {
				t.Fatalf("the http step placed BEFORE the refused step must not have fired, the server counted %d request(s)", *hits)
			}
			if f.dials != 0 || len(f.calls) != 0 {
				t.Fatalf("no connection may be opened for a refused chain: %d dial(s), %d query(ies)", f.dials, len(f.calls))
			}
			if len(res.Steps) != 0 {
				t.Fatalf("a refused chain runs no step: %+v", res.Steps)
			}
			if c.authored {
				_, errs := scenario.Validate(chainRunMD("", `{"steps":[{"type":"http","name":"mk","method":"GET","url":"http://x.invalid/o","save":{"oid":"id"}},`+c.step+`]}`,
					"### Runnable\n- step mk: status=200\n"+strings.TrimPrefix(c.expect, "### Runnable\n- step mk: status=200\n")))
				found := false
				for _, e := range errs {
					if strings.Contains(e.Message, c.want) {
						found = true
					}
				}
				if !found {
					t.Fatalf("the authoring validator must refuse it with the same sentence %q; got %v", c.want, errs)
				}
			}
		})
	}
}

func TestDBChain_NoDatabaseDeclaredIsRefusedNamingWhatIsMissing(t *testing.T) {
	f := &argusFakeDB{}
	withFakeDB(t, f)
	s, hits, stop := dbChain(t, dbGoodStep, dbGoodExpect)
	defer stop()
	res := runChainScenario(&config.Config{}, s, "tr-db-nodb", "testkit/ui")
	if res.Status != "failed" || !strings.Contains(res.Failure.Observed, "argus-config.yaml declares no targets.database") || *hits != 0 || f.dials != 0 {
		t.Fatalf("no database declared: %q %+v hits=%d dials=%d", res.Status, res.Failure, *hits, f.dials)
	}
}

// Outcomes through the whole path.
func TestDBChain_Outcomes(t *testing.T) {
	cases := []struct {
		name       string
		rows       chain.DBRows
		err        error
		status     string
		stepStatus string
		observed   string
	}{
		{"claim miss", chain.DBRows{Columns: []string{"status"}, Rows: [][]any{{"PENDING"}}}, nil, "failed", "failed", "did not satisfy the scenario's body assertion"},
		{"server refusal", chain.DBRows{}, &chain.DBServerError{SQLState: "42501", Message: "permission denied for table orders"}, "failed", "failed",
			"database query did not run (SQLSTATE 42501)"},
		{"connection lost", chain.DBRows{}, context.DeadlineExceeded, report.StatusError, report.StatusError, "the database did not answer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &argusFakeDB{rows: c.rows, err: c.err}
			withFakeDB(t, f)
			s, _, stop := dbChain(t, dbGoodStep, dbGoodExpect)
			defer stop()
			res := runChainScenario(dbConfig(), s, "tr-db-out", "testkit/ui")
			if res.Status != c.status || res.Steps[1].Status != c.stepStatus || !strings.Contains(res.Steps[1].Observed, c.observed) {
				t.Fatalf("want %s / %s / %q, got %q / %q / %q", c.status, c.stepStatus, c.observed, res.Status, res.Steps[1].Status, res.Steps[1].Observed)
			}
			b, _ := json.Marshal(res)
			if strings.Contains(string(b), "pw-SECRET-123") || strings.Contains(string(b), "SELECT status FROM") {
				t.Fatalf("the report never carries the password or the query text: %s", b)
			}
		})
	}
}

// The credential scrub reaches a connect error that quotes the password.
func TestDBChain_ConnectErrorIsScrubbedOfTheTargetsCredentials(t *testing.T) {
	prev := dialPostgres
	dialPostgres = func(context.Context, chain.DBConn) (chain.DBQuerier, error) {
		return nil, &leakyErr{}
	}
	t.Cleanup(func() { dialPostgres = prev })
	s, _, stop := dbChain(t, dbGoodStep, dbGoodExpect)
	defer stop()
	res := runChainScenario(dbConfig(), s, "tr-db-scrub", "testkit/ui")
	b, _ := json.Marshal(res)
	if res.Status != report.StatusError || strings.Contains(string(b), "pw-SECRET-123") {
		t.Fatalf("a connect error is `error` and carries no password: %q %s", res.Status, b)
	}
}

type leakyErr struct{}

func (*leakyErr) Error() string { return "auth failed for argus_ro with pw-SECRET-123" }

// ── POINT 12: the stand-in for an executor without this change ──────────────────────────────────────
//
// An executor that does not know the db step reaches the `default:` arm of runChainScenarioWithFaults' switch and
// refuses the chain by name BEFORE chain.Run: nothing has fired. This test pins that behaviour for ANY unknown
// type (the arm every old executor runs for a `db` step), at the head of this branch.
func TestRunChain_AnUnknownStepTypeIsRefusedBeforeAnEarlierStepFires(t *testing.T) {
	var hits int32
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer api.Close()
	trig := `{"steps":[{"type":"http","name":"mk","method":"GET","url":"` + api.URL + `/o"},{"type":"teleport","name":"x"}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step mk: status=200\n"))
	res := runChainScenario(&config.Config{}, s, "tr-unknown", "testkit/ui")
	if res.Status != "failed" || !strings.Contains(res.Failure.Observed, scenario.UnknownStepTypeProblem("teleport")) || atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("an unknown step type is refused by name before any step fires: %q %+v hits=%d", res.Status, res.Failure, hits)
	}
}

// ── POINT 10: the money pins ────────────────────────────────────────────────────────────────────────

func writeDBChainScenario(t *testing.T, scDir, id string) {
	t.Helper()
	d := filepath.Join(scDir, "chains")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	trig := `{"steps":[{"type":"db","name":"q","query":"SELECT status FROM orders WHERE id = $1","params":["1"]}]}`
	md := strings.Replace(chainRunMD("", trig, "### Runnable\n- step q: body has row_count >= 1\n"), "CHN-RT", id, 1)
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunAll_MoneyHandlingRefusesAChainWithADBStep_NoConnectionOpened(t *testing.T) {
	f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"status"}, Rows: [][]any{{"PAID"}}}}
	withFakeDB(t, f)
	for _, money := range []bool{true, false} {
		dir := t.TempDir()
		scDir := filepath.Join(dir, "scenarios")
		writeDBChainScenario(t, scDir, "MONEY-DB")
		cfg := dbConfig()
		cfg.MoneyHandling = money
		cfg.Project.Name = "p"
		before := f.dials
		rr, err := RunAll(cfg, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{})
		if err != nil {
			t.Fatalf("RunAll: %v", err)
		}
		got, _ := rr.Report.Find("MONEY-DB")
		if got == nil {
			t.Fatal("the scenario is missing from the report")
		}
		if money {
			if got.Status != report.StatusError || got.Failure == nil || !strings.HasPrefix(got.Failure.Observed, MoneyGuardRefusal) ||
				!strings.Contains(got.Failure.Observed, `chain step q is a "db" step`) {
				t.Errorf("money_handling: the db chain ran or was misreported: %q %+v", got.Status, got.Failure)
			}
			if f.dials != before || len(f.calls) != 0 {
				t.Errorf("money_handling: a connection was opened (%d new dials, %d queries)", f.dials-before, len(f.calls))
			}
		} else if got.Status != "passed" {
			t.Errorf("without money_handling the guard changes nothing, got %q %+v", got.Status, got.Failure)
		}
	}
}

package argus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/obsquery"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The logs step through the WHOLE executor path (claims, preflight, LogsStep, chain.Run), with an
// httptest Loki behind the real obsquery.Loki. No real Loki, no cluster.

type testLoki struct {
	srv     *httptest.Server
	queries []string
	hits    int32
	count   int
}

func newTestLoki(t *testing.T, lines int) *testLoki {
	t.Helper()
	l := &testLoki{count: lines}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&l.hits, 1)
		l.queries = append(l.queries, r.URL.Query().Get("query"))
		vals := make([]string, l.count)
		for i := range vals {
			vals[i] = fmt.Sprintf(`["%d","line-%d"]`, 1000+i, i)
		}
		fmt.Fprint(w, `{"status":"success","data":{"result":[{"values":[`+strings.Join(vals, ",")+`]}]}}`)
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *testLoki) reader() obsquery.LogsReader { return &obsquery.Loki{BaseURL: l.srv.URL} }

func (l *testLoki) requests() int { return int(atomic.LoadInt32(&l.hits)) }

// logsChain returns a chain: an http step that counts its requests, then the given logs step JSON.
func logsChain(t *testing.T, logsStep string, extraExpect string) (*scenario.Scenario, *int32, func()) {
	t.Helper()
	var hits int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"id":"o-1"}`))
	}))
	trig := `{"steps":[{"type":"http","name":"mk","method":"GET","url":"` + api.URL + `/o","save":{"oid":"id"}},` + logsStep + `]}`
	expect := "### Runnable\n- step mk: status=200\n" + extraExpect
	return scenario.Parse(chainRunMD("", trig, expect)), &hits, api.Close
}

func runLogsChain(c *config.Config, s *scenario.Scenario, reader obsquery.LogsReader) report.ScenarioResult {
	return runChainScenarioWithFaults(c, s, "tr-20261011T101530123-CHN-RT-abcdef12", "testkit/ui", nil, nil, nil, faultEnv{Logs: reader})
}

const logsGoodStep = `{"type":"logs","name":"l","scope":"correlation","contains":"order ${saved.oid}","settle":"0s"}`
const logsGoodExpect = "- step l: body has count >= 1\n"

func TestLogsChain_RunsEndToEnd_ContainsFromASavedValue(t *testing.T) {
	loki := newTestLoki(t, 2)
	s, hits, stop := logsChain(t, logsGoodStep, logsGoodExpect)
	defer stop()
	res := runLogsChain(&config.Config{}, s, loki.reader())
	if res.Status != "passed" {
		t.Fatalf("want passed: %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if *hits != 1 || loki.requests() != 1 {
		t.Fatalf("one http request then one Loki query: %d / %d", *hits, loki.requests())
	}
	want := `{service=~".+",service!~"` + obsquery.ObsInfraExclusion + `"} |= "tr-20261011T101530123-CHN-RT-abcdef12" |= "order o-1"`
	if loki.queries[0] != want {
		t.Fatalf("the query:\n got %s\nwant %s", loki.queries[0], want)
	}
	if res.Steps[1].Observed != "log query matched 2 line(s)" {
		t.Fatalf("observed %q", res.Steps[1].Observed)
	}
}

func TestLogsChain_TheRunsOwnIdIsFilledIntoContains_AndEnvIsNeverSplicedIn(t *testing.T) {
	t.Setenv("LOGS_ENVVAL", "from-the-environment")
	loki := newTestLoki(t, 1)
	s, _, stop := logsChain(t, `{"type":"logs","name":"l","scope":"correlation","contains":"id=${cid}","settle":"0s"}`, logsGoodExpect)
	defer stop()
	if res := runLogsChain(&config.Config{}, s, loki.reader()); res.Status != "passed" {
		t.Fatalf("%q %+v", res.Status, res.Failure)
	}
	if !strings.HasSuffix(loki.queries[0], `|= "id=tr-20261011T101530123-CHN-RT-abcdef12"`) {
		t.Fatalf("${cid} is the run's own id: %s", loki.queries[0])
	}
	// an environment value never reaches a selector: it is refused, by name, and nothing is sent
	loki2 := newTestLoki(t, 1)
	s2, hits, stop2 := logsChain(t, `{"type":"logs","name":"l","scope":"source","labels":{"app":"${LOGS_ENVVAL}"},"contains":"x","settle":"0s"}`, logsGoodExpect)
	defer stop2()
	res := runLogsChain(&config.Config{}, s2, loki2.reader())
	if res.Status != "failed" || !strings.Contains(res.Failure.Observed, "`labels` value of \"app\" carries `${…}`") || *hits != 0 || loki2.requests() != 0 {
		t.Fatalf("%q %+v hits=%d loki=%d", res.Status, res.Failure, *hits, loki2.requests())
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "from-the-environment") {
		t.Fatalf("the environment value is in the report: %s", b)
	}
}

// Every preflight refusal: the SAME sentence the authoring validator uses (asserted against Validate for the
// shared rules), the chain fails as `failed` with the preflight lead, an http step placed BEFORE the logs step has
// NOT fired, and Loki was not asked.
func TestLogsChain_PreflightRefusals_SameSentenceAsAuthoring_NothingFiredNothingQueried(t *testing.T) {
	cases := []struct {
		name     string
		step     string
		expect   string
		want     string
		authored bool // the authoring validator refuses it with the same sentence
	}{
		{"no scope", `{"type":"logs","name":"l","contains":"x"}`, logsGoodExpect, "this logs step declares no `scope`", true},
		{"unknown scope", `{"type":"logs","name":"l","scope":"all"}`, logsGoodExpect, `this logs step has scope "all"`, true},
		{"labels on correlation", `{"type":"logs","name":"l","scope":"correlation","labels":{"a":"b"}}`, logsGoodExpect, `has scope "correlation" and carries `, true},
		{"source without labels", `{"type":"logs","name":"l","scope":"source","contains":"x"}`, logsGoodExpect, `has scope "source" and no `, true},
		{"source without a filter", `{"type":"logs","name":"l","scope":"source","labels":{"a":"b"}}`, logsGoodExpect, "a step may not count a whole stream", true},
		{"bad label name", `{"type":"logs","name":"l","scope":"source","labels":{"a-b":"b"},"contains":"x"}`, logsGoodExpect, "`labels` name \"a-b\" is not a label name", true},
		{"empty label value", `{"type":"logs","name":"l","scope":"source","labels":{"a":""},"contains":"x"}`, logsGoodExpect, "`labels` value of \"a\" is empty", true},
		{"regex that does not compile", `{"type":"logs","name":"l","scope":"correlation","matching":"(x"}`, logsGoodExpect, "does not compile as an RE2 pattern", true},
		{"foreign placeholder in contains", `{"type":"logs","name":"l","scope":"correlation","contains":"${TOKEN}"}`, logsGoodExpect, "`contains` carries `${TOKEN}`", true},
		{"window over 1h", `{"type":"logs","name":"l","scope":"correlation","window":"2h"}`, logsGoodExpect, "more than the 1h0m0s bound on a logs step", true},
		{"settle over 60s", `{"type":"logs","name":"l","scope":"correlation","settle":"2m"}`, logsGoodExpect, "more than the 1m0s bound on a logs step", true},
		{"poll interval under 1s", `{"type":"logs","name":"l","scope":"correlation","poll":{"timeout":"10s","interval":"100ms"}}`, logsGoodExpect,
			"which is under 1s - a check must not hammer the log store", true},
		{"a stray field", `{"type":"logs","name":"l","scope":"correlation","url":"http://evil.invalid"}`, logsGoodExpect, `this logs step carries the field "url"`, true},
		{"a status claim", `{"type":"logs","name":"l","scope":"correlation"}`, "- step l: status=200\n", "is a status claim - a logs step has no HTTP status", true},
		{"no claim", `{"type":"logs","name":"l","scope":"correlation"}`, "", "needs at least one `body …` claim", false},
		{"an unknown step type after the logs step", `{"type":"splunk","name":"l"}`, logsGoodExpect, scenario.UnknownStepTypeProblem("splunk"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loki := newTestLoki(t, 1)
			s, hits, stop := logsChain(t, c.step, c.expect)
			defer stop()
			res := runLogsChain(&config.Config{}, s, loki.reader())
			if res.Status != "failed" || res.Failure == nil || !strings.HasSuffix(res.Failure.Observed, "— preflight") {
				t.Fatalf("want a preflight failure, got %q %+v", res.Status, res.Failure)
			}
			if !strings.Contains(res.Failure.Observed, c.want) {
				t.Fatalf("want the sentence %q in %q", c.want, res.Failure.Observed)
			}
			if *hits != 0 {
				t.Fatalf("the http step placed BEFORE the refused step must not have fired, the server counted %d request(s)", *hits)
			}
			if loki.requests() != 0 {
				t.Fatalf("Loki must not be asked for a refused chain: %d request(s)", loki.requests())
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

// POINT 4: the two environment refusals, before any step of the chain runs.
func TestLogsChain_NoLokiAndBetterStackAreRefusedByName_NothingFired(t *testing.T) {
	bs := &config.Config{}
	bs.Observability.BetterStack.QueryURL = "https://betterstack-connect.example.com"
	bs.Observability.BetterStack.TeamID = "1"
	if !bs.UseBetterStack() {
		t.Fatal("setup: the config must select BetterStack")
	}
	cases := []struct {
		name   string
		c      *config.Config
		reader obsquery.LogsReader
		want   string
	}{
		{"--obs none / an empty --loki (no reader)", &config.Config{}, nil,
			"a logs step reads the Loki this executor is configured with, and this executor has none (onboarded with --obs none, or --loki is empty)"},
		{"the config selects BetterStack", bs, &obsquery.Loki{BaseURL: "http://unused.invalid"},
			"a logs step reads Loki only for now, and this instance's argus-config selects BetterStack"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, hits, stop := logsChain(t, logsGoodStep, logsGoodExpect)
			defer stop()
			res := runLogsChain(c.c, s, c.reader)
			if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, c.want) || !strings.HasSuffix(res.Failure.Observed, "— preflight") {
				t.Fatalf("want %q, got %q %+v", c.want, res.Status, res.Failure)
			}
			if *hits != 0 || len(res.Steps) != 0 {
				t.Fatalf("nothing may have fired: http %d, steps %+v", *hits, res.Steps)
			}
		})
	}
}

// POINT 5 through the whole path: an unreachable store is `error`, never a pass of count == 0.
func TestLogsChain_Outcomes(t *testing.T) {
	down := newTestLoki(t, 0)
	downReader := down.reader()
	down.srv.Close()
	cases := []struct {
		name       string
		reader     obsquery.LogsReader
		expect     string
		status     string
		stepStatus string
		observed   string
	}{
		{"claims hold", newTestLoki(t, 3).reader(), logsGoodExpect, "passed", "passed", "log query matched 3 line(s)"},
		{"claim miss", newTestLoki(t, 0).reader(), logsGoodExpect, "failed", "failed", "did not satisfy the scenario's body assertion"},
		{"absence holds", newTestLoki(t, 0).reader(), "- step l: body has count == 0\n", "passed", "passed", "log query matched 0 line(s)"},
		{"store down", downReader, "- step l: body has count == 0\n", report.StatusError, report.StatusError, "the log store did not answer"},
		{"lossy", newTestLoki(t, 2000).reader(), logsGoodExpect, report.StatusError, report.StatusError, "narrow `contains` / `matching` or `window`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, stop := logsChain(t, logsGoodStep, c.expect)
			defer stop()
			res := runLogsChain(&config.Config{}, s, c.reader)
			if res.Status != c.status || res.Steps[1].Status != c.stepStatus || !strings.Contains(res.Steps[1].Observed, c.observed) {
				t.Fatalf("want %s / %s / %q, got %q / %q / %q", c.status, c.stepStatus, c.observed, res.Status, res.Steps[1].Status, res.Steps[1].Observed)
			}
		})
	}
}

// ── POINT 7: the money pins ────────────────────────────────────────────────────────────────────────

func writeLogsChainScenario(t *testing.T, scDir, id string) {
	t.Helper()
	d := filepath.Join(scDir, "chains")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	trig := `{"steps":[{"type":"logs","name":"l","scope":"correlation","settle":"0s"}]}`
	md := strings.Replace(chainRunMD("", trig, "### Runnable\n- step l: body has count == 0\n"), "CHN-RT", id, 1)
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunAll_MoneyHandlingRefusesAChainWithALogsStep_NoQuerySent(t *testing.T) {
	for _, money := range []bool{true, false} {
		loki := newTestLoki(t, 0)
		dir := t.TempDir()
		scDir := filepath.Join(dir, "scenarios")
		writeLogsChainScenario(t, scDir, "MONEY-LOGS")
		cfg := &config.Config{MoneyHandling: money}
		cfg.Project.Name = "p"
		rr, err := RunAllWithLogs(cfg, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{}, nil, "", "", nil, loki.reader())
		if err != nil {
			t.Fatalf("RunAllWithLogs: %v", err)
		}
		got, _ := rr.Report.Find("MONEY-LOGS")
		if got == nil {
			t.Fatal("the scenario is missing from the report")
		}
		if money {
			if got.Status != report.StatusError || got.Failure == nil || !strings.HasPrefix(got.Failure.Observed, MoneyGuardRefusal) ||
				!strings.Contains(got.Failure.Observed, `chain step l is a "logs" step`) || !strings.Contains(got.Failure.Observed, "only GET http steps may run") {
				t.Errorf("money_handling: the logs chain ran or was misreported: %q %+v", got.Status, got.Failure)
			}
			if loki.requests() != 0 {
				t.Errorf("money_handling: Loki was queried %d time(s)", loki.requests())
			}
		} else if got.Status != "passed" || loki.requests() != 1 {
			t.Errorf("without money_handling the guard changes nothing, got %q %+v (%d queries)", got.Status, got.Failure, loki.requests())
		}
	}
}

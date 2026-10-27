package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// The db step's RUNNER, exercised with a fake DBQuerier. No database, no connection of any kind.

type fakeResult struct {
	rows DBRows
	err  error
}

type fakeCall struct {
	query   string
	params  []string
	timeout time.Duration
	limit   int
}

// fakeDB answers Query from results in order (the last one repeats) and honours the limit exactly like a real
// querier: it hands back at most limit rows and nothing else, so a step that asks for max_rows instead of
// max_rows+1 cannot tell a capped answer from a complete one - which is the mutation the cap test catches.
type fakeDB struct {
	mu       sync.Mutex
	results  []fakeResult
	calls    []fakeCall
	dialErr  error
	dials    int
	closes   int
	dialedAs DBConn
}

func (f *fakeDB) dial(_ context.Context, c DBConn) (DBQuerier, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	f.dialedAs = c
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return f, nil
}

func (f *fakeDB) Query(_ context.Context, query string, params []string, st time.Duration, limit int) (DBRows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{query: query, params: append([]string(nil), params...), timeout: st, limit: limit})
	i := len(f.calls) - 1
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	r := f.results[i]
	if r.err != nil {
		return DBRows{}, r.err
	}
	out := DBRows{Columns: r.rows.Columns, Rows: r.rows.Rows}
	if limit > 0 && len(out.Rows) > limit {
		out.Rows = out.Rows[:limit]
	}
	return out, nil
}

func (f *fakeDB) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func rowsOf(cols []string, rows ...[]any) DBRows { return DBRows{Columns: cols, Rows: rows} }

func claims(t *testing.T, bullets ...string) []mcp.BodyAssert {
	t.Helper()
	a, err := scenario.DBStepClaims(bullets)
	if err != nil {
		t.Fatalf("claims %q: %v", bullets, err)
	}
	return a
}

const dbTestQuery = "SELECT status FROM orders WHERE id = $1"

func runDB(t *testing.T, f *fakeDB, spec DBSpec, want []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, always bool) report.StepResult {
	t.Helper()
	if spec.Query == "" {
		spec.Query = dbTestQuery
	}
	st := DBStep("q", spec, want, save, poll, always, f.dial)
	return st.Run("tr-1", map[string]string{})
}

func TestDBStep_PassesAndReportsTheRowCount(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"status"}, []any{"PAID"})}}}
	res := runDB(t, f, DBSpec{Params: []string{"o-1"}, MaxRows: 100},
		claims(t, "body has row_count == 1", "body has rows.0.status equals PAID", "body every rows has status not equals FAILED"), nil, nil, false)
	if res.Status != "passed" || res.Observed != "db query returned 1 row(s)" {
		t.Fatalf("want passed / `db query returned 1 row(s)`, got %q / %q", res.Status, res.Observed)
	}
	if len(res.AssertionsEnforced) != 3 {
		t.Fatalf("the claims judged are listed, got %v", res.AssertionsEnforced)
	}
	if f.dials != 1 || f.closes != 1 {
		t.Fatalf("one connection per step, closed when it returns: dials %d closes %d", f.dials, f.closes)
	}
}

func TestDBStep_ClaimMissFailsWithTheClaimsAuthorOnlyAndNoQueryOrValueInObserved(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"status"}, []any{"PENDING"}, []any{"PENDING"})}}}
	res := runDB(t, f, DBSpec{Params: []string{"o-1"}},
		claims(t, "body has row_count == 1", "body has rows.0.status equals PAID"), nil, nil, false)
	if res.Status != "failed" {
		t.Fatalf("a claim that does not hold is `failed`, got %q (%s)", res.Status, res.Observed)
	}
	if !strings.Contains(res.Observed, "db query returned 2 row(s)") || !strings.Contains(res.Observed, chainClaimsNote) {
		t.Fatalf("Observed carries the row count and the held-out note, got %q", res.Observed)
	}
	for _, leak := range []string{"PAID", "PENDING", "orders", "SELECT", "o-1"} {
		if strings.Contains(res.Observed, leak) {
			t.Fatalf("Observed (builder-visible) must never carry %q: %q", leak, res.Observed)
		}
	}
	if len(res.FailedClaims) != 2 {
		t.Fatalf("both claims missed and are listed (author-only), got %+v", res.FailedClaims)
	}
	if !strings.Contains(res.FailedClaims[1].Observed, "PENDING") {
		t.Fatalf("the author-only record carries what the rows showed, got %+v", res.FailedClaims[1])
	}
}

func TestDBStep_RowCap_AtTheCapPasses_OneOverIsAnErrorNamingTheCap(t *testing.T) {
	mk := func(n int) *fakeDB {
		var rows [][]any
		for i := 0; i < n; i++ {
			rows = append(rows, []any{json.Number(fmt.Sprint(i))})
		}
		return &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"n"}, rows...)}}}
	}
	want := claims(t, "body has row_count >= 1")

	at := mk(3)
	if res := runDB(t, at, DBSpec{MaxRows: 3}, want, nil, nil, false); res.Status != "passed" {
		t.Fatalf("exactly max_rows rows is within the cap, got %q %q", res.Status, res.Observed)
	}
	if at.calls[0].limit != 4 {
		t.Fatalf("the step reads max_rows + 1 rows (that extra row is how it knows the cap was hit); it asked for %d", at.calls[0].limit)
	}

	over := mk(4)
	res := runDB(t, over, DBSpec{MaxRows: 3}, want, nil, nil, false)
	if res.Status != report.StatusError {
		t.Fatalf("one row over the cap is `error` (never a silent wrong answer), got %q %q", res.Status, res.Observed)
	}
	for _, s := range []string{"more than max_rows (3) rows", "add a LIMIT", "select count(*)"} {
		if !strings.Contains(res.Observed, s) {
			t.Fatalf("the sentence must say %q, got %q", s, res.Observed)
		}
	}
}

func TestDBStep_RowCapIsNotRetriedByAPoll(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"n"}, []any{"a"}, []any{"b"})}}}
	res := runDB(t, f, DBSpec{MaxRows: 1}, claims(t, "body has row_count >= 1"), nil, &Poll{Timeout: time.Second, Interval: 5 * time.Millisecond}, false)
	if res.Status != report.StatusError || len(f.calls) != 1 {
		t.Fatalf("an over-cap answer is an error, once: %q after %d call(s)", res.Status, len(f.calls))
	}
}

func TestDBStep_DocumentCap(t *testing.T) {
	big := strings.Repeat("x", scenario.DBDocumentMaxBytes)
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"blob"}, []any{big})}}}
	res := runDB(t, f, DBSpec{MaxRows: 10}, claims(t, "body has row_count >= 1"), nil, nil, false)
	if res.Status != report.StatusError || !strings.Contains(res.Observed, "1048576-byte limit") {
		t.Fatalf("a document over 1 MiB is an error naming the limit, got %q %q", res.Status, res.Observed)
	}
	small := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"blob"}, []any{strings.Repeat("x", 1000)})}}}
	if res := runDB(t, small, DBSpec{MaxRows: 10}, claims(t, "body has row_count >= 1"), nil, nil, false); res.Status != "passed" {
		t.Fatalf("a small document passes, got %q %q", res.Status, res.Observed)
	}
}

func TestDBStep_DuplicateColumnIsAnErrorNamingIt(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"id", "name", "id"}, []any{"1", "a", "2"})}}}
	res := runDB(t, f, DBSpec{}, claims(t, "body has row_count >= 1"), nil, nil, false)
	if res.Status != report.StatusError || !strings.Contains(res.Observed, `two columns named "id"`) || !strings.Contains(res.Observed, "alias") {
		t.Fatalf("a duplicate column name is an error naming the column, got %q %q", res.Status, res.Observed)
	}
}

func TestDBStep_ConnectErrorIsAnErrorScrubbedAndNeverRetried(t *testing.T) {
	f := &fakeDB{dialErr: errors.New("password authentication failed for user app: s3cret-pw at db.internal"), results: []fakeResult{{}}}
	scrub := func(s string) string { return strings.ReplaceAll(s, "s3cret-pw", "[redacted]") }
	res := runDB(t, f, DBSpec{Scrub: scrub}, claims(t, "body has row_count >= 1"), nil, &Poll{Timeout: time.Second, Interval: time.Millisecond}, false)
	if res.Status != report.StatusError || !strings.HasPrefix(res.Observed, "cannot connect to the database: ") {
		t.Fatalf("cannot connect is `error`, got %q %q", res.Status, res.Observed)
	}
	if strings.Contains(res.Observed, "s3cret-pw") || !strings.Contains(res.Observed, "[redacted]") {
		t.Fatalf("the message is scrubbed of the credential, got %q", res.Observed)
	}
	if f.dials != 1 || len(f.calls) != 0 {
		t.Fatalf("a poll does not retry a connection error: dials %d, queries %d", f.dials, len(f.calls))
	}
}

func TestDBStep_ConnectionLostAndDeadlineAreErrors(t *testing.T) {
	for name, qerr := range map[string]error{
		"connection lost": errors.New("the connection to the database failed"),
		"deadline":        context.DeadlineExceeded,
	} {
		f := &fakeDB{results: []fakeResult{{err: qerr}}}
		res := runDB(t, f, DBSpec{}, claims(t, "body has row_count >= 1"), nil, nil, false)
		if res.Status != report.StatusError || !strings.HasPrefix(res.Observed, "the database did not answer: ") {
			t.Errorf("%s: want error / `the database did not answer`, got %q %q", name, res.Status, res.Observed)
		}
	}
}

func TestDBStep_ServerRefusalIsFailedNeverNoRows_AndIsNotRetriedByAPoll(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{err: &DBServerError{SQLState: "42P01", Message: `relation "orderz" does not exist`}}}}
	res := runDB(t, f, DBSpec{}, claims(t, "body has row_count == 0"), nil, &Poll{Timeout: time.Second, Interval: time.Millisecond}, false)
	if res.Status != "failed" || res.Observed != `database query did not run (SQLSTATE 42P01)` {
		t.Fatalf("a server refusal is `failed`, said as what it is, with the SQLSTATE only; got %q %q", res.Status, res.Observed)
	}
	if strings.Contains(res.Observed, "orderz") || strings.Contains(res.Observed, "does not exist") {
		t.Fatalf("the server's message is never in the builder-visible Observed: %q", res.Observed)
	}
	if res.BodyExcerpt != `relation "orderz" does not exist` {
		t.Fatalf("the server's message goes to the author in BodyExcerpt, got %q", res.BodyExcerpt)
	}
	if len(f.calls) != 1 {
		t.Fatalf("a syntax error does not heal: the poll must not retry it, got %d calls", len(f.calls))
	}
	if strings.Contains(res.Observed, "no rows") {
		t.Fatalf("a refused query is never reported as no rows: %q", res.Observed)
	}
}

func TestDBStep_ServerRefusalMessageIsScrubbedMaskedAndCut(t *testing.T) {
	long := `invalid input syntax for type uuid: "SAVED-VALUE-123" cred=CRED-9 ` + strings.Repeat("é", report.MaxBodyExcerpt)
	f := &fakeDB{results: []fakeResult{{err: &DBServerError{SQLState: "22P02", Message: long}}}}
	scrub := func(s string) string { return strings.ReplaceAll(s, "CRED-9", "[cred]") }
	st := DBStep("q", DBSpec{Query: dbTestQuery, Scrub: scrub}, claims(t, "body has row_count >= 1"), nil, nil, false, f.dial)
	res := st.Run("tr-1", map[string]string{"tok": "SAVED-VALUE-123"})
	if res.Status != "failed" || strings.Contains(res.Observed, "SAVED-VALUE-123") {
		t.Fatalf("failed, and Observed carries no server words: %q %q", res.Status, res.Observed)
	}
	if strings.Contains(res.BodyExcerpt, "SAVED-VALUE-123") || strings.Contains(res.BodyExcerpt, "CRED-9") {
		t.Fatalf("a secret reached the excerpt: %q", res.BodyExcerpt)
	}
	if !strings.Contains(res.BodyExcerpt, "${saved.tok}") || !strings.Contains(res.BodyExcerpt, "[cred]") {
		t.Fatalf("positive control: both redactions should be visible: %q", res.BodyExcerpt)
	}
	if !strings.HasSuffix(res.BodyExcerpt, report.FailedClaimTruncatedSuffix) {
		t.Fatalf("a long message is cut and marked: %q", res.BodyExcerpt)
	}
	text := strings.TrimSuffix(res.BodyExcerpt, report.FailedClaimTruncatedSuffix)
	if len(text) > report.MaxBodyExcerpt || strings.Contains(text, "�") {
		t.Fatalf("cut must be at most %d bytes of whole characters, got %d", report.MaxBodyExcerpt, len(text))
	}
}

// The contract on DBQuerier: a read that returned `limit` rows leaves the connection spent. The step must never
// Query on it again - not on the next attempt of a poll either - and must close it once.
func TestDBStep_NeverQueriesAConnectionAgainAfterAnOverCapRead(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"n"}, []any{"a"}, []any{"b"})}}}
	res := runDB(t, f, DBSpec{MaxRows: 1}, claims(t, "body has row_count >= 1"), nil,
		&Poll{Timeout: time.Second, Interval: 5 * time.Millisecond}, false)
	if res.Status != report.StatusError {
		t.Fatalf("an over-cap read is an error, got %q %q", res.Status, res.Observed)
	}
	if len(f.calls) != 1 {
		t.Fatalf("exactly one Query on the spent connection, got %d", len(f.calls))
	}
	if f.closes != 1 {
		t.Fatalf("the connection is closed once, got %d", f.closes)
	}
}

func TestDBStep_PollPassesOnTheThirdAttempt(t *testing.T) {
	miss := fakeResult{rows: rowsOf([]string{"status"}, []any{"PENDING"})}
	hit := fakeResult{rows: rowsOf([]string{"status"}, []any{"PAID"})}
	f := &fakeDB{results: []fakeResult{miss, miss, hit}}
	res := runDB(t, f, DBSpec{Params: []string{"o-1"}}, claims(t, "body has rows.0.status equals PAID"), nil,
		&Poll{Timeout: 5 * time.Second, Interval: 5 * time.Millisecond}, false)
	if res.Status != "passed" || len(f.calls) != 3 {
		t.Fatalf("want passed on attempt 3, got %q after %d call(s): %s", res.Status, len(f.calls), res.Observed)
	}
	if f.dials != 1 || f.closes != 1 {
		t.Fatalf("the poll reuses the step's one connection: dials %d closes %d", f.dials, f.closes)
	}
}

func TestDBStep_PollTimesOutNamingTheAttemptsAndKeepsTheLastFailedClaims(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"status"}, []any{"PENDING"})}}}
	res := runDB(t, f, DBSpec{}, claims(t, "body has rows.0.status equals PAID"), nil,
		&Poll{Timeout: 60 * time.Millisecond, Interval: 20 * time.Millisecond}, false)
	if res.Status != "failed" || !strings.Contains(res.Observed, "poll timed out after 60ms (") || !strings.Contains(res.Observed, "attempt(s)); last observed: ") {
		t.Fatalf("want a poll timeout naming the attempts, got %q %q", res.Status, res.Observed)
	}
	if len(f.calls) < 2 || len(res.FailedClaims) != 1 {
		t.Fatalf("it re-ran the query and kept the last attempt's failed claims: %d calls, %+v", len(f.calls), res.FailedClaims)
	}
}

func TestDBStep_AlwaysSkipsTheNeedsGate_AndAnOrdinaryStepIsNotMeasured(t *testing.T) {
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"n"}, []any{"x"})}}}
	spec := DBSpec{Query: dbTestQuery, Params: []string{"${saved.oid}"}}
	ordinary := DBStep("q", spec, claims(t, "body has row_count >= 1"), nil, nil, false, f.dial)
	if err := ordinary.Needs(map[string]string{}); err == nil || !strings.Contains(err.Error(), `"oid"`) {
		t.Fatalf("an unsaved param variable makes an ordinary step not-measured, naming it; got %v", err)
	}
	cleanup := DBStep("q", spec, claims(t, "body has row_count >= 1"), nil, nil, true, f.dial)
	if !cleanup.Always || cleanup.Needs(map[string]string{}) != nil {
		t.Fatal("an `always` step is exempt from the gate")
	}
	res := cleanup.Run("tr-1", map[string]string{})
	if res.Status != "failed" || !strings.Contains(res.Observed, `"oid"`) || len(f.calls) != 0 {
		t.Fatalf("the cleanup attempt fails cleanly naming the variable and sends nothing: %q %q (%d calls)", res.Status, res.Observed, len(f.calls))
	}
}

// ── the typing table ────────────────────────────────────────────────────────────────────────────────

func TestDBStep_ValueTyping_EveryDigitSurvivesIntoAClaimAndASave(t *testing.T) {
	ts := time.Date(2026, 10, 11, 14, 30, 0, 0, time.FixedZone("x", 2*3600))
	f := &fakeDB{results: []fakeResult{{rows: rowsOf(
		[]string{"big", "price", "ratio", "ok", "gone", "note", "at", "doc", "weird"},
		[]any{json.Number("12345678901234567890"), json.Number("0.10"), 0.5, true, nil, "a<b & \"c\"", ts,
			json.RawMessage(`{ "k": [1, 2.50] }`), json.Number("NaN")})}}}
	save := map[string]scenario.SaveSpec{
		"bigjson": {Path: "rows.0.big", JSON: true},
		"price":   {Path: "rows.0.price", JSON: true},
		"doc":     {Path: "rows.0.doc", JSON: true},
		"note":    {Path: "rows.0.note"},
	}
	res := runDB(t, f, DBSpec{}, claims(t,
		"body has rows.0.big == 12345678901234567890", // exact: a float64 would read ...567000
		"body has rows.0.price == 0.1",
		"body contains 0.10",
		"body contains 12345678901234567890",
		"body has rows.0.ok equals true",
		"body has no rows.0.gone",
		"body has rows.0.at equals 2026-10-11T12:30:00Z", // timestamptz as RFC 3339 in UTC
		"body has rows.0.doc.k.1 == 2.5",
		"body has rows.0.weird equals NaN", // not a JSON number: kept as text
		"body has rows.0.note equals a<b & \"c\"",
	), save, nil, false)
	if res.Status != "passed" {
		t.Fatalf("every typing claim holds: %q %q %+v", res.Status, res.Observed, res.FailedClaims)
	}
	for k, want := range map[string]string{"bigjson": "12345678901234567890", "price": "0.10", "doc": `{"k":[1,2.50]}`, "note": `a<b & "c"`} {
		if res.Captured[k] != want {
			t.Errorf("saved %s = %q, want %q", k, res.Captured[k], want)
		}
	}
}

func TestDBStep_DocumentShape(t *testing.T) {
	doc, err := dbDocument(rowsOf([]string{"b", "a"}, []any{json.Number("1"), "x"}, []any{nil, "y"}))
	if err != nil || string(doc) != `{"row_count":2,"rows":[{"b":1,"a":"x"},{"b":null,"a":"y"}]}` {
		t.Fatalf("columns keep the server's order, got %s / %v", doc, err)
	}
	empty, _ := dbDocument(DBRows{Columns: []string{"a"}})
	if string(empty) != `{"row_count":0,"rows":[]}` {
		t.Fatalf("no rows is row_count 0 and an empty array, got %s", empty)
	}
}

// ── a saved value reaches the server as a PARAMETER, never in the text ──────────────────────────────

func TestDBStep_SavedValueIsAParameterAndTheQueryTextIsByteIdentical(t *testing.T) {
	const query = "SELECT status FROM orders WHERE id = $1 AND note = $2 -- untouched ${correlation_id}"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"o\"1'; DROP TABLE orders; --"}`)
	}))
	defer api.Close()
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"status"}, []any{"PAID"})}}}
	mk := HTTPStep("mk", "POST", api.URL+"/o", map[string]string{}, "", 0, nil,
		map[string]scenario.SaveSpec{"oid": {Path: "id"}}, nil, false, nil, nil)
	db := DBStep("check", DBSpec{Query: query, Params: []string{"${saved.oid}", "fixed-${saved.oid}"}, MaxRows: 5},
		claims(t, "body has rows.0.status equals PAID"), nil, nil, false, f.dial)
	res := Run("tr-1", []Step{mk, db})
	if res.Status != "passed" {
		t.Fatalf("chain: %q %+v", res.Status, res.Steps)
	}
	if len(f.calls) != 1 {
		t.Fatalf("one query, got %d", len(f.calls))
	}
	c := f.calls[0]
	if c.query != query {
		t.Fatalf("the query text must reach the querier byte-identical to the scenario's:\n got %q\nwant %q", c.query, query)
	}
	wantParam := `o"1'; DROP TABLE orders; --`
	if len(c.params) != 2 || c.params[0] != wantParam || c.params[1] != "fixed-"+wantParam {
		t.Fatalf("the saved value goes as a parameter, exactly as the system returned it (no JSON escaping); got %q", c.params)
	}
	if strings.Contains(c.query, "DROP") {
		t.Fatal("a saved value reached the SQL text")
	}
}

func TestDBStep_SaveFeedsALaterHTTPStep_BothForms(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotBody = r.URL.Path, string(b)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer api.Close()
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"id", "tags"}, []any{"ord-7", json.RawMessage(`["a","b"]`)})}}}
	db := DBStep("lookup", DBSpec{Query: dbTestQuery, Params: []string{"1"}}, claims(t, "body has row_count == 1"),
		map[string]scenario.SaveSpec{"oid": {Path: "rows.0.id"}, "tags": {Path: "rows.0.tags", JSON: true}}, nil, false, f.dial)
	use := HTTPStep("use", "POST", api.URL+"/orders/${saved.oid}", map[string]string{}, `{"tags":"${saved.tags}"}`, 200, nil, nil, nil, false, nil, nil)
	res := Run("tr-1", []Step{db, use})
	if res.Status != "passed" {
		t.Fatalf("chain: %q %+v", res.Status, res.Steps)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/orders/ord-7" || gotBody != `{"tags":["a","b"]}` {
		t.Fatalf("the saved text went into the url and the saved JSON into the body, got %q %q", gotPath, gotBody)
	}
}

// ── the chain keeps a db step's own error sentence ──────────────────────────────────────────────────

func TestDBStep_ErrorKeepsItsOwnSentenceInTheChain_AndTheLaterStepsAreNotMeasured(t *testing.T) {
	var fired int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fired++ }))
	defer api.Close()
	f := &fakeDB{results: []fakeResult{{rows: rowsOf([]string{"n"}, []any{"a"}, []any{"b"})}}}
	db := DBStep("lookup", DBSpec{Query: dbTestQuery, Params: []string{"1"}, MaxRows: 1}, claims(t, "body has row_count >= 1"), nil, nil, false, f.dial)
	after := HTTPStep("after", "GET", api.URL, map[string]string{}, "", 200, nil, nil, nil, false, nil, nil)
	res := Run("tr-1", []Step{db, after})
	if res.Status != report.StatusError || fired != 0 {
		t.Fatalf("the chain ends in error and the later step is not fired: %q, fired %d", res.Status, fired)
	}
	if res.Steps[0].Status != report.StatusError || !strings.Contains(res.Steps[0].Observed, "more than max_rows (1) rows") {
		t.Fatalf("the db step keeps its own sentence (not `SUT unreachable`), got %+v", res.Steps[0])
	}
	if res.Steps[1].Status != report.StepNotMeasured || !strings.Contains(res.Steps[1].Observed, `step "lookup" ended with an error`) {
		t.Fatalf("the later step is told which step ended the chain, got %+v", res.Steps[1])
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "more than max_rows (1) rows") {
		t.Fatalf("the failure names the sentence, got %+v", res.Failure)
	}
}

// ── ParsePostgresJDBC ───────────────────────────────────────────────────────────────────────────────

func TestParsePostgresJDBC(t *testing.T) {
	c, err := ParsePostgresJDBC("jdbc:postgresql://db.internal:6543/orders?sslmode=require&currentSchema=app,public&ApplicationName=argus", "ro", "pw")
	if err != nil {
		t.Fatal(err)
	}
	want := DBConn{Host: "db.internal", Port: 6543, Database: "orders", User: "ro", Password: "pw", SSLMode: "require", Schema: "app,public", AppName: "argus"}
	if c != want {
		t.Fatalf("got %+v want %+v", c, want)
	}
	d, err := ParsePostgresJDBC("jdbc:postgresql://db/orders", "ro", "")
	if err != nil || d.Port != 5432 || d.SSLMode != "" {
		t.Fatalf("defaults: %+v %v", d, err)
	}
	if s, _ := ParsePostgresJDBC("jdbc:postgresql://db/orders?ssl=true", "ro", ""); s.SSLMode != "verify-full" {
		t.Fatalf("ssl=true is verify-full, got %q", s.SSLMode)
	}
	refuse := []struct{ name, url, user, want string }{
		{"another dialect", "jdbc:oracle:thin:@h:1521:sid", "u", "a db step reads PostgreSQL targets only"},
		{"mysql", "jdbc:mysql://h/db", "u", `the "mysql" driver; ` + "`## VERIFY` on a plain scenario covers the JDBC dialects"},
		{"not jdbc", "postgres://h/db", "u", "is not a jdbc: URL"},
		{"empty", "", "u", "declares no jdbc_url"},
		{"userinfo", "jdbc:postgresql://u:pw@h/db", "u", "carries credentials in its authority"},
		{"credential parameter", "jdbc:postgresql://h/db?password=hunter2", "u", `the parameter "password"`},
		{"unmapped parameter", "jdbc:postgresql://h/db?connectTimeout=5", "u", `the parameter "connectTimeout", which a db step does not map`},
		{"another unmapped parameter", "jdbc:postgresql://h/db?targetServerType=primary", "u", `the parameter "targetServerType"`},
		{"no database", "jdbc:postgresql://h/", "u", "names no single database"},
		{"no username", "jdbc:postgresql://h/db", "", "declares no username"},
		{"bad sslmode", "jdbc:postgresql://h/db?sslmode=maybe", "u", "sslmode to a value outside"},
		{"ssl and sslmode disagree", "jdbc:postgresql://h/db?ssl=false&sslmode=require", "u", "disagree"},
		{"schema injection", "jdbc:postgresql://h/db?currentSchema=a%3Bdrop", "u", "currentSchema to something other than"},
		{"raw semicolon in the query", "jdbc:postgresql://h/db?currentSchema=a;drop", "u", "query string that does not parse"},
		{"bad port", "jdbc:postgresql://h:99999/db", "u", "port outside 1..65535"},
	}
	for _, c := range refuse {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParsePostgresJDBC(c.url, c.user, "pw")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, err)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "u:pw") {
				t.Fatalf("a refusal never echoes a credential: %q", err)
			}
		})
	}
}

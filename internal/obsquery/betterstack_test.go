package obsquery

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// AC-D13: BetterStack is the second Backend implementation, queried over BetterStack's HTTP SQL
// API instead of Loki's LogQL. These tests run against an httptest fake standing in for that API.

// bsFakeServer records every request it receives and replies with a canned per-source row set,
// keyed by which table the SQL body names (remote(t<team>_<source>_logs)) — the same server
// backs every configured source, so the SQL text alone tells the fake which canned rows to hand
// back.
type bsFakeServer struct {
	t       *testing.T
	mu      []recordedReq
	rowsFor map[string][]bsRow // keyed by source slug
}

type recordedReq struct {
	sql    string
	params url.Values
}

func newBSFakeServer(t *testing.T, rowsFor map[string][]bsRow) (*bsFakeServer, *httptest.Server) {
	f := &bsFakeServer{t: t, rowsFor: rowsFor}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		f.mu = append(f.mu, recordedReq{sql: string(body), params: r.URL.Query()})

		var rows []bsRow
		for slug, rs := range f.rowsFor {
			if strings.Contains(string(body), "_"+slug+"_logs") {
				rows = rs
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(rows); err != nil {
			http.Error(w, err.Error(), 500)
		}
	}))
	return f, ts
}

func mustFindReq(t *testing.T, f *bsFakeServer) recordedReq {
	t.Helper()
	if len(f.mu) == 0 {
		t.Fatal("fake server received no requests")
	}
	return f.mu[0]
}

// TestBetterStack_CorrelationID_IsParameterNeverConcatenated is the injection test (REQUIRED):
// the correlation id must arrive ONLY as a query PARAMETER (param_correlation_id=...), and must
// NEVER appear literally inside the posted SQL text. A dangerous-looking id is used deliberately
// so a concatenation bug would both leak into the SQL body AND be visible to this assertion.
func TestBetterStack_CorrelationID_IsParameterNeverConcatenated(t *testing.T) {
	const corrID = `tr-x' OR '1'='1`
	f, ts := newBSFakeServer(t, map[string][]bsRow{
		"accounting": {{DT: "2026-06-22 10:00:00", Raw: `{"correlation_id":"tr-x' OR '1'='1","level":"info","msg":"hi"}`}},
	})
	defer ts.Close()

	b := &BetterStack{QueryURL: ts.URL, TeamID: "123456", Sources: map[string]string{"accounting": "accounting-service"}}
	// Logs()/Sagas() now refuse a non-canonical id before any query (corrid_test.go),
	// so this fixture can no longer arrive through them. The parameter-not-concatenation property is
	// the SECOND lock and is tested where it lives: the layer that attaches the id to the request.
	_, _ = b.queryAllSources(corrID, "30m", time.Time{}, false)

	req := mustFindReq(t, f)
	if strings.Contains(req.sql, corrID) {
		t.Fatalf("REJECTED: correlation id was concatenated into the SQL text, never a parameter — sql=%q", req.sql)
	}
	got := req.params.Get("param_correlation_id")
	if got != corrID {
		t.Fatalf("correlation id must be carried as the param_correlation_id query parameter, got %q (params=%v)", got, req.params)
	}
	// the SQL text must reference it only via the {correlation_id:String} placeholder.
	if !strings.Contains(req.sql, "{correlation_id:String}") {
		t.Fatalf("sql must reference the correlation id via the {correlation_id:String} placeholder, got: %s", req.sql)
	}
}

// TestBetterStack_BuildQuery_SQLShape asserts the SQL text's overall shape: a remote(<table>)
// source, a window predicate using the {start:DateTime64}/{end:DateTime64} placeholders (never
// literal timestamps baked into the WHERE clause), and a row-cap LIMIT using {row_cap:UInt32}.
func TestBetterStack_BuildQuery_SQLShape(t *testing.T) {
	b := &BetterStack{TeamID: "123456"}
	start := time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	q := b.buildQuery("t123456_accounting_logs", start, end, false)

	for _, want := range []string{
		"FROM remote(t123456_accounting_logs)",
		"{start:DateTime64}", "{end:DateTime64}",
		"LIMIT {row_cap:UInt32}",
	} {
		if !strings.Contains(q.sql, want) {
			t.Errorf("sql missing %q: %s", want, q.sql)
		}
	}
	if q.params["start"] == "" || q.params["end"] == "" {
		t.Errorf("start/end must be carried as parameters, got %v", q.params)
	}
	// the literal timestamp value itself must never appear directly concatenated in the SQL text.
	if strings.Contains(q.sql, "2026-06-22 09:00:00") || strings.Contains(q.sql, "2026-06-22 10:00:00") {
		t.Errorf("window bounds must be parameters, not concatenated into the SQL text: %s", q.sql)
	}
}

// an MCP call's request id is `<corr>.<8 hex>`, so a SUT that logs it under a
// correlation field must still be found. Every declared path matches the id exactly OR as the prefix
// before a dot — and the id stays a parameter, never text in the SQL.
func TestBetterStack_BuildQuery_MatchesAPerCallRequestIDByPrefix(t *testing.T) {
	b := &BetterStack{TeamID: "123456", CorrelationFields: []string{"message_json.correlationId", "request_id"}}
	q := b.buildQuery("t123456_social_logs", time.Now().Add(-time.Hour), time.Now(), false)
	for _, want := range []string{
		"= {correlation_id:String} OR startsWith(",
		"concat({correlation_id:String}, '.')",
	} {
		if n := strings.Count(q.sql, want); n != 2 {
			t.Errorf("sql carries %q %d time(s), want 2 (one per declared path): %s", want, n, q.sql)
		}
	}
}

// TestBetterStack_Logs_CannedRows_PerServiceGrouping proves per-service grouping: two sources,
// each its own table/service, merged and each row correctly tagged with ITS source's service —
// not a SQL GROUP BY, since BetterStack has one source per service (CONTEXT).
func TestBetterStack_Logs_CannedRows_PerServiceGrouping(t *testing.T) {
	_, ts := newBSFakeServer(t, map[string][]bsRow{
		"accounting":      {{DT: "2026-06-22 10:00:00", Raw: `{"correlation_id":"tr-20260902T134305716-FX-X-00000001","level":"info","msg":"accounting hit"}`}},
		"trade_execution": {{DT: "2026-06-22 10:00:01", Raw: `{"correlation_id":"tr-20260902T134305716-FX-X-00000001","level":"info","msg":"trade hit"}`}},
	})
	defer ts.Close()

	b := &BetterStack{QueryURL: ts.URL, TeamID: "123456", Sources: map[string]string{
		"accounting":      "accounting-service",
		"trade_execution": "trade-execution-service",
	}}
	logs := b.Logs("tr-20260902T134305716-FX-X-00000001", "30m", time.Time{})
	if !logs.Available {
		t.Fatalf("logs must be available, note=%q", logs.Note)
	}
	if len(logs.Lines) != 2 {
		t.Fatalf("want 2 merged lines, got %d: %+v", len(logs.Lines), logs.Lines)
	}
	byMsg := map[string]string{}
	for _, l := range logs.Lines {
		byMsg[l.Msg] = l.Service
	}
	if byMsg["accounting hit"] != "accounting-service" {
		t.Errorf("accounting row must be tagged accounting-service, got %q", byMsg["accounting hit"])
	}
	if byMsg["trade hit"] != "trade-execution-service" {
		t.Errorf("trade row must be tagged trade-execution-service, got %q", byMsg["trade hit"])
	}
}

// TestBetterStack_CorrelationField_BothPaths proves correlation lookup works across BOTH declared
// field paths: a line nesting the id under message_json.correlationId, and a line carrying it as
// a flat top-level correlation_id — the default two-path chain (config's stated order).
func TestBetterStack_CorrelationField_BothPaths(t *testing.T) {
	b := &BetterStack{} // canonical default chain: message_json.correlationId, then correlation_id
	nested := b.parseLogLine(bsResultRow{ts: "2026-06-22 10:00:00", raw: `{"message_json":{"correlationId":"tr-20260902T134305716-FX-NESTED-00000007"},"level":"info","msg":"nested"}`, service: "svc-a"})
	if nested.Fields["correlation_id"] != "tr-20260902T134305716-FX-NESTED-00000007" {
		t.Errorf("message_json.correlationId path must resolve, got Fields=%v", nested.Fields)
	}
	flat := b.parseLogLine(bsResultRow{ts: "2026-06-22 10:00:01", raw: `{"correlation_id":"tr-20260902T134305716-FX-FLAT-00000008","level":"info","msg":"flat"}`, service: "svc-a"})
	if flat.Fields["correlation_id"] != "tr-20260902T134305716-FX-FLAT-00000008" {
		t.Errorf("top-level correlation_id path must resolve, got Fields=%v", flat.Fields)
	}
}

// TestBetterStack_Timeout_TypedUnavailable: a source that hangs past the configured timeout must
// surface the SAME typed "observability unavailable" outcome the Loki path uses (Available:false
// + an explanatory Note) — never a panic, never a silent empty success.
func TestBetterStack_Timeout_TypedUnavailable(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never responds within the test's lifetime
	}))
	// deliberately NOT ts.Close()'d: httptest.Server.Close blocks until every handler goroutine
	// returns, and this one is parked on <-block until the process exits — closing it here would
	// itself deadlock the test. The server is reclaimed when the test binary exits.

	b := &BetterStack{QueryURL: ts.URL, TeamID: "123456", Timeout: 50 * time.Millisecond,
		Sources: map[string]string{"accounting": "accounting-service"}}

	sagas := b.Sagas("tr-20260902T134305716-FX-X-00000001", "30m", time.Time{})
	if sagas.Available {
		t.Fatalf("a timed-out source must report Available:false, got %+v", sagas)
	}
	if sagas.Note == "" {
		t.Fatalf("a timed-out source must carry an explanatory Note, got empty")
	}

	logs := b.Logs("tr-20260902T134305716-FX-X-00000001", "30m", time.Time{})
	if logs.Available {
		t.Fatalf("a timed-out source must report Available:false, got %+v", logs)
	}
	if logs.Note == "" {
		t.Fatalf("a timed-out source must carry an explanatory Note, got empty")
	}
}

// TestBetterStack_EmptyInputs_HonestErrors: an input error (blank correlation id) is reported
// honestly, never mistaken for a source outage — same VR-A4/VR-A5 contract as Loki.
func TestBetterStack_EmptyInputs_HonestErrors(t *testing.T) {
	b := &BetterStack{QueryURL: "http://unused", TeamID: "1", Sources: map[string]string{"a": "svc"}}
	sagas := b.Sagas("", "30m", time.Time{})
	if sagas.Available {
		t.Fatal("blank correlation id must not be Available:true")
	}
	if sagas.Note == "" {
		t.Fatal("blank correlation id must carry an explanatory Note")
	}
}

var _ Backend = (*BetterStack)(nil) // compile-time proof BetterStack satisfies the shared query surface

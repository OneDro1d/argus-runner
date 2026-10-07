package obsquery

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Spec 26 P1, finding A3: queryLines turns every failure into (nil, false), so the reason a Loki read
// failed is lost. The sandbox-evidence read must say WHY it is `unavailable`, so the one query_range
// call is extracted into queryRange, which returns the reason; queryLines keeps its signature and its
// bytes (every Logs/Sagas test in this package is the guard for that).

var qrStart = time.Unix(1775014130, 0).UTC()
var qrEnd = time.Unix(1775014150, 0).UTC()

func TestQueryRange_NamesHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("no org id; body-canary-7f3a"))
	}))
	defer srv.Close()
	_, err := (&Loki{BaseURL: srv.URL}).queryRange(`{job="x"}`, qrStart, qrEnd, 2000)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("a 401 must be named, got %v", err)
	}
	if strings.Contains(err.Error(), "body-canary-7f3a") {
		t.Fatalf("the response body must never ride the error (it is the server's free text): %v", err)
	}
}

// The stored reason never carries the URL: the query string holds the selector and the sandbox id, and
// a base URL may carry userinfo. Same rule as the chain-error URL rule in DATA-FLOW's validation table.
func TestQueryRange_TransportErrorHasNoURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listens any more: a transport error, not an HTTP status
	_, err := (&Loki{BaseURL: base}).queryRange(`{job="x"} |= "sb-1"`, qrStart, qrEnd, 2000)
	if err == nil {
		t.Fatal("a closed server must be an error")
	}
	for _, leak := range []string{"http://", "query_range", "sb-1", "job="} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("the transport error must not carry the URL or the query (%q found): %v", leak, err)
		}
	}
}

func TestQueryRange_DecodeErrorIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>a proxy login page</html>"))
	}))
	defer srv.Close()
	_, err := (&Loki{BaseURL: srv.URL}).queryRange(`{job="x"}`, qrStart, qrEnd, 2000)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("a 200 that is not a Loki answer must be a named decode error, got %v", err)
	}
}

func TestQueryRange_ReturnsEntriesAndSendsTheQuery(t *testing.T) {
	var got struct {
		sync.Mutex
		q, start, end, limit, dir string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Lock()
		defer got.Unlock()
		v := r.URL.Query()
		got.q, got.start, got.end, got.limit, got.dir = v.Get("query"), v.Get("start"), v.Get("end"), v.Get("limit"), v.Get("direction")
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"stream":{"job":"x"},"values":[["1775014138811000000","line-a"],["1775014139000000000","line-b"]]}]}}`))
	}))
	defer srv.Close()
	entries, err := (&Loki{BaseURL: srv.URL}).queryRange(`{job="x"} |= "sb-1"`, qrStart, qrEnd, 77)
	if err != nil {
		t.Fatalf("queryRange: %v", err)
	}
	if len(entries) != 2 || entries[0].Line != "line-a" || entries[1].TS != "1775014139000000000" {
		t.Fatalf("entries = %+v", entries)
	}
	got.Lock()
	defer got.Unlock()
	if got.q != `{job="x"} |= "sb-1"` || got.limit != "77" || got.dir != "forward" ||
		got.start != "1775014130000000000" || got.end != "1775014150000000000" {
		t.Fatalf("query sent: query=%q start=%s end=%s limit=%s direction=%s", got.q, got.start, got.end, got.limit, got.dir)
	}
}

// T3.1 / T3.3 carried over: Basic Auth only with a credential, X-Scope-OrgID only with a tenant —
// absent otherwise, never present-and-empty (as loki_tenant_test.go pins for queryLines).
func TestQueryRange_CarriesTenantAndBasicAuth(t *testing.T) {
	type seen struct {
		tenant []string
		auth   []string
	}
	var mu sync.Mutex
	var last seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = seen{tenant: r.Header.Values("X-Scope-OrgID"), auth: r.Header.Values("Authorization")}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()

	if _, err := (&Loki{BaseURL: srv.URL, Tenant: "inst-a", Credential: "u:p"}).queryRange(`{job="x"}`, qrStart, qrEnd, 10); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(last.tenant) != 1 || last.tenant[0] != "inst-a" || len(last.auth) != 1 || !strings.HasPrefix(last.auth[0], "Basic ") {
		t.Fatalf("with tenant and credential: saw %+v", last)
	}
	mu.Unlock()

	if _, err := (&Loki{BaseURL: srv.URL}).queryRange(`{job="x"}`, qrStart, qrEnd, 10); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(last.tenant) != 0 || len(last.auth) != 0 {
		t.Fatalf("without tenant and credential both headers must be ABSENT, saw %+v", last)
	}
}

// Guard for the extraction (green before and after): when the Loki read fails, Logs and Sagas still
// say "query failed", never "no lines" — the failure branch of queryLines is what moved. No test in
// this package pinned it before (measured on 8b74ffb by mutating queryLines to return true on error:
// all 82 tests stayed green).
func TestQueryLines_LokiFailureStaysUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	l := &Loki{BaseURL: srv.URL}
	// A minted-shape id: a fragment such as "tr-1" is now refused before any query,
	// which would answer this test's question (does a FAILING Loki read as unavailable?) for the wrong reason.
	if lg := l.Logs("tr-20260902T134305716-FX-QR-00000001", "5m", time.Time{}); lg.Available || !strings.Contains(lg.Note, "log query failed") {
		t.Errorf("Logs on a failing Loki = available:%v note:%q; want unavailable with the query-failed note", lg.Available, lg.Note)
	}
	if sg := l.Sagas("tr-20260902T134305716-FX-QR-00000001", "5m", time.Time{}); sg.Available || !strings.Contains(sg.Note, "saga query failed") {
		t.Errorf("Sagas on a failing Loki = available:%v note:%q; want unavailable with the query-failed note", sg.Available, sg.Note)
	}
}

package obsquery

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// T3.3 (E3 shared ingest): a shared Loki runs auth_enabled: true, and every request must name
// its tenant in X-Scope-OrgID. When no tenant is configured (bundled, adopt, export) the header
// must be ABSENT — not present-and-empty — so those modes send exactly today's requests.

// tenantRecorder is a fake Loki that records, per request path, whether X-Scope-OrgID was
// present at all and what it carried.
type tenantRecorder struct {
	mu    sync.Mutex
	seen  map[string][]string // path -> header values ([] = absent)
	calls int
}

func newTenantRecorder(t *testing.T) (*tenantRecorder, *httptest.Server) {
	t.Helper()
	rec := &tenantRecorder{seen: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.calls++
		rec.seen[r.URL.Path] = r.Header.Values("X-Scope-OrgID")
		rec.mu.Unlock()
		if r.URL.Path == "/loki/api/v1/push" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func oneRequestReport() *report.Report {
	return &report.Report{RunID: "run-1", Layers: []report.Layer{{Layer: "HTTP", Scenarios: []report.ScenarioResult{{
		ID: "S-1", CorrelationID: "tr-20260902T134305716-FX-1-00000002",
		Requests: []report.RequestSample{{AtMs: time.Now().UnixMilli(), Outcome: report.OutcomeSuccess}},
	}}}}}
}

func TestQueryLines_SendsTenantHeaderWhenSet(t *testing.T) {
	rec, srv := newTenantRecorder(t)
	l := &Loki{BaseURL: srv.URL, Tenant: "memstore-k3d"}
	l.Logs("tr-20260902T134305716-FX-1-00000002", "5m", time.Time{})
	if got := rec.seen["/loki/api/v1/query_range"]; len(got) != 1 || got[0] != "memstore-k3d" {
		t.Fatalf("query_range must carry X-Scope-OrgID: memstore-k3d, got %q", got)
	}
}

func TestQueryLines_NoTenantHeaderWhenUnset(t *testing.T) {
	rec, srv := newTenantRecorder(t)
	l := &Loki{BaseURL: srv.URL}
	l.Logs("tr-20260902T134305716-FX-1-00000002", "5m", time.Time{})
	if rec.calls == 0 {
		t.Fatal("the fake Loki was never called — the test proves nothing")
	}
	if got, ok := rec.seen["/loki/api/v1/query_range"]; !ok || len(got) != 0 {
		t.Fatalf("with no tenant, X-Scope-OrgID must be ABSENT, got %q (recorded=%v)", got, ok)
	}
}

func TestPushRequestEvents_SendsTenantHeaderWhenSet(t *testing.T) {
	rec, srv := newTenantRecorder(t)
	if err := PushRequestEvents(&Loki{BaseURL: srv.URL, Tenant: "memstore-k3d"}, "memstore-k3d", "p", "k3d", oneRequestReport()); err != nil {
		t.Fatal(err)
	}
	if got := rec.seen["/loki/api/v1/push"]; len(got) != 1 || got[0] != "memstore-k3d" {
		t.Fatalf("the push must carry X-Scope-OrgID: memstore-k3d, got %q", got)
	}
}

func TestPushRequestEvents_NoTenantHeaderWhenUnset(t *testing.T) {
	rec, srv := newTenantRecorder(t)
	if err := PushRequestEvents(&Loki{BaseURL: srv.URL}, "local", "p", "k3d", oneRequestReport()); err != nil {
		t.Fatal(err)
	}
	if got, ok := rec.seen["/loki/api/v1/push"]; !ok || len(got) != 0 {
		t.Fatalf("with no tenant, the push must carry NO X-Scope-OrgID, got %q (recorded=%v)", got, ok)
	}
}

package toolcore

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// T3.3 (E3 shared ingest): Env.LokiTenant must reach BOTH Loki call sites the executor has — the
// evidence read (lokiFor -> obsquery.Loki.queryLines) and the request-event push in Run
// (obsquery.PushRequestEvents). A tenant threaded into one and not the other would leave half the
// traffic unscoped, which a shared auth_enabled Loki refuses (or, worse, files under nobody).

func TestLokiFor_ThreadsTenant(t *testing.T) {
	e := lokiCredEnv(t, "")
	e.LokiTenant = "memstore-k3d"
	if got := lokiFor(e).Tenant; got != "memstore-k3d" {
		t.Errorf("lokiFor must thread Env.LokiTenant into obsquery.Loki.Tenant, got %q", got)
	}
	e.LokiTenant = ""
	if got := lokiFor(e).Tenant; got != "" {
		t.Errorf("no tenant configured must leave obsquery.Loki.Tenant empty, got %q", got)
	}
}

func TestRun_PushCarriesTenant(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	var mu sync.Mutex
	var pushTenant []string
	pushed := false
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/push" {
			mu.Lock()
			pushed = true
			pushTenant = r.Header.Values("X-Scope-OrgID")
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer loki.Close()

	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = loki.URL
	e.Pushgateway = loki.URL
	e.LokiTenant = "memstore-k3d"

	if _, _, err := Run(e, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !pushed {
		t.Fatal("Run made no request-event push — the test proves nothing")
	}
	if len(pushTenant) != 1 || pushTenant[0] != "memstore-k3d" {
		t.Errorf("Run's request-event push must carry X-Scope-OrgID: memstore-k3d, got %q", pushTenant)
	}
}

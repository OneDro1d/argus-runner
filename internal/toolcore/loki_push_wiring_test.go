package toolcore

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// lokiFor threads observability.loki.push_url ONLY when this executor reads the
// configured Loki (adopt/export: --loki == observability.loki.url). A bundled or shared executor can
// load a config that also declares push_url; its events belong in ITS Loki (and, shared, its tenant),
// never in a hosted one the render did not point it at.

const hostedLokiBlock = `observability:
  loki:
    url: https://logs.example.grafana.net
    push_url: https://logs-prod.example.grafana.net/loki/api/v1/push
    credential: ${LOKI_EXPORT_CREDENTIAL}
`

func TestLokiFor_ThreadsPushURLWhenReadingTheConfiguredLoki(t *testing.T) {
	t.Setenv("LOKI_EXPORT_CREDENTIAL", "hosted-user:hosted-pass")
	e := lokiCredEnv(t, hostedLokiBlock)
	e.Loki = "https://logs.example.grafana.net/" // trailing slash: the same Loki
	if got := lokiFor(e).PushURL; got != "https://logs-prod.example.grafana.net/loki/api/v1/push" {
		t.Errorf("an executor reading the configured Loki must push to its push_url, got %q", got)
	}
}

func TestLokiFor_NoPushURLWhenReadingAnotherLoki(t *testing.T) {
	t.Setenv("LOKI_EXPORT_CREDENTIAL", "hosted-user:hosted-pass")
	e := lokiCredEnv(t, hostedLokiBlock) // e.Loki = http://loki:3100, the bundled default
	if got := lokiFor(e).PushURL; got != "" {
		t.Errorf("a bundled executor must keep pushing to its own Loki, got PushURL %q", got)
	}
	e.Loki, e.LokiTenant = "http://loki.argus-obs.svc:3100", "memstore-k3d" // shared
	if got := lokiFor(e).PushURL; got != "" {
		t.Errorf("a shared executor must keep pushing to the shared Loki under its tenant, got PushURL %q", got)
	}
}

// The call site: Run must push request events through lokiFor, not a bare URL. A hosted Loki that
// requires auth sees the push at push_url, authenticated.
func TestRun_PushesRequestEventsToHostedPushURLWithAuth(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	t.Setenv("LOKI_EXPORT_CREDENTIAL", "hosted-user:hosted-pass")
	var mu sync.Mutex
	var pushedAuthed bool
	hosted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if u, p, ok := r.BasicAuth(); r.URL.Path == "/hosted/push" && ok && u == "hosted-user" && p == "hosted-pass" {
			pushedAuthed = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer hosted.Close()

	cfg := "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n" +
		"observability:\n  loki:\n    url: " + hosted.URL + "\n    push_url: " + hosted.URL + "/hosted/push\n" +
		"    credential: ${LOKI_EXPORT_CREDENTIAL}\n"
	e := rlEnv(t, cfg, map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = hosted.URL
	e.Pushgateway = ""

	p, _, err := Run(e, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if w, has := p.(map[string]any)["obs_push_warnings"]; has {
		t.Errorf("the authenticated push to push_url must succeed; got warnings %v", w)
	}
	mu.Lock()
	defer mu.Unlock()
	if !pushedAuthed {
		t.Error("no authenticated request-event push reached push_url")
	}
}

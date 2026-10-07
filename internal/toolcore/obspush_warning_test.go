package toolcore

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// AC-D32 (issue #173): a REFUSED telemetry push must be visible. Both pushes were `_ =`, and on
// hub-dev Loki refused the runner's request events for 30 minutes (stream cap full): the dashboard
// was empty and nothing said why. The run still must not fail — the verdict does not depend on Loki —
// so the refusal rides the payload (and stderr), with Loki's own reason in it.
func TestRun_ReportsARefusedObservabilityPush(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("maximum active stream limit exceeded when trying to create stream\n"))
	}))
	defer loki.Close()
	pgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer pgw.Close()

	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = loki.URL
	e.Pushgateway = pgw.URL

	p, _, err := Run(e, "", "", "", "")
	if err != nil {
		t.Fatalf("a refused telemetry push must not fail the run: %v", err)
	}
	ws, _ := p.(map[string]any)["obs_push_warnings"].([]string)
	joined := strings.Join(ws, "\n")
	if !strings.Contains(joined, "request-event push to Loki failed: loki push returned 429: maximum active stream limit exceeded") {
		t.Errorf("the Loki refusal, with Loki's reason, is not reported; obs_push_warnings = %q", ws)
	}
	if !strings.Contains(joined, "metrics push to the pushgateway failed") {
		t.Errorf("the pushgateway refusal is not reported; obs_push_warnings = %q", ws)
	}
}

// Healthy stores: no warning key at all, so a clean run's payload is unchanged.
func TestRun_NoObservabilityWarningWhenPushesSucceed(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()

	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = ok.URL
	e.Pushgateway = ok.URL

	p, _, err := Run(e, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if w, has := p.(map[string]any)["obs_push_warnings"]; has {
		t.Errorf("successful pushes must add no warning; got %v", w)
	}
}

package toolcore

import (
	"testing"
)

// item 9, first half (msgbus tester 2026-09-28): "when the instance has no pushgateway / no Loki
// configured (obs none or absent), Run must not attempt the metrics push or the request-event
// push... so no 'WARN observability push failed... no such host'".
//
// Diagnosis (read before touching Run's push block): render-k8s already emits an EXPLICIT
// `--loki ""` / `--pushgateway ""` for `--obs none` (internal/k8srender/k8srender.go's
// obsExecArgsBlock, obsModeNone branch, #287) — never the bundled `http://loki:3100` default — and
// obsquery.PushMetrics/PushRequestEvents ALREADY no-op on an empty URL with no attempt and no
// warning (internal/obsquery/push.go:20-23, internal/obsquery/lokievents.go:46-48). So for the
// documented `--obs none` shape, Run already does not attempt either push and already logs
// nothing. This test locks that behaviour in at the toolcore.Run level (nothing exercised it end
// to end before this ticket — obspush_warning_test.go only covers the REFUSED-push case, both URLs
// non-empty). No code change was needed for this half; see the report for what WAS changed (the
// deprecation-message wording, item 9's second half) and why the render-layer fix already covers
// the reported symptom.
func TestRun_ObsNoneOrAbsent_NoPushAttemptedNoWarning(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.Loki = ""        // --obs none / absent: render-k8s's obsExecArgsBlock emits this explicitly
	e.Pushgateway = "" // same

	p, _, err := Run(e, "", "", "", "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if w, has := p.(map[string]any)["obs_push_warnings"]; has {
		t.Errorf("no obs plane configured must produce NO push warning at all; got %v", w)
	}
}

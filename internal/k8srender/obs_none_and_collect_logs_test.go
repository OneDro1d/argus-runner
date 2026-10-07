package k8srender

// obs_none_and_collect_logs_test.go — two independent PROMISEs:
//
//  1. --obs=none renders NO observability object of any kind, and the executor's own --loki/
//     --pushgateway args are explicit empties (never a default pointing at nothing).
//  2. --collect-sut-logs (Instance.CollectSUTLogs), default false, gates promtail ALONE: Loki and
//     the pushgateway still render in bundled/shared mode without it, but no promtail (and so no
//     SUT-namespace log glob) is rendered anywhere until it is set. The export hosted-Loki target
//     (whose ENTIRE obs.yaml is promtail) renders nothing at all without it.

import (
	"strings"
	"testing"
)

func TestRenderObs_NoneMode_RendersNothing(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "none"
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(none): %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--obs=none must render NOTHING, got %d bytes:\n%s", len(out), out)
	}
}

func TestInstanceValidate_noneModeAccepted(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "none"
	if _, err := RenderExecutor(in); err != nil {
		t.Fatalf("--obs=none must be an accepted mode, RenderExecutor refused: %v", err)
	}
}

// TestRenderExecutor_NoneMode_ExplicitEmptyObsArgs pins obsExecArgsBlock's none branch: never a
// default that points at a Loki/Pushgateway this mode never deploys.
func TestRenderExecutor_NoneMode_ExplicitEmptyObsArgs(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "none"
	out, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor(none): %v", err)
	}
	if !strings.Contains(out, "- --loki\n            - \"\"\n") {
		t.Errorf("none mode must render an EXPLICIT empty --loki, got:\n%s", out)
	}
	if !strings.Contains(out, "- --pushgateway\n            - \"\"\n") {
		t.Errorf("none mode must render an EXPLICIT empty --pushgateway, got:\n%s", out)
	}
	if strings.Contains(out, "http://loki:3100") || strings.Contains(out, "http://pushgateway:9091") {
		t.Errorf("none mode must not carry the bundled defaults:\n%s", out)
	}
}

// ── --collect-sut-logs: bundled mode ────────────────────────────────────────────────────────────

func TestRenderObs_CollectSUTLogsOff_Bundled_NoPromtailButLokiAndPushgatewayRemain(t *testing.T) {
	in := sampleInstance()
	in.CollectSUTLogs = false
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs: %v", err)
	}
	if strings.Contains(out, "kind: DaemonSet") || strings.Contains(out, "app: promtail") {
		t.Errorf("--collect-sut-logs=false must render NO promtail, got:\n%s", out)
	}
	if strings.Contains(out, "/var/log/pods/") {
		t.Errorf("--collect-sut-logs=false must not render the SUT log glob, got:\n%s", out)
	}
	if !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "app: loki") {
		t.Errorf("--collect-sut-logs=false must still render Loki:\n%s", out)
	}
	if !strings.Contains(out, "app: pushgateway") {
		t.Errorf("--collect-sut-logs=false must still render the pushgateway:\n%s", out)
	}
}

func TestRenderObs_CollectSUTLogsOn_Bundled_PromtailPresent(t *testing.T) {
	in := sampleInstance()
	in.CollectSUTLogs = true
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs: %v", err)
	}
	if !strings.Contains(out, "kind: DaemonSet") || !strings.Contains(out, "app: promtail") {
		t.Errorf("--collect-sut-logs=true must render promtail, got:\n%s", out)
	}
	if !strings.Contains(out, "/var/log/pods/"+in.SUTNamespace+"_*/*/*.log") {
		t.Errorf("--collect-sut-logs=true must render the SUT log glob for %q, got:\n%s", in.SUTNamespace, out)
	}
}

// ── --collect-sut-logs: export hosted-Loki target (T3.1) — its WHOLE obs.yaml is promtail ──────

func TestRenderObs_CollectSUTLogsOff_ExportHostedLoki_RendersNothing(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "export"
	in.ObsLokiURL = "http://operator-loki.example:3100"
	in.ObsLokiPushURL = "https://logs-example.grafana.net/loki/api/v1/push"
	in.ObsCredentialVarName = "LOKI_CREDENTIAL"
	in.CollectSUTLogs = false
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(export, hosted-loki, no collect-sut-logs): %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("export+hosted-Loki with --collect-sut-logs=false must render NOTHING (there is nothing to forward), got:\n%s", out)
	}
}

func TestRenderObs_CollectSUTLogsOn_ExportHostedLoki_PromtailOnly(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "export"
	in.ObsLokiURL = "http://operator-loki.example:3100"
	in.ObsLokiPushURL = "https://logs-example.grafana.net/loki/api/v1/push"
	in.ObsCredentialVarName = "LOKI_CREDENTIAL"
	in.CollectSUTLogs = true
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(export, hosted-loki): %v", err)
	}
	if !strings.Contains(out, "kind: DaemonSet") {
		t.Fatalf("export+hosted-Loki with --collect-sut-logs=true must still render promtail:\n%s", out)
	}
}

// ── --collect-sut-logs: shared mode (T3.3) — pushgateway is unconditional, promtail is not ─────

func TestRenderObs_CollectSUTLogsOff_Shared_NoPromtailButPushgatewayRemains(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "shared"
	in.ObsSharedURL = "http://shared-loki.example:3100"
	in.CollectSUTLogs = false
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(shared): %v", err)
	}
	if strings.Contains(out, "kind: DaemonSet") || strings.Contains(out, "app: promtail") {
		t.Errorf("--collect-sut-logs=false must render NO promtail under --obs=shared, got:\n%s", out)
	}
	if !strings.Contains(out, "app: pushgateway") {
		t.Errorf("--collect-sut-logs=false must still render the pushgateway under --obs=shared:\n%s", out)
	}
}

func TestRenderObs_CollectSUTLogsOn_Shared_PromtailPresent(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "shared"
	in.ObsSharedURL = "http://shared-loki.example:3100"
	in.CollectSUTLogs = true
	out, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatalf("RenderObs(shared): %v", err)
	}
	if !strings.Contains(out, "kind: DaemonSet") || !strings.Contains(out, "tenant_id: "+in.ID) {
		t.Errorf("--collect-sut-logs=true must render promtail (tenant %q) under --obs=shared, got:\n%s", in.ID, out)
	}
}

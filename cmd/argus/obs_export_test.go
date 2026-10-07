package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T3.1 (E3 export mode): render-k8s --obs export end to end on a temp config — both accepted
// targets (hosted Loki, BetterStack), the refusal shapes already covered in obs_modes_test.go,
// and the one property that must hold no matter what: the credential VALUE never reaches disk.

const exportHostedLokiConfig = "project:\n  name: obs-modes-test\ntargets:\n  http:\n    base_url: https://example.invalid\n" +
	"observability:\n  loki:\n    url: https://logs.example.grafana.net\n    push_url: https://logs.example.grafana.net/loki/api/v1/push\n    credential: ${OBS_EXPORT_MARKER_VAR}\n"

const exportBetterStackConfig = "project:\n  name: obs-modes-test\ntargets:\n  http:\n    base_url: https://example.invalid\n" +
	"observability:\n  betterstack:\n    query_url: https://eu-nbg-2-connect.betterstackdata.com\n    credential: ${OBS_EXPORT_MARKER_VAR}\n    team_id: \"123456\"\n    sources:\n      accounting: accounting-service\n"

// export + hosted-Loki renders successfully end to end: obs.yaml carries promtail, executor.yaml
// carries the --loki query url, and both JSON fields (obs_loki_url, obs_credential_var) surface
// for onboard.sh.
func TestRenderK8s_ObsExportHostedLoki_EndToEnd(t *testing.T) {
	t.Setenv("OBS_EXPORT_MARKER_VAR", "hosted-user:hosted-pass")
	rc, got, out := runRenderK8sWithConfig(t, exportHostedLokiConfig, "--obs", "export")
	if rc != exitOK {
		t.Fatalf("--obs export (hosted-loki): exit %d, want exitOK: %+v", rc, got)
	}
	if got.ObsMode != "export" {
		t.Errorf("obs_mode = %q, want export", got.ObsMode)
	}
	if got.ObsLoki != "https://logs.example.grafana.net" {
		t.Errorf("obs_loki_url = %q, want the configured query url", got.ObsLoki)
	}
	if got.ObsCredentialVar != "OBS_EXPORT_MARKER_VAR" {
		t.Errorf("obs_credential_var = %q, want OBS_EXPORT_MARKER_VAR", got.ObsCredentialVar)
	}
	obsBytes, err := os.ReadFile(filepath.Join(out, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if !strings.Contains(string(obsBytes), "kind: DaemonSet") || !strings.Contains(string(obsBytes), "promtail") {
		t.Error("export+hosted-loki obs.yaml must render the promtail DaemonSet")
	}
	if strings.Contains(string(obsBytes), "kind: Deployment") && strings.Contains(string(obsBytes), "name: loki") {
		t.Error("export+hosted-loki obs.yaml must not render a Loki Deployment")
	}
	execBytes, err := os.ReadFile(filepath.Join(out, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(execBytes), "secretKeyRef: {name: argus-obs-credential, key: OBS_EXPORT_MARKER_VAR}") {
		t.Errorf("executor.yaml must reference the argus-obs-credential Secret, got:\n%s", execBytes)
	}
}

// export + BetterStack renders successfully end to end: obs.yaml is EMPTY (BetterStack deploys
// nothing of Argus's own), and the executor still carries the credential secretKeyRef.
func TestRenderK8s_ObsExportBetterStack_EndToEnd(t *testing.T) {
	t.Setenv("OBS_EXPORT_MARKER_VAR", "bs-user:bs-pass")
	rc, got, out := runRenderK8sWithConfig(t, exportBetterStackConfig, "--obs", "export")
	if rc != exitOK {
		t.Fatalf("--obs export (betterstack): exit %d, want exitOK: %+v", rc, got)
	}
	if got.ObsCredentialVar != "OBS_EXPORT_MARKER_VAR" {
		t.Errorf("obs_credential_var = %q, want OBS_EXPORT_MARKER_VAR", got.ObsCredentialVar)
	}
	if got.ObsLoki != "" {
		t.Errorf("obs_loki_url must be empty for a BetterStack export target, got %q", got.ObsLoki)
	}
	obsBytes, err := os.ReadFile(filepath.Join(out, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if strings.TrimSpace(string(obsBytes)) != "" {
		t.Errorf("export+betterstack obs.yaml must be EMPTY, got %d bytes:\n%s", len(obsBytes), obsBytes)
	}
	execBytes, err := os.ReadFile(filepath.Join(out, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(execBytes), "secretKeyRef: {name: argus-obs-credential, key: OBS_EXPORT_MARKER_VAR}") {
		t.Errorf("executor.yaml must reference the argus-obs-credential Secret, got:\n%s", execBytes)
	}
}

// TestCmdRenderK8s_ExportNeverEmitsCredentialValue is the PROMISE's core safety property, proven
// at the layer where the value actually enters the pipeline: a real env var (a distinctive marker
// no accidental string could produce) set exactly as onboard.sh's secrets preflight would set it,
// then both rendered files on disk are read back and searched for it. This is what the
// k8srender-package test of the same name could NOT prove (it never receives a real value at
// all) — this is the layer that does the exclusion (cmdRenderK8s's `delete(secretEnv, ...)`).
func TestCmdRenderK8s_ExportNeverEmitsCredentialValue(t *testing.T) {
	const marker = "MARKER-4f8c9a2e-must-never-reach-a-rendered-manifest"
	for _, tc := range []struct {
		name   string
		config string
	}{
		{"hosted-loki", exportHostedLokiConfig},
		{"betterstack", exportBetterStackConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OBS_EXPORT_MARKER_VAR", marker)
			rc, got, out := runRenderK8sWithConfig(t, tc.config, "--obs", "export")
			if rc != exitOK {
				t.Fatalf("render-k8s --obs export (%s): exit %d, want exitOK: %+v", tc.name, rc, got)
			}
			execBytes, err := os.ReadFile(filepath.Join(out, "executor.yaml"))
			if err != nil {
				t.Fatalf("read executor.yaml: %v", err)
			}
			obsBytes, err := os.ReadFile(filepath.Join(out, "obs.yaml"))
			if err != nil {
				t.Fatalf("read obs.yaml: %v", err)
			}
			if strings.Contains(string(execBytes), marker) {
				t.Errorf("%s: executor.yaml carries the credential VALUE — must reference argus-obs-credential by secretKeyRef instead:\n%s", tc.name, execBytes)
			}
			if strings.Contains(string(obsBytes), marker) {
				t.Errorf("%s: obs.yaml carries the credential VALUE:\n%s", tc.name, obsBytes)
			}
		})
	}
}

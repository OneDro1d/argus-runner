package main

// render_k8s_podmonitor_obslogs_test.go covers three PROMISEs added to `argus render-k8s`:
//
//  1. --podmonitor auto|on|off controls the managed-tier PodMonitor object (validated up front;
//     auto with no --kube-context includes it and prints a one-line stderr note).
//  2. --collect-sut-logs, default OFF, gates whether promtail (and so the SUT namespace's pod
//     logs) is rendered at all; ON prints which namespace will be read.
//  3. render-k8s always warns (stderr) that the written executor.yaml carries the exec-tokens
//     Secret's credentials in clear, naming the file, whenever it writes one.
//
// These use their OWN arg-building helper (renderK8sRaw) rather than obs_modes_test.go's
// runRenderK8sWithConfig / render_next_test.go's runRenderK8s, which both pin
// "--collect-sut-logs" into every call for THEIR OWN pre-existing tests (written against the old
// always-on default) — this file needs to observe the flag's actual, unforced default.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderK8sRaw runs `render-k8s` with no flag forced beyond the bare minimum every render needs,
// capturing stdout (the JSON) and stderr (the printed notices) separately.
func renderK8sRaw(t *testing.T, configBody string, extra ...string) (rc int, out obsRenderOutput, outDir, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	if configBody == "" {
		configBody = noObsConfig
	}
	if err := os.WriteFile(cfg, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_ENROLLMENT_TOKEN", "enrollment-test-token")
	t.Setenv("ARGUS_CP_URL", "https://cp.example.invalid")
	t.Setenv("ARGUS_WORKSPACE_ID", "ws_test")
	outDir = filepath.Join(dir, "out")

	args := append([]string{"render-k8s", "--config", cfg, "--instance-id", "podmon-obslogs-test",
		"--sut-namespace", "sut-app-ns", "--image", "registry.invalid/argus@sha256:" + strings.Repeat("0", 64),
		"--tier", "aks", "--replicas", "1", "--out", outDir}, extra...)
	rc = exitOK
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() { rc = dispatch(args) })
	})
	if strings.TrimSpace(stdout) != "" {
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatalf("render-k8s stdout is not the JSON object it should be: %v\n%s", err, stdout)
		}
	}
	return rc, out, outDir, stdout, stderr
}

// ── --podmonitor: validation ─────────────────────────────────────────────────────────────────

func TestRenderK8s_PodmonitorInvalidValueRefused(t *testing.T) {
	rc, out, outDir, _, _ := renderK8sRaw(t, noObsConfig, "--podmonitor", "sideways")
	if rc == exitOK {
		t.Fatalf("--podmonitor sideways must be refused, got exitOK: %+v", out)
	}
	if !strings.Contains(out.Error, "auto") || !strings.Contains(out.Error, "on") || !strings.Contains(out.Error, "off") {
		t.Errorf("refusal must name all three accepted values, got %q", out.Error)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("--podmonitor sideways must write NOTHING before refusing, found %d entries", len(entries))
	}
}

// ── --podmonitor=off: never renders the PodMonitor, whatever the tier ───────────────────────────

func TestRenderK8s_PodmonitorOff_OmitsPodMonitor(t *testing.T) {
	rc, out, outDir, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--podmonitor", "off")
	if rc != exitOK {
		t.Fatalf("--podmonitor off: exit %d: %+v", rc, out)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if strings.Contains(string(obs), "kind: PodMonitor") {
		t.Errorf("--podmonitor off must omit the PodMonitor, got:\n%s", obs)
	}
	// off is a deliberate, explicit choice — no note is owed for it (unlike auto's two branches).
	if strings.Contains(stderr, "podmonitor:") {
		t.Errorf("--podmonitor off printed an unexpected podmonitor note: %q", stderr)
	}
}

func TestRenderK8s_PodmonitorOn_IncludesPodMonitorEvenWithEnvOptOut(t *testing.T) {
	t.Setenv("ARGUS_OBS_PROM_LABEL", "none")
	rc, out, outDir, _, _ := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--podmonitor", "on")
	if rc != exitOK {
		t.Fatalf("--podmonitor on: exit %d: %+v", rc, out)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if !strings.Contains(string(obs), "kind: PodMonitor") {
		t.Errorf("--podmonitor on must FORCE the PodMonitor in even over ARGUS_OBS_PROM_LABEL=none, got:\n%s", obs)
	}
}

// ── --podmonitor=auto (default): no --kube-context -> included, with a note ─────────────────────

func TestRenderK8s_PodmonitorAuto_NoKubeContext_IncludesAndNotes(t *testing.T) {
	rc, out, outDir, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs")
	if rc != exitOK {
		t.Fatalf("--podmonitor auto (default), no --kube-context: exit %d: %+v", rc, out)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if !strings.Contains(string(obs), "kind: PodMonitor") {
		t.Errorf("auto with no --kube-context must still include the PodMonitor (today's default), got:\n%s", obs)
	}
	if !strings.Contains(stderr, "podmonitor:") || !strings.Contains(stderr, "--podmonitor=off") {
		t.Errorf("auto with no --kube-context must print a note naming --podmonitor=off as the way to drop it, got stderr: %q", stderr)
	}
}

// ── --collect-sut-logs: default OFF ─────────────────────────────────────────────────────────────

func TestRenderK8s_CollectSUTLogs_DefaultOff_NoPromtailNoNotice(t *testing.T) {
	rc, out, outDir, _, stderr := renderK8sRaw(t, noObsConfig)
	if rc != exitOK {
		t.Fatalf("default (no --collect-sut-logs): exit %d: %+v", rc, out)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if strings.Contains(string(obs), "kind: DaemonSet") || strings.Contains(string(obs), "app: promtail") {
		t.Errorf("--collect-sut-logs default OFF must render no promtail, got:\n%s", obs)
	}
	if strings.Contains(string(obs), "/var/log/pods/") {
		t.Errorf("--collect-sut-logs default OFF must not render the SUT log glob, got:\n%s", obs)
	}
	// Loki + pushgateway (metrics) must be unaffected.
	if !strings.Contains(string(obs), "app: loki") || !strings.Contains(string(obs), "app: pushgateway") {
		t.Errorf("--collect-sut-logs default OFF must still render Loki + pushgateway, got:\n%s", obs)
	}
	if strings.Contains(stderr, "collect-sut-logs: ON") {
		t.Errorf("no notice is owed when the flag is OFF, got stderr: %q", stderr)
	}
}

func TestRenderK8s_CollectSUTLogs_On_RendersPromtailAndPrintsNamespace(t *testing.T) {
	rc, out, outDir, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs")
	if rc != exitOK {
		t.Fatalf("--collect-sut-logs: exit %d: %+v", rc, out)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if !strings.Contains(string(obs), "kind: DaemonSet") {
		t.Errorf("--collect-sut-logs must render promtail, got:\n%s", obs)
	}
	if !strings.Contains(stderr, "collect-sut-logs: ON") || !strings.Contains(stderr, "sut-app-ns") {
		t.Errorf("--collect-sut-logs must print which namespace it reads (sut-app-ns), got stderr: %q", stderr)
	}
}

// ── --obs=none: renders no observability object anywhere, executor still starts ────────────────

func TestRenderK8s_ObsNone_EmptyObsYamlExecutorStillRenders(t *testing.T) {
	rc, out, outDir, _, _ := renderK8sRaw(t, noObsConfig, "--obs", "none")
	if rc != exitOK {
		t.Fatalf("--obs none: exit %d: %+v", rc, out)
	}
	if out.ObsMode != "none" {
		t.Errorf("obs_mode = %q, want none", out.ObsMode)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if strings.TrimSpace(string(obs)) != "" {
		t.Errorf("--obs none must write an EMPTY obs.yaml, got %d bytes:\n%s", len(obs), obs)
	}
	exec, err := os.ReadFile(filepath.Join(outDir, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(exec), "kind: Deployment") || !strings.Contains(string(exec), "name: executor") {
		t.Errorf("--obs none must still render a normal executor Deployment (it still starts), got:\n%s", exec)
	}
}

func TestRenderK8s_ObsUnknownValueRefused_ListsNone(t *testing.T) {
	rc, out, _, _, _ := renderK8sRaw(t, noObsConfig, "--obs", "sideways")
	if rc == exitOK {
		t.Fatalf("--obs sideways must be refused, got exitOK: %+v", out)
	}
	for _, want := range []string{"bundled", "adopt", "export", "shared", "none"} {
		if !strings.Contains(out.Error, want) {
			t.Errorf("unknown --obs value refusal must list %q, got %q", want, out.Error)
		}
	}
}

// ── executor.yaml holds the Secret in clear: always warned, naming the file ─────────────────────

func TestRenderK8s_SecretInClearWarning_PrintedWithFilePath(t *testing.T) {
	_, _, outDir, _, stderr := renderK8sRaw(t, noObsConfig)
	execPath := filepath.Join(outDir, "executor.yaml")
	if !strings.Contains(stderr, execPath) {
		t.Errorf("secret warning must name the executor.yaml path %q, got stderr: %q", execPath, stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "clear") {
		t.Errorf("secret warning must say the credentials are in CLEAR, got stderr: %q", stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "delete") {
		t.Errorf("secret warning must say to delete the file, got stderr: %q", stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "commit") {
		t.Errorf("secret warning must say never to commit the file, got stderr: %q", stderr)
	}
}

func TestRenderK8s_SecretInClearWarning_NotPrintedWithoutOut(t *testing.T) {
	// No --out: nothing is written to disk, so the file-specific warning (which names a path) does
	// not apply — render-k8s streams the manifests to stdout instead.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte(noObsConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_ENROLLMENT_TOKEN", "enrollment-test-token")
	t.Setenv("ARGUS_CP_URL", "https://cp.example.invalid")
	t.Setenv("ARGUS_WORKSPACE_ID", "ws_test")
	args := []string{"render-k8s", "--config", cfg, "--instance-id", "podmon-obslogs-test",
		"--sut-namespace", "sut-app-ns", "--image", "registry.invalid/argus@sha256:" + strings.Repeat("0", 64),
		"--tier", "aks", "--replicas", "1"}
	var rc int
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { rc = dispatch(args) })
	})
	if rc != exitOK {
		t.Fatalf("render-k8s to stdout: exit %d, stdout=%s stderr=%s", rc, stdout, stderr)
	}
	if strings.Contains(stderr, "executor.yaml") {
		t.Errorf("no --out was given (nothing written to disk); the file-specific secret warning should not fire, got stderr: %q", stderr)
	}
}

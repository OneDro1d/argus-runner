package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderK8sRefusesATierTheControlPlaneRefuses drives the REAL render-k8s with the tier a fresh
// agent actually typed on a homelab k3s cluster (2026-09-23). It rendered, the agent created the
// namespace and the executor, and registration answered `500 invalid tier "k3s"` — in a namespace the
// agent then had no permission to delete. The refusal must come BEFORE anything is written, so the
// assertion that matters is the empty out directory, not the exit code.
func TestRenderK8sRefusesATierTheControlPlaneRefuses(t *testing.T) {
	for _, tier := range []string{"k3s", "kind", "minikube", "typo"} {
		t.Run(tier, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "argus-config.yaml")
			if err := os.WriteFile(cfg, []byte("project:\n  name: render-tier-test\n"+
				"targets:\n  http:\n    base_url: https://example.invalid\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
			t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
			t.Setenv("ARGUS_CP_URL", "https://cp.example.invalid")
			t.Setenv("ARGUS_WORKSPACE_ID", "ws_test")
			out := filepath.Join(dir, "out")

			rc := exitOK
			stdout := captureStdout(t, func() {
				rc = dispatch([]string{"render-k8s", "--config", cfg, "--instance-id", "render-tier",
					"--sut-namespace", "sut", "--image", "registry.invalid/argus@sha256:" + strings.Repeat("0", 64),
					"--tier", tier, "--replicas", "1", "--out", out})
			})
			if rc == exitOK {
				t.Fatalf("render-k8s --tier %s exited ok; the control plane refuses this tier at registration:\n%s", tier, stdout)
			}
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("render-k8s output is not the JSON error object it should be: %v\n%s", err, stdout)
			}
			if !strings.Contains(got.Error, "--tier managed") {
				t.Errorf("error = %q — it must name the tier to use instead, or the operator is left guessing", got.Error)
			}
			if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
				t.Errorf("render-k8s wrote %d file(s) into --out before refusing; nothing may exist to be created", len(entries))
			}
		})
	}
}

// TestRenderK8sAcceptsEveryRegistrableTier is the other half: the guard must not refuse a tier that
// registers, or it moves the failure instead of removing it. Empty is included because it is the
// flag's documented fall-through to k3d.
func TestRenderK8sAcceptsEveryRegistrableTier(t *testing.T) {
	for _, tier := range []string{"k3d", "aks", "eks", "gke", "k8s-dev", "managed", "AKS", ""} {
		t.Run(tier, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "argus-config.yaml")
			if err := os.WriteFile(cfg, []byte("project:\n  name: render-tier-test\n"+
				"targets:\n  http:\n    base_url: https://example.invalid\n"), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
			t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
			t.Setenv("ARGUS_CP_URL", "https://cp.example.invalid")
			t.Setenv("ARGUS_WORKSPACE_ID", "ws_test")

			rc := exitOK
			stdout := captureStdout(t, func() {
				rc = dispatch([]string{"render-k8s", "--config", cfg, "--instance-id", "render-tier",
					"--sut-namespace", "sut", "--image", "registry.invalid/argus@sha256:" + strings.Repeat("0", 64),
					"--tier", tier, "--replicas", "1", "--out", filepath.Join(dir, "out")})
			})
			if rc != exitOK {
				t.Fatalf("render-k8s --tier %s exited %d; this tier registers:\n%s", tier, rc, stdout)
			}
		})
	}
}

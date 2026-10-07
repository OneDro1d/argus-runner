package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §1, through the REAL CLI command (not just
// internal/k8srender's unit tests): a declared `path:` schema reaches the instance ConfigMap, and
// a MISSING declared file fails render-k8s naming the schema and the path (config.Load, called
// before k8srender.Instance is even built, already refuses this — see internal/config/schemas.go).

func TestRenderK8s_MessageSchemaPath_EmbeddedInExecutorYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	avsc := `{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"}]}`
	if err := os.WriteFile(filepath.Join(dir, "schemas", "ping.avsc"), []byte(avsc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := noObsConfig + "\nmessage_schemas:\n  ping:\n    path: schemas/ping.avsc\n"
	rc, _, outDir, _, _ := renderK8sRawFrom(t, dir, cfgBody)
	if rc != exitOK {
		t.Fatalf("render-k8s with a declared path: schema must succeed, got rc=%d", rc)
	}
	exec, err := os.ReadFile(filepath.Join(outDir, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(exec), `"name":"Ping"`) {
		t.Errorf("the schema's own content must be embedded in executor.yaml, got:\n%s", exec)
	}
	if !strings.Contains(string(exec), "path: schemas/ping.avsc") {
		t.Errorf("the config volume must mount the schema back at its declared relative path, got:\n%s", exec)
	}
}

func TestRenderK8s_MessageSchemaPath_MissingFileFailsNamingSchemaAndPath(t *testing.T) {
	dir := t.TempDir()
	// Deliberately do NOT create schemas/ping.avsc.
	cfgBody := noObsConfig + "\nmessage_schemas:\n  ping:\n    path: schemas/ping.avsc\n"
	rc, out, outDir, _, _ := renderK8sRawFrom(t, dir, cfgBody)
	if rc == exitOK {
		t.Fatalf("render-k8s with a MISSING declared path: schema must fail, got exitOK: %+v", out)
	}
	if !strings.Contains(out.Error, "ping") || !strings.Contains(out.Error, "schemas/ping.avsc") {
		t.Errorf("the refusal must name the schema (%q) and the path (%q): %q", "ping", "schemas/ping.avsc", out.Error)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("a failed render must write NOTHING, found %d entries in %s", len(entries), outDir)
	}
}

// renderK8sRawFrom is renderK8sRaw (render_k8s_podmonitor_obslogs_test.go) but writes the config
// under a CALLER-CHOSEN directory (so a `path:` schema can sit alongside it at a real relative
// path) instead of a fresh t.TempDir() every time.
func renderK8sRawFrom(t *testing.T, dir, configBody string) (rc int, out obsRenderOutput, outDir, stdout, stderr string) {
	t.Helper()
	cfg := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_ENROLLMENT_TOKEN", "enrollment-test-token")
	t.Setenv("ARGUS_CP_URL", "https://cp.example.invalid")
	t.Setenv("ARGUS_WORKSPACE_ID", "ws_test")
	outDir = filepath.Join(dir, "out")
	args := []string{"render-k8s", "--config", cfg, "--instance-id", "msgschema-test",
		"--sut-namespace", "sut-app-ns", "--image", "registry.invalid/argus@sha256:" + strings.Repeat("0", 64),
		"--tier", "aks", "--replicas", "1", "--out", outDir}
	rc = exitOK
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() { rc = dispatch(args) })
	})
	if strings.TrimSpace(stdout) != "" {
		_ = json.Unmarshal([]byte(stdout), &out)
	}
	return rc, out, outDir, stdout, stderr
}

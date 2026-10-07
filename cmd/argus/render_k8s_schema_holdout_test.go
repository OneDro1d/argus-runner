package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── ADVERSARIAL HOLDOUT (H8) ─────────────────────────────────────────────────────────────────────
// design §1, through the REAL CLI (render-k8s), reusing renderK8sRawFrom/noObsConfig
// (render_k8s_message_schemas_test.go, same package) as the test seam.

func TestHoldout_H8_PathSchema_InConfigMapAndMountedAtConfigPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	avsc := `{"type":"record","name":"HoldoutPing","fields":[{"name":"x","type":"string"}]}`
	if err := os.WriteFile(filepath.Join(dir, "schemas", "holdout.avsc"), []byte(avsc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := noObsConfig + "\nmessage_schemas:\n  hp:\n    path: schemas/holdout.avsc\n"
	rc, _, outDir, _, _ := renderK8sRawFrom(t, dir, cfgBody)
	if rc != exitOK {
		t.Fatalf("render-k8s with a declared path: schema must succeed, got rc=%d", rc)
	}
	exec, err := os.ReadFile(filepath.Join(outDir, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	execStr := string(exec)
	if !strings.Contains(execStr, `"name":"HoldoutPing"`) {
		t.Errorf("the schema's own content must be embedded (ConfigMap), got:\n%s", execStr)
	}
	if !strings.Contains(execStr, "/config/schemas/holdout.avsc") && !strings.Contains(execStr, "path: schemas/holdout.avsc") {
		t.Errorf("the declared relative path must be mounted back at /config/<path>, got:\n%s", execStr)
	}
}

func TestHoldout_H8_PathSchema_MissingFile_FailsNamingSchemaAndPath(t *testing.T) {
	dir := t.TempDir()
	// schemas/holdout2.avsc deliberately not created.
	cfgBody := noObsConfig + "\nmessage_schemas:\n  hp2:\n    path: schemas/holdout2.avsc\n"
	rc, out, outDir, _, _ := renderK8sRawFrom(t, dir, cfgBody)
	if rc == exitOK {
		t.Fatalf("render-k8s with a MISSING declared path: schema must fail, got exitOK: %+v", out)
	}
	if !strings.Contains(out.Error, "hp2") {
		t.Errorf("the refusal must name the schema %q, got %q", "hp2", out.Error)
	}
	if !strings.Contains(out.Error, "schemas/holdout2.avsc") {
		t.Errorf("the refusal must name the path %q, got %q", "schemas/holdout2.avsc", out.Error)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("a failed render must write NOTHING, found %d entries in %s", len(entries), outDir)
	}
}

// H8's third clause: "inline: needs nothing extra" — an inline schema must render successfully with
// NO schemas/ directory or any other file on disk at all.
func TestHoldout_H8_InlineSchema_NeedsNoExtraFile(t *testing.T) {
	dir := t.TempDir() // deliberately empty except the config file renderK8sRawFrom writes
	avsc := `{"type":"record","name":"HoldoutInline","fields":[{"name":"y","type":"long"}]}`
	cfgBody := noObsConfig + "\nmessage_schemas:\n  hi:\n    inline: '" + avsc + "'\n"
	rc, out, outDir, _, stderr := renderK8sRawFrom(t, dir, cfgBody)
	if rc != exitOK {
		t.Fatalf("render-k8s with an inline: schema and NO extra file must succeed, got rc=%d out=%+v stderr=%s", rc, out, stderr)
	}
	exec, err := os.ReadFile(filepath.Join(outDir, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(exec), `"name":"HoldoutInline"`) {
		t.Errorf("the inline schema's content must still reach the ConfigMap, got:\n%s", exec)
	}
	// Confirm nothing under schemas/ was ever required: the temp dir never had one.
	if _, err := os.Stat(filepath.Join(dir, "schemas")); !os.IsNotExist(err) {
		t.Errorf("test fixture bug: a schemas/ dir unexpectedly exists")
	}
}

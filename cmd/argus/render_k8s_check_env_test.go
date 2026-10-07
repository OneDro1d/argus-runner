package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// /, the k3d/aks half: a name declared under `check_env` has no ${VAR}
// in the config text, so the render's own sweep of ${VAR}s (referencedVars) never saw it. A declared
// name must reach the executor's Secret (exec-tokens, read via envFrom) exactly like a credential.

const checkEnvFakeValue = "fake-render-value-7731"

func TestRenderK8s_CheckEnvNameReachesTheExecutorSecret(t *testing.T) {
	t.Setenv("SOME_PASSWORD", checkEnvFakeValue)
	dir := t.TempDir()
	cfgBody := noObsConfig + "\ncheck_env:\n  - SOME_PASSWORD\n"
	rc, out, outDir, stdout, stderr := renderK8sRawFrom(t, dir, cfgBody)
	if rc != exitOK {
		t.Fatalf("render-k8s with a declared check_env name must succeed, rc=%d: %+v", rc, out)
	}
	exec, err := os.ReadFile(filepath.Join(outDir, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(exec), `  SOME_PASSWORD: "`+checkEnvFakeValue+`"`) {
		t.Errorf("the declared name SOME_PASSWORD must be a key of the exec-tokens Secret, but executor.yaml has none (%d bytes)", len(exec))
	}
	// The value lives in the Secret only: the ConfigMap embeds the config, which holds the NAME.
	if n := strings.Count(string(exec), checkEnvFakeValue); n != 1 {
		t.Errorf("the value must appear exactly once (the Secret key), found %d times", n)
	}
	// What the render PRINTS never carries it.
	if strings.Contains(stdout+stderr, checkEnvFakeValue) {
		t.Errorf("the render's own output carries the value")
	}
}

func TestRenderK8s_CheckEnvUnsetNameFailsNamingIt(t *testing.T) {
	dir := t.TempDir()
	cfgBody := noObsConfig + "\ncheck_env:\n  - SOME_PASSWORD\n"
	rc, out, _, _, _ := renderK8sRawFrom(t, dir, cfgBody)
	if rc == exitOK {
		t.Fatalf("render-k8s with a declared name missing from the environment must fail: %+v", out)
	}
	if !strings.Contains(out.Error, "SOME_PASSWORD") {
		t.Errorf("the refusal must name the missing variable: %q", out.Error)
	}
}

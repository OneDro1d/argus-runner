package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `argus secrets-scan` (what onboard.sh builds sut-secrets.env from, compose tier)
// lists a declared check_env name, and its output carries NAMES ONLY even when the value is set.
func TestSecretsScan_ListsDeclaredCheckEnvNamesAndNeverAValue(t *testing.T) {
	const fake = "fake-scan-value-2209"
	t.Setenv("SOME_PASSWORD", fake)
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_TOKEN", "runner-test-token")
	cfg := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: p\ntargets:\n  http:\n    base_url: http://sut.invalid:8081\ncheck_env:\n  - SOME_PASSWORD\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := exitErr
	out := captureStdout(t, func() { rc = dispatch([]string{"secrets-scan", "--config", cfg}) })
	if rc != exitOK {
		t.Fatalf("secrets-scan exited %d: %s", rc, out)
	}
	if !strings.Contains(out, `"name": "SOME_PASSWORD"`) && !strings.Contains(out, `"name":"SOME_PASSWORD"`) {
		t.Errorf("secrets-scan did not list the declared name: %s", out)
	}
	if !strings.Contains(out, `"field": "check_env"`) && !strings.Contains(out, `"field":"check_env"`) {
		t.Errorf("secrets-scan did not say the name came from check_env: %s", out)
	}
	if strings.Contains(out, fake) {
		t.Errorf("secrets-scan output carries a value: %s", out)
	}
}

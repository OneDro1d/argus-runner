package main

// (a): the `--obs` mode onboarding chose reaches `argus validate-config` as
// ARGUS_OBS_MODE, the way the tier reaches it as ARGUS_TIER. Run through dispatch(), so the test
// covers env() -> toolcore.ValidateConfig -> the exit code the wrapper's `|| die` reads.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNoPublicURLConfig(t *testing.T) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte("project:\n  name: e13\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestValidateConfig_ObsNoneFromEnvSkipsThePerTierGrafanaCheck(t *testing.T) {
	clearAllTokenEnv(t)
	t.Setenv("ARGUS_TIER", "compose")
	t.Setenv("ARGUS_OBS_MODE", "none")
	var rc int
	out := captureStdout(t, func() {
		rc = dispatch([]string{"validate-config", "--config", writeNoPublicURLConfig(t), "--scenarios", t.TempDir()})
	})
	if rc != exitOK {
		t.Fatalf("ARGUS_OBS_MODE=none must not require public_url on the compose tier (rc=%d):\n%s", rc, out)
	}
	if !strings.Contains(out, `"tier": "skipped-obs-none"`) {
		t.Errorf("the payload must say the tier check was skipped, not passed:\n%s", out)
	}
}

func TestValidateConfig_NoObsModeInEnvStillRefusesAndSaysWhy(t *testing.T) {
	clearAllTokenEnv(t)
	t.Setenv("ARGUS_TIER", "compose")
	t.Setenv("ARGUS_OBS_MODE", "")
	var rc int
	out := captureStdout(t, func() {
		rc = dispatch([]string{"validate-config", "--config", writeNoPublicURLConfig(t), "--scenarios", t.TempDir()})
	})
	if rc == exitOK {
		t.Fatalf("a hand-run validate-config with no mode must still refuse a missing public_url:\n%s", out)
	}
	// The reason is on STDOUT as errors[].message, which is what onboard.sh now prints.
	if !strings.Contains(out, `"message"`) || !strings.Contains(out, "observability.grafana.public_url") {
		t.Errorf("the refusal's reason must be in errors[].message on stdout:\n%s", out)
	}
}

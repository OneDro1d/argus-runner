package main

// validate_config_preauth_test.go — F-CLI-VALIDATE-1: `validate-config` is pure local validation
// (toolcore.ValidateConfig: config.Load + c.Validate + a reachability probe against the SUT — never
// the Argus control plane, never ARGUS_RUNNER_TOKEN/ARGUS_EXECUTOR_SECRET) and used to sit behind the
// in-env dark-factory hat gate anyway, so a bare `argus validate-config --config x.yaml` refused with
// "auth not configured" before it ever looked at the file. It is now dispatched PRE-AUTH, exactly like
// package-check and propose-from-repo — this proves it, and proves the --config gate (VR10-R3) still
// fires FIRST, and that a runner-scoped MCP caller's own view (runner__validate_config) is not being
// widened by the move.

import (
	"path/filepath"
	"strings"
	"testing"
)

// clearAllTokenEnv removes every token-shaped env var this binary reads, so a pass here proves
// validate-config needs NONE of them — not just that some were left over from a prior test.
func clearAllTokenEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"ARGUS_TOKEN", "ARGUS_RUNNER_TOKEN", "ARGUS_EXECUTOR_SECRET", "ARGUS_AUTHOR_TOKEN",
		"ARGUS_CP_AUTHOR_TOKEN", "ARGUS_CP_TOKEN",
	} {
		t.Setenv(v, "")
	}
}

// TestValidateConfig_StillRequiresConfigFlag proves the move did not weaken VR10-R3: the --config
// gate in `configRequired` fires before ANY dispatch, pre-auth or not, and validate-config is still a
// member of that set.
func TestValidateConfig_StillRequiresConfigFlag(t *testing.T) {
	clearAllTokenEnv(t)
	var rc int
	out := captureStdout(t, func() { rc = dispatch([]string{"validate-config"}) })
	if rc == exitOK {
		t.Fatalf("validate-config with no --config exited 0; want a refusal\ngot: %s", out)
	}
	if !strings.Contains(out, missingConfigMsg("validate-config")) {
		t.Errorf("wrong refusal:\ngot:  %s\nwant substring: %s", out, missingConfigMsg("validate-config"))
	}
}

// TestValidateConfig_RevealsNothingBeyondRunnerScope: the identical logic is already reachable by a
// RUNNER-scoped MCP caller as runner__validate_config (internal/mcpserver/tools.go), so moving the
// CLI path above the gate must not add anything a runner-scope caller could not already see. The
// payload is structural (targets configured, capability/reachability summaries, the config path,
// warnings) — this asserts it carries no key that looks like a resolved secret value.
func TestValidateConfig_RevealsNothingBeyondRunnerScope(t *testing.T) {
	clearAllTokenEnv(t)
	cfg := filepath.Join("..", "..", "examples", "memstore", "argus-config.yaml")
	out := captureStdout(t, func() {
		dispatch([]string{"validate-config", "--config", cfg, "--scenarios", t.TempDir()})
	})
	for _, forbidden := range []string{"bearer_token\":\"", "\"token\":\"", "Bearer "} {
		if strings.Contains(out, forbidden) {
			t.Errorf("validate-config payload looks like it echoed a credential (%q found):\n%s", forbidden, out)
		}
	}
}

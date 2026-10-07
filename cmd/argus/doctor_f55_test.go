package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// `argus doctor --config` said "validate-config's checks pass" for a config that
// validate-config WARNS about (a base_url with a path, which the runner drops). The two must agree:
// every warning validate-config prints, doctor prints too, and a config with warnings is never "pass".
func TestDoctor_ConfigWarningsAreTheOnesValidateConfigPrints(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)
	t.Setenv("SYN_TOKEN", "set-here")
	cfg := writeConfig(t, `project:
  name: hub
targets:
  http:
    base_url: http://hub-api:3100/api/v2
  mcp:
    base_url: http://hub-api:8080/mcp
    auth:
      type: bearer
      bearer_token: ${SYN_TOKEN}
`)
	scen := scenariosDir(t)

	payload, _, err := toolcore.ValidateConfig(toolcore.Env{ConfigPath: cfg, ScenariosDir: scen})
	if err != nil {
		t.Fatalf("validate-config: %v", err)
	}
	vcWarnings, _ := payload.(map[string]any)["warnings"].([]string)
	if len(vcWarnings) == 0 {
		t.Fatalf("precondition: validate-config must warn about the base_url path; payload %v", payload)
	}

	rep, _, _ := runDoctor(t, "--config", cfg, "--scenarios", scen, "--control-plane", "https://argus-dev.onedroid.ai")
	c := checkByID(t, rep, "argus-config")
	if c.Status != doctor.StatusWarn {
		t.Errorf("config check status = %q, want warn: validate-config warns, so doctor must not pass it (%s)", c.Status, c.Detail)
	}
	if strings.Contains(c.Detail, "checks pass") {
		t.Errorf("doctor claims validate-config's checks pass while validate-config warns: %s", c.Detail)
	}
	for _, w := range vcWarnings {
		if !strings.Contains(c.Detail, w) {
			t.Errorf("validate-config warns %q but doctor's detail omits it: %s", w, c.Detail)
		}
	}
}

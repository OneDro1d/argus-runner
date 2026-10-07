package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// (a): the executor's runner__validate_config tool answers through
// toolcore.ValidateConfig(env). With the obs mode in that Env (cmd/argus env() reads ARGUS_OBS_MODE)
// an --obs none instance's config with no Grafana URL is valid; without it, it is refused.
func TestDefaultTools_ValidateConfigHonoursObsNone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte("project:\n  name: e13\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scen := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode      string
		wantValid bool
	}{{"none", true}, {"", false}, {"bundled", false}} {
		env := toolcore.Env{Instance: "local", ConfigPath: cfg, ScenariosDir: scen, ResultsRoot: t.TempDir(), Tier: "compose", ObsMode: c.mode}
		var out Outcome
		for _, tl := range DefaultTools(env) {
			if tl.Name == "runner__validate_config" {
				out = tl.Handler(json.RawMessage(`{"instance_id":"local"}`), Principal{})
			}
		}
		m, ok := out.Payload.(map[string]any)
		if !ok {
			t.Fatalf("mode %q: payload is %T, want the validator's map", c.mode, out.Payload)
		}
		if got, _ := m["valid"].(bool); got != c.wantValid {
			t.Errorf("mode %q: runner__validate_config valid=%v, want %v (tier=%v errors=%v error=%v)", c.mode, got, c.wantValid, m["tier"], m["errors"], m["error"])
		}
	}
}

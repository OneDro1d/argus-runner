package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// (a), the executor half: the long-lived executor's own validate_config (the relayed
// verb) must validate the way onboarding did. An instance onboarded with `--obs none` declares no
// observability.grafana.public_url, and its executor must not refuse that config for it.

func relayValidate(t *testing.T, cfg ExecConfig) (valid bool, raw string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := dir + "/argus-config.yaml"
	writeFile(t, cfgPath, "project:\n  name: obs-none-relay\n") // declares NO public_url
	cfg.ToolInstance = "local"
	cfg.ResultsRoot = t.TempDir()
	cfg.ConfigPath = cfgPath
	cfg.ScenariosDir = dir
	got, err := NewCommandFunc(cfg)(context.Background(), "validate_config", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("NewCommandFunc validate_config: %v", err)
	}
	var out struct {
		Valid  bool   `json:"valid"`
		Tier   string `json:"tier"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v (got %s)", err, got)
	}
	// compact on purpose: the full payload carries the capability matrix
	msgs := ""
	for _, e := range out.Errors {
		if len(e.Message) > 90 {
			e.Message = e.Message[:90]
		}
		msgs += " error=" + e.Message
	}
	return out.Valid, "valid=" + map[bool]string{true: "true", false: "false"}[out.Valid] + " tier=" + out.Tier + msgs
}

func TestRelayValidateConfig_ObsNoneDoesNotRequirePublicURL(t *testing.T) {
	for _, tier := range []string{"compose", "k3d", "managed"} {
		valid, raw := relayValidate(t, ExecConfig{Tier: tier, ObsMode: "none"})
		if !valid {
			t.Errorf("%s: an executor of an --obs none instance must answer valid for a config with no Grafana URL; got %s", tier, raw)
		}
		if !strings.Contains(raw, "skipped-obs-none") {
			t.Errorf("%s: the answer must say the tier check was skipped, not passed; got %s", tier, raw)
		}
	}
}

func TestRelayValidateConfig_OtherModesStillRequirePublicURL(t *testing.T) {
	for _, mode := range []string{"", "bundled", "adopt", "export", "shared"} {
		valid, raw := relayValidate(t, ExecConfig{Tier: "compose", ObsMode: mode})
		if valid {
			t.Errorf("mode %q: the relayed validate_config must still refuse a missing public_url; got %s", mode, raw)
		}
		if !strings.Contains(raw, "public_url") {
			t.Errorf("mode %q: the refusal must name public_url; got %s", mode, raw)
		}
	}
}

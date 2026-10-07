package toolcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validate-config echoes the declared check_env NAMES and nothing else about them —
// no value, with the value set in the environment — and a name missing from the environment is a
// refusal that names it.
func TestValidateConfig_CheckEnvEchoesNamesOnly(t *testing.T) {
	const fake = "fake-validate-value-5513"
	t.Setenv("SOME_PASSWORD", fake)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: p\ntargets:\n  http:\n    base_url: http://127.0.0.1:1\ncheck_env:\n  - SOME_PASSWORD\n  - SOME_PASSWORD\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scen := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatal(err)
	}
	payload, _, err := ValidateConfig(Env{ConfigPath: cfg, ScenariosDir: scen})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	b, _ := json.Marshal(payload)
	if !strings.Contains(string(b), `"check_env":["SOME_PASSWORD"]`) {
		t.Errorf("validate-config did not echo the declared name once: %s", b)
	}
	if strings.Contains(string(b), fake) {
		t.Errorf("validate-config output carries a value: %s", b)
	}
}

func TestValidateConfig_CheckEnvUnsetNameIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: p\ntargets:\n  http:\n    base_url: http://127.0.0.1:1\ncheck_env:\n  - SOME_PASSWORD\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ValidateConfig(Env{ConfigPath: cfg, ScenariosDir: dir})
	if err == nil || !strings.Contains(err.Error(), "SOME_PASSWORD (referenced by check_env)") {
		t.Fatalf("an unset declared name must be refused by name, got: %v", err)
	}
}

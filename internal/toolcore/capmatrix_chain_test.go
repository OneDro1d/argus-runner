package toolcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// item 8c (msgbus tester 2026-09-28): "runner__validate_config says valid:false for a correct
// chain-only kit... reports '0 of 8 layers testable' because the kit declares no targets (AMQP
// chain steps don't need any)". These tests write a REAL chain scenario to disk (amqp + http
// steps, each carrying its own connection: url_env / url — never targets.*) against a config that
// declares NO targets block at all, and assert the matrix credits the layers those steps actually
// reach instead of reading every one of them "unavailable".

// writeChainScenario writes a minimal, VALID chain scenario markdown file — the same shape
// internal/scenario's own chainMD test helper produces (package scenario, unexported there).
func writeChainScenario(t *testing.T, dir, id, layer, trigger, expect string) {
	t.Helper()
	body := strings.Join([]string{
		"# Scenario: " + id, "",
		"## Metadata",
		"- **ID**: " + id,
		"- **Layer**: " + layer,
		"- **Tags**: chain", "",
		"## TRIGGER",
		"POST `chain`", "",
		"```json",
		trigger,
		"```", "",
		"## EXPECT",
		expect,
		"## TIMEOUT", "120s", "",
		"## CLEANUP", "N/A — chain-only kit fixture; creates nothing off-SUT.", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const chainOnlyAMQPHTTPTrigger = `{"steps":[` +
	`{"type":"amqp","name":"pub","op":"publish","url_env":"MY_BROKER_URL","exchange":"x","routing_key":"k","body":"hi"},` +
	`{"type":"http","name":"req","method":"GET","url":"http://sut/health"}` +
	`]}`

const chainOnlyAMQPHTTPExpect = "### Runnable\n- step pub: broker accepts\n- step req: status=200\n"

// TestCapabilityMatrix_ChainOnlyKitWithNoTargets_CreditsSelfContainedSteps: the exact symptom —
// a config with NO targets block at all, and a single chain scenario whose amqp+http steps carry
// their own connection. Message Flow (amqp), HTTP Ingestion / Rate Limiting / Permissions / Error
// Path (http) must read available; Database State / External Delivery (no chain step type reaches
// them) stay unavailable; Web UI stays outside-config. And the config stays VALID throughout —
// this is informational only, never a validity change (capmatrix.go's own long-standing rule).
func TestCapabilityMatrix_ChainOnlyKitWithNoTargets_CreditsSelfContainedSteps(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: chainonly\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeChainScenario(t, dir, "CHN-001", "Message Flow", chainOnlyAMQPHTTPTrigger, chainOnlyAMQPHTTPExpect)

	p, failed, err := ValidateConfig(Env{ConfigPath: cfgPath, ScenariosDir: dir})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	m := p.(map[string]any)
	if failed || m["valid"] != true {
		t.Fatalf("a chain-only kit with well-formed steps must stay valid: errors=%v", m["errors"])
	}
	rows, ok := m["capability_matrix"].([]CapabilityRow)
	if !ok {
		t.Fatalf("capability_matrix missing or wrong type: %T", m["capability_matrix"])
	}
	by := map[string]CapabilityRow{}
	for _, r := range rows {
		by[r.Layer] = r
	}
	for _, l := range []string{"Message Flow", "HTTP Ingestion", "Error Path", "Rate Limiting", "Permissions"} {
		if got := by[l].Status; got != CapAvailable {
			t.Errorf("%s: want %s (credited via chain step), got %s (%s)", l, CapAvailable, got, by[l].Reason)
		}
	}
	for _, l := range []string{"Database State", "External Delivery"} {
		if got := by[l].Status; got != CapUnavailable {
			t.Errorf("%s: want %s (no chain step type reaches it), got %s", l, CapUnavailable, got)
		}
	}
	if by["Web UI"].Status != CapOutsideConfig {
		t.Errorf("Web UI: want %s, got %s", CapOutsideConfig, by["Web UI"].Status)
	}
	if r := by["Message Flow"]; !strings.Contains(r.Reason, "CHN-001") || !strings.Contains(r.Reason, "amqp") {
		t.Errorf("Message Flow reason must name the covering scenario and step type: %q", r.Reason)
	}
}

// TestCapabilityMatrix_ChainStepCoverage_NeverMasksARealConfigFault: the matrix crediting a chain
// step must not become a second way to make a genuinely broken config read valid. targets.database
// declares an UNSUPPORTED type (oracle) — a real defect, unrelated to the chain scenario present in
// the same scenarios dir — and validate-config must still refuse it by name.
func TestCapabilityMatrix_ChainStepCoverage_NeverMasksARealConfigFault(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: chainonly\ntargets:\n  database:\n    type: oracle\n    jdbc_url: jdbc:oracle:thin:@db:1521:x\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	writeChainScenario(t, dir, "CHN-002", "Message Flow", chainOnlyAMQPHTTPTrigger, chainOnlyAMQPHTTPExpect)

	p, failed, err := ValidateConfig(Env{ConfigPath: cfgPath, ScenariosDir: dir})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	m := p.(map[string]any)
	if !failed || m["valid"] != false {
		t.Fatalf("an unsupported target type must still refuse the config: valid=%v", m["valid"])
	}
	if !strings.Contains(errorText(m), "oracle") {
		t.Errorf("the real fault must still be named: %s", errorText(m))
	}
	// The chain step credit is still informational, alongside the real fault.
	rows := m["capability_matrix"].([]CapabilityRow)
	for _, r := range rows {
		if r.Layer == "Message Flow" && r.Status != CapAvailable {
			t.Errorf("Message Flow must still be credited via the chain step even though the config is invalid elsewhere: %+v", r)
		}
	}
}

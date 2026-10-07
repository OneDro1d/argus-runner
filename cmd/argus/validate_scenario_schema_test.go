package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §8: "argus validate-scenario --config
// argus-config.yaml run locally with the app's config, so they can check the record against the
// declared schema fully." This is the STANDALONE authoring check item — no broker, no run — as
// opposed to internal/argus/chain_amqp_schema_test.go, which proves the SAME schema is enforced
// again at run time through the whole chain path.

const pingAvscForCmdTest = `{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"},{"name":"seq","type":"long"}]}`

const pingSchemaScenarioMD = `# Scenario: c

## Metadata
- **ID**: CHN-VS
- **Layer**: Permissions
- **Tags**: chain

## TRIGGER
POST ` + "`chain`" + `

` + "```json" + `
{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL","exchange":"lab.inbox","routing_key":"lab.ping","schema":"ping","record":{"id":"abc","seq":%s}}]}
` + "```" + `

## EXPECT
### Runnable
- step spoof: broker accepts
## TIMEOUT
60s

## CLEANUP
N/A — a unit-test fixture; it creates nothing.
`

func writeValidateScenarioConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "project:\n  name: t\ntargets:\n  http:\n    base_url: http://api:8080\n" +
		"message_schemas:\n  ping:\n    inline: '" + pingAvscForCmdTest + "'\n"
	cfg := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func runValidateScenario(t *testing.T, scenarioBody string, extra ...string) (rc int, stdout string) {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "scenario.md")
	if err := os.WriteFile(f, []byte(scenarioBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_TOKEN", "author-test-token")
	args := append([]string{"validate-scenario", "--file", f, "--token", "author-test-token"}, extra...)
	stdout = captureStdout(t, func() { rc = dispatch(args) })
	return rc, stdout
}

func TestValidateScenario_WithConfig_RecordMisfitsSchema_RefusedByFieldPath(t *testing.T) {
	cfg := writeValidateScenarioConfig(t)
	md := strings.Replace(pingSchemaScenarioMD, "%s", `"not-a-number"`, 1)
	rc, stdout := runValidateScenario(t, md, "--config", cfg)
	if rc == exitOK {
		t.Fatalf("a record that misfits its schema must fail validate-scenario --config, got exitOK: %s", stdout)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if v, _ := out["valid"].(bool); v {
		t.Fatalf("valid must be false: %s", stdout)
	}
	if !strings.Contains(stdout, "seq") {
		t.Fatalf("refusal must name field `seq`: %s", stdout)
	}
}

func TestValidateScenario_WithConfig_RecordFitsSchema_Passes(t *testing.T) {
	cfg := writeValidateScenarioConfig(t)
	md := strings.Replace(pingSchemaScenarioMD, "%s", `7`, 1)
	rc, stdout := runValidateScenario(t, md, "--config", cfg)
	if rc != exitOK {
		t.Fatalf("a record that fits its schema must pass validate-scenario --config, got rc=%d: %s", rc, stdout)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if v, _ := out["valid"].(bool); !v {
		t.Fatalf("valid must be true: %s", stdout)
	}
}

func TestValidateScenario_WithoutConfig_BehaviourUnchanged(t *testing.T) {
	// No --config: a record that would misfit its schema is NOT checked (shape-only, design §8 —
	// the control plane's own posture) — behaviour stays exactly what it was before this item.
	md := strings.Replace(pingSchemaScenarioMD, "%s", `"not-a-number"`, 1)
	rc, stdout := runValidateScenario(t, md)
	if rc != exitOK {
		t.Fatalf("without --config, a schema-invalid record must not be checked at all, got rc=%d: %s", rc, stdout)
	}
}

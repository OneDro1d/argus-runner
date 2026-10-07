package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §8: "argus validate-scenario --config
// argus-config.yaml run locally with the app's config, so they can check the record against the
// declared schema fully." CheckScenarioSchemas is the STANDALONE half of that promise — the same
// check runChainScenarioWithMoneyWrites already applies on every run (amqpStepSpec ->
// buildAMQPRecordTemplate -> avroschema.BuildNative in Authoring mode), reachable WITHOUT dialing
// a broker, so `argus validate-scenario --config` can refuse a bad record by field path before any
// run — chain_amqp_schema_test.go already proves the RUN-TIME half through the whole chain path;
// these prove the same schema is enforced with NOTHING run at all.

func TestCheckScenarioSchemas_RecordMisfitsSchema_RefusedByFieldPath(t *testing.T) {
	c := configWithPingSchema(t)
	s := scenario.Parse(chainRunMD("", badSchemaRecordTrigger, "### Runnable\n- step spoof: broker accepts\n"))
	errs := CheckScenarioSchemas(s, c)
	if len(errs) == 0 {
		t.Fatal("a record that does not fit its schema must be refused, with nothing run")
	}
	if !strings.Contains(strings.Join(errs, "; "), "seq") {
		t.Fatalf("refusal must name field `seq`: %v", errs)
	}
}

func TestCheckScenarioSchemas_RecordFitsSchema_NoErrors(t *testing.T) {
	c := configWithPingSchema(t)
	s := scenario.Parse(chainRunMD("", pingSchemaTrigger, "### Runnable\n- step spoof: broker accepts\n"))
	errs := CheckScenarioSchemas(s, c)
	if len(errs) != 0 {
		t.Fatalf("a record that fits its schema must pass with no errors, got %v", errs)
	}
}

func TestCheckScenarioSchemas_UnknownSchemaName_RefusedByName(t *testing.T) {
	c := configWithPingSchema(t)
	trigger := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.ping","schema":"not-declared","record":{"id":"x"}}]}`
	s := scenario.Parse(chainRunMD("", trigger, "### Runnable\n- step spoof: broker accepts\n"))
	errs := CheckScenarioSchemas(s, c)
	if len(errs) == 0 || !strings.Contains(errs[0], "not-declared") {
		t.Fatalf("an undeclared schema name must be refused by name, got %v", errs)
	}
}

func TestCheckScenarioSchemas_NonChainScenario_NoErrors(t *testing.T) {
	c := configWithPingSchema(t)
	// An ordinary (non-chain) scenario carries no amqp steps at all — nothing to check, and this
	// must never panic on a scenario with no Trigger.Payload chain shape.
	md := strings.Join([]string{
		"# Scenario: p", "",
		"## Metadata",
		"- **ID**: PLAIN-1",
		"- **Layer**: Permissions", "",
		"## TRIGGER",
		"GET `/health`", "",
		"## EXPECT",
		"- status is 200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A", "",
	}, "\n")
	s := scenario.Parse(md)
	if errs := CheckScenarioSchemas(s, c); len(errs) != 0 {
		t.Fatalf("a non-chain scenario must never produce schema errors, got %v", errs)
	}
}

func TestCheckScenarioSchemas_NilConfig_NoErrors(t *testing.T) {
	s := scenario.Parse(chainRunMD("", pingSchemaTrigger, "### Runnable\n- step spoof: broker accepts\n"))
	if errs := CheckScenarioSchemas(s, nil); len(errs) != 0 {
		t.Fatalf("a nil config (no --config given) must produce no schema errors, got %v", errs)
	}
}

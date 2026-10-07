package argus

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ── ADVERSARIAL HOLDOUT TESTS (internal/argus) ──────────────────────────────────────────────────
// Written by the VERIFIER against ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28.md, through the
// WHOLE chain path (scenario.Parse -> runChainScenario -> amqpStepSpec -> chain.AMQPStep), exactly
// the same seam internal/argus/chain_amqp_schema_test.go and chain_amqp_test.go already use.

func configWithSchema(t *testing.T, name, schemaJSON string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/argus-config.yaml"
	body := "project:\n  name: t\ntargets:\n  http:\n    base_url: http://api:8080\n" +
		"message_schemas:\n  " + name + ":\n    inline: '" + schemaJSON + "'\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return loaded
}

const h1TwoBranchSchemaJSON = `{"type":"record","name":"Choice","fields":[{"name":"id","type":"string"},{"name":"pick","type":["string","long"]}]}`

// H1 (authoring half) — "a union with 2+ non-null branches written as a bare value → refused by
// field path" — via CheckScenarioSchemas (the standalone authoring check `argus validate-scenario
// --config` runs, design §8), not just internal/avroschema directly.
func TestHoldout_H1_Authoring_TwoBranchUnionBareValue_RefusedByFieldPath(t *testing.T) {
	c := configWithSchema(t, "choice", h1TwoBranchSchemaJSON)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.choice","schema":"choice","record":{"id":"x","pick":"bare"}}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	errs := CheckScenarioSchemas(s, c)
	if len(errs) == 0 {
		t.Fatalf("a bare value against a 2-branch union must be refused at authoring time, got no errors")
	}
	if !strings.Contains(errs[0], "pick") {
		t.Fatalf("refusal must name field path \"pick\", got: %v", errs)
	}
	t.Logf("authoring refusal (expected): %v", errs)
}

// ── H10 — "a publish carries exactly one of `body`, `envelope`, or `schema`+`record`, refused by
// name otherwise. `record` without `schema` (or the reverse) is refused." (design §2, line 46-47)
//
// Each sub-case below builds a chain scenario through scenario.Parse (the SAME function the real
// run path uses to load a scenario from disk — internal/scenario/discover.go's DiscoverFiles calls
// scenario.Parse, NOT scenario.Validate) and runs it through runChainScenario, the actual gate an
// amqp step goes through at RUN time (argus.amqpStepSpec, chain_scenario.go).

func TestHoldout_H10_BodyPlusRecord_WithoutSchema_MustBeRefused(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.h10","body":"hello","record":{"id":"x"}}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(&config.Config{}, s, "tr-h10-body-record", "testkit/ui")
	if res.Status != "failed" {
		t.Errorf("FAIL (defect): a publish step with BOTH `body` and `record` (no `schema`) must be "+
			"refused per design §2 (\"record without schema is refused\"), but runChainScenario "+
			"returned status=%q (want \"failed\"). %d publish(es) reached the fake broker: %+v",
			res.Status, len(f.bodies), res)
		if len(f.bodies) > 0 {
			t.Errorf("the record was silently DROPPED and the literal `body` was sent instead: %q", string(f.bodies[0]))
		}
	}
}

func TestHoldout_H10_EnvelopePlusBody_MustBeRefused(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.h10",` +
		`"envelope":{"to_inbox":"agent.hop","from_agent_id":"argus-test","prompt":"hi"},"body":"hello"}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(&config.Config{}, s, "tr-h10-env-body", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("`envelope` + `body` together must be refused, got status=%q: %+v", res.Status, res)
	}
	if len(f.bodies) != 0 {
		t.Fatalf("a refused combination must never reach the wire, got %d publishes", len(f.bodies))
	}
}

func TestHoldout_H10_RecordWithoutSchema_NoBody_MustBeRefused(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.h10","record":{"id":"x"}}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(&config.Config{}, s, "tr-h10-record-only", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("`record` with no `schema` and no `body`/`envelope` must be refused, got status=%q: %+v", res.Status, res)
	}
	if len(f.bodies) != 0 {
		t.Fatalf("a refused publish must never reach the wire, got %d publishes", len(f.bodies))
	}
}

func TestHoldout_H10_SchemaWithoutRecord_MustBeRefused(t *testing.T) {
	c := configWithSchema(t, "ping2", `{"type":"record","name":"Ping2","fields":[{"name":"id","type":"string"}]}`)
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.h10","schema":"ping2"}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(c, s, "tr-h10-schema-only", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("`schema` with no `record` must be refused, got status=%q: %+v", res.Status, res)
	}
	if len(f.bodies) != 0 {
		t.Fatalf("a refused publish must never reach the wire, got %d publishes", len(f.bodies))
	}
}

// H11 — "a step `headers` key equal to a declaration header (x-envelope-schema) → refused" — on the
// generic `schema`+`record` publish form (not just the `envelope` preset, which the existing suite
// already covers via scenario.Validate's shape-only check).
func TestHoldout_H11_HeaderCollidesWithDeclarationHeader_Refused_SchemaForm(t *testing.T) {
	c := configWithSchema(t, "ping3", `{"type":"record","name":"Ping3","fields":[{"name":"id","type":"string"}]}`)
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.h11","schema":"ping3","record":{"id":"x"},` +
		`"headers":{"X-Envelope-Schema":"corrupt-me"}}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(c, s, "tr-h11-1", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("a custom header colliding with the reserved x-envelope-schema declaration header "+
			"(case-insensitively) must be refused, got status=%q: %+v", res.Status, res)
	}
	if len(f.bodies) != 0 {
		t.Fatalf("a refused publish must never reach the wire, got %d publishes", len(f.bodies))
	}
}

// H4/H5 (report-level half) — the WHOLE report.ScenarioResult, marshalled to JSON exactly as a real
// report would be, must never carry a secret's value; a non-secret value legitimately never appears
// either (it is template-only in the report — the resolved record is never echoed).
func TestHoldout_H4_SecretValue_NeverInFullReportJSON(t *testing.T) {
	const sentinel = "leak-check-9c3f1a"
	t.Setenv("LAB_AUTH_TOKEN", sentinel)
	c := configWithSchema(t, "ping4", `{"type":"record","name":"Ping4","fields":[{"name":"id","type":"string"}]}`)
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.h4","schema":"ping4","record":{"id":"${LAB_AUTH_TOKEN}"}}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(c, s, "tr-h4-1", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("a secret-shaped ${VAR} in a record must be refused, got status=%q", res.Status)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("the secret's VALUE must never appear anywhere in the marshalled report JSON, got:\n%s", raw)
	}
}

func TestHoldout_H5_NonSecretValue_NeverInFullReportJSON_PublishSucceeds(t *testing.T) {
	const sentinel = "visible-report-check-4d2e"
	t.Setenv("LAB_REGION", sentinel)
	c := configWithSchema(t, "ping5", `{"type":"record","name":"Ping5","fields":[{"name":"id","type":"string"}]}`)
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	trig := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
		`"exchange":"lab.inbox","routing_key":"lab.h5","schema":"ping5","record":{"id":"${LAB_REGION}"}}]}`
	s := scenario.Parse(chainRunMD("", trig, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(c, s, "tr-h5-1", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("a non-secret ${VAR} record must publish successfully, got status=%q %+v", res.Status, res.Failure)
	}
	if len(f.bodies) != 1 {
		t.Fatalf("want exactly one publish, got %d", len(f.bodies))
	}
	// The wire body legitimately carries the resolved value (avro-encoded, binary) — the promise is
	// about the REPORT.
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("the report JSON must show the TEMPLATE only, never the resolved value, got:\n%s", raw)
	}
}

package argus

import (
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/config"
	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/hamba/avro/v2"
)

// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 — the generic `schema`+`record` amqp publish/
// consume path, and the `envelope`+`record` override, through the WHOLE chain path (scenario.Parse
// -> runChainScenario -> chain.AMQPStep -> chain.Run), a fake broker behind dialAMQPBroker.

const pingAvscJSON = `{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"},{"name":"seq","type":"long"}]}`

// configWithPingSchema loads a real argus-config.yaml declaring the `ping` schema inline — through
// config.Load (the same path validate-config and a real run take), not a hand-built *Config, so the
// unexported parsed-schema cache (MessageSchemaDecl.validate) is actually populated.
func configWithPingSchema(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/argus-config.yaml"
	body := "project:\n  name: t\ntargets:\n  http:\n    base_url: http://api:8080\n" +
		"message_schemas:\n  ping:\n    inline: '" + pingAvscJSON + "'\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return loaded
}

const pingSchemaTrigger = `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"exchange":"lab.inbox","routing_key":"lab.ping","schema":"ping","record":{"id":"${cid8}","seq":"${now_ms}"}}]}`

func TestAMQPChain_SchemaRecordPublish_NonMsgbusSchema(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	c := configWithPingSchema(t)
	s := scenario.Parse(chainRunMD("", pingSchemaTrigger, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(c, s, "tr-ping-1", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("want passed, got %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if len(f.bodies) != 1 {
		t.Fatalf("want exactly one publish, got %d", len(f.bodies))
	}
	if f.props[0].ContentType != "application/avro" {
		t.Fatalf("content-type: %q", f.props[0].ContentType)
	}
}

const badSchemaRecordTrigger = `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"exchange":"lab.inbox","routing_key":"lab.ping","schema":"ping","record":{"id":"only-a-string","seq":"not-a-number-and-not-a-placeholder"}}]}`

func TestAMQPChain_SchemaRecordPublish_RefusedByFieldPath(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	c := configWithPingSchema(t)
	s := scenario.Parse(chainRunMD("", badSchemaRecordTrigger, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(c, s, "tr-ping-2", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("want failed, got %+v", res)
	}
	if len(f.bodies) != 0 {
		t.Fatalf("a refused record must never reach the wire, got %d publishes", len(f.bodies))
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "seq") {
		t.Fatalf("refusal must name field `seq`: %+v", res.Failure)
	}
}

const secretRecordTrigger = `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"exchange":"lab.inbox","routing_key":"lab.ping","schema":"ping","record":{"id":"${LAB_API_TOKEN}","seq":1}}]}`

func TestAMQPChain_SchemaRecordPublish_SecretRefused(t *testing.T) {
	t.Setenv("LAB_API_TOKEN", "shh-do-not-leak")
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	c := configWithPingSchema(t)
	s := scenario.Parse(chainRunMD("", secretRecordTrigger, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(c, s, "tr-ping-3", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("want failed, got %+v", res)
	}
	if len(f.bodies) != 0 {
		t.Fatalf("a secret-carrying record must never reach the wire, got %d publishes", len(f.bodies))
	}
	if res.Failure == nil || strings.Contains(res.Failure.Observed, "shh-do-not-leak") {
		t.Fatalf("the secret's value must never appear in the failure: %+v", res.Failure)
	}
}

const consumePingTrigger = `{"steps":[{"type":"amqp","name":"read","op":"consume","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"queue":"lab.ping","schema":"ping"}]}`

func TestAMQPChain_ConsumeSchemaDecode(t *testing.T) {
	c := configWithPingSchema(t)
	schema, err := c.MessageSchema("ping")
	if err != nil {
		t.Fatal(err)
	}
	body, err := avro.Marshal(schema.Avro(), map[string]any{"id": "abc", "seq": int64(9)})
	if err != nil {
		t.Fatal(err)
	}
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: body}
	withFakeBroker(t, f)
	s := scenario.Parse(chainRunMD("", consumePingTrigger, "### Runnable\n- step read: body has id equals abc\n"))
	res := runChainScenario(c, s, "tr-ping-4", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("want passed, got %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
}

// slackRefsEnvelopeTrigger is the concrete production blocker the requirements doc opens with: a
// Slack-bridge heartbeat needs `refs`, which the old `envelope` helper hard-coded away.
const slackRefsEnvelopeTrigger = `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"exchange":"msgbus.inbox","routing_key":"inbox.agent.slack",` +
	`"envelope":{"to_inbox":"agent.slack","from_agent_id":"argus-test","prompt":"pulse"},` +
	`"record":{"refs":["slack:chan-example-1"]}}]}`

func TestAMQPChain_EnvelopeRecordOverride_RefsReachTheWire(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	withFakeBroker(t, f)
	s := scenario.Parse(chainRunMD("", slackRefsEnvelopeTrigger, "### Runnable\n- step spoof: broker accepts\n"))
	res := runChainScenario(&config.Config{}, s, "tr-refs-1", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("want passed, got %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	m, err := envelope.DecodeWithHeaders(f.bodies[0], f.props[0].Headers)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(m.Refs) != 1 || m.Refs[0] != "slack:chan-example-1" {
		t.Fatalf("the record.refs override must reach the wire: %#v", m.Refs)
	}
}

package chain

import (
	"encoding/json"
	"strings"
	"testing"

	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
	"github.com/hamba/avro/v2"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/avroschema"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 — the generic schema+record publish/consume
// path, driven against the SAME fake broker test double the rest of this file uses (AC-D18b's own
// convention), so no RabbitMQ is needed here either.

const pingSchemaJSON = `{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"},{"name":"seq","type":"long"}]}`

func mustPingSchema(t *testing.T) *avroschema.Schema {
	t.Helper()
	s, err := avroschema.Parse("ping", "application/avro", nil, pingSchemaJSON)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

// recordTemplateFrom mirrors what internal/argus/chain_scenario.go does to a step's PRISTINE
// `record` JSON before it ever reaches chain.AMQPSpec: parse, then mark whole-value placeholders.
func recordTemplateFrom(t *testing.T, recordJSON string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(recordJSON), &v); err != nil {
		t.Fatalf("record json: %v", err)
	}
	return avroschema.MarkWholeValuePlaceholders(v)
}

func TestAMQPStep_SchemaRecordPublish_NonMsgbusSchema(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	spec := AMQPSpec{
		Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL",
		Exchange: "lab.inbox", RoutingKey: "lab.ping",
		Schema:         mustPingSchema(t),
		RecordTemplate: recordTemplateFrom(t, `{"id":"abc","seq":"${now_ms}"}`),
	}
	sr := runOne(t, spec, accepts, f)
	if sr.Status != "passed" {
		t.Fatalf("want passed, got %+v", sr)
	}
	if len(f.publishes) != 1 {
		t.Fatalf("want exactly one publish, got %d", len(f.publishes))
	}
	call := f.publishes[0]
	if call.props.ContentType != "application/avro" {
		t.Fatalf("content-type: %q", call.props.ContentType)
	}
	var decoded map[string]any
	if err := avro.Unmarshal(mustPingSchema(t).Avro(), call.body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["id"] != "abc" {
		t.Fatalf("id mismatch: %#v", decoded)
	}
}

func TestAMQPStep_SchemaRecordPublish_UUIDFreshPerRun(t *testing.T) {
	schema := mustPingSchema(t)
	run := func() string {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		spec := AMQPSpec{
			Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL",
			Exchange: "lab.inbox", RoutingKey: "lab.ping",
			Schema:         schema,
			RecordTemplate: recordTemplateFrom(t, `{"id":"${uuid}","seq":1}`),
		}
		runOne(t, spec, accepts, f)
		var decoded map[string]any
		if err := avro.Unmarshal(schema.Avro(), f.publishes[0].body, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		id, _ := decoded["id"].(string)
		return id
	}
	id1, id2 := run(), run()
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("expected two distinct fresh uuids, got %q and %q", id1, id2)
	}
}

func TestAMQPStep_SchemaRecordPublish_SecretVarRefusedAtRunTime(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	t.Setenv("MSGBUS_SUPER_SECRET_TOKEN", "shh-do-not-leak")
	spec := AMQPSpec{
		Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL",
		Exchange: "lab.inbox", RoutingKey: "lab.ping",
		Schema:         mustPingSchema(t),
		RecordTemplate: recordTemplateFrom(t, `{"id":"${MSGBUS_SUPER_SECRET_TOKEN}","seq":1}`),
	}
	sr := runOne(t, spec, accepts, f)
	if sr.Status != "failed" {
		t.Fatalf("want failed, got %+v", sr)
	}
	if len(f.publishes) != 0 {
		t.Fatalf("a refused record must never reach the wire, got %d publishes", len(f.publishes))
	}
	if !strings.Contains(sr.Observed, "credential") {
		t.Fatalf("observed should name the reason, got %q", sr.Observed)
	}
	if strings.Contains(sr.Observed, "shh-do-not-leak") {
		t.Fatalf("the secret's VALUE must never appear in the result: %q", sr.Observed)
	}
}

func TestAMQPStep_SchemaRecordPublish_RunTimeRefusalByFieldPath(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	spec := AMQPSpec{
		Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL",
		Exchange: "lab.inbox", RoutingKey: "lab.ping",
		Schema: mustPingSchema(t),
		// seq is a long; ${saved.notseeded} is unresolved (never captured) so this must refuse
		// naming the field, never send a literal placeholder to the broker.
		RecordTemplate: recordTemplateFrom(t, `{"id":"abc","seq":"${saved.notseeded}"}`),
	}
	sr := runOne(t, spec, accepts, f)
	if sr.Status != "failed" {
		t.Fatalf("want failed, got %+v", sr)
	}
	if len(f.publishes) != 0 {
		t.Fatalf("want zero publishes, got %d", len(f.publishes))
	}
	if !strings.Contains(sr.Observed, "seq") {
		t.Fatalf("observed should name the field `seq`, got %q", sr.Observed)
	}
}

func TestAMQPStep_ConsumeSchemaDecode(t *testing.T) {
	schema := mustPingSchema(t)
	body, err := avro.Marshal(schema.Avro(), map[string]any{"id": "abc", "seq": int64(7)})
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: body}
	spec := AMQPSpec{Op: "consume", URLEnv: "MSGBUS_ARGUS_TEST_URL", Queue: "lab.ping", ConsumeSchema: schema}
	want := scenario.AMQPStepWant{Broker: accepts, Body: []mcp.BodyAssert{{Field: "id", Op: mcp.BodyEqualsOp, Value: "abc"}}}
	sr := runOneWant(t, spec, want, f)
	if sr.Status != "passed" {
		t.Fatalf("want passed, got %+v", sr)
	}
}

func TestAMQPStep_ConsumeSchemaDecode_UndecodableIsItsOwnFailure(t *testing.T) {
	schema := mustPingSchema(t)
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("this is not avro at all")}
	spec := AMQPSpec{Op: "consume", URLEnv: "MSGBUS_ARGUS_TEST_URL", Queue: "lab.ping", ConsumeSchema: schema}
	sr := runOneWant(t, spec, scenario.AMQPStepWant{Broker: accepts}, f)
	if sr.Status != "failed" {
		t.Fatalf("want failed, got %+v", sr)
	}
	if !strings.HasPrefix(sr.Observed, "delivered message does not decode as ping:") {
		t.Fatalf("want the distinct decode-failure text, got %q", sr.Observed)
	}
}

func TestAMQPStep_EnvelopeRecordOverride_RefsReachTheWire(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	spec := AMQPSpec{
		Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL",
		Exchange: "msgbus.inbox", RoutingKey: "inbox.agent.slack",
		Envelope:       &AMQPEnvelope{ToInbox: "agent.slack", FromAgentID: "argus-test", Prompt: "pulse"},
		RecordTemplate: recordTemplateFrom(t, `{"refs":["slack:chan-example-1"]}`),
	}
	sr := runOne(t, spec, accepts, f)
	if sr.Status != "passed" {
		t.Fatalf("want passed, got %+v", sr)
	}
	if len(f.publishes) != 1 {
		t.Fatalf("want exactly one publish, got %d", len(f.publishes))
	}
	m, err := envelope.DecodeWithHeaders(f.publishes[0].body, f.publishes[0].props.Headers)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(m.Refs) != 1 || m.Refs[0] != "slack:chan-example-1" {
		t.Fatalf("refs override did not reach the wire: %#v", m.Refs)
	}
}

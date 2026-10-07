package chain

import (
	"os"
	"strings"
	"testing"
	"time"

	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
	"github.com/hamba/avro/v2"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/avroschema"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ── ADVERSARIAL HOLDOUT TESTS ────────────────────────────────────────────────────────────────────
// These were written by the VERIFIER, not the feature's author, against
// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28.md, exercising internal/chain's runtime record
// resolution (record.go) and the amqp step (amqp.go) directly, using the SAME fakeBroker test seam
// amqp_test.go already uses.

const h2h3Schema = `{"type":"record","name":"H2H3","fields":[
  {"name":"id","type":"string"},
  {"name":"uidA","type":"string"},
  {"name":"uidB","type":"string"},
  {"name":"ts","type":"long"},
  {"name":"ts2","type":"long"}
]}`

func mustSchema(t *testing.T, name, schemaJSON string) *avroschema.Schema {
	t.Helper()
	s, err := avroschema.Parse(name, "application/avro", nil, schemaJSON)
	if err != nil {
		t.Fatalf("schema %q must parse: %v", name, err)
	}
	return s
}

func markedTemplate(t *testing.T, raw map[string]any) any {
	t.Helper()
	return avroschema.MarkWholeValuePlaceholders(any(raw))
}

// H2 — "${uuid} twice in one record → two different values; same step in two runs → different
// messageIds."
func TestHoldout_H2_UUIDPerOccurrenceAndPerRun(t *testing.T) {
	schema := mustSchema(t, "h2h3", h2h3Schema)
	tmpl := markedTemplate(t, map[string]any{"id": "x", "uidA": "${uuid}", "uidB": "${uuid}", "ts": 1, "ts2": 1})

	native1, err := buildRecordForRun(schema, tmpl, "run-1", nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	m1 := native1.(map[string]any)
	if m1["uidA"] == m1["uidB"] {
		t.Fatalf("two ${uuid} in ONE record must resolve to two different values, got the same: %v", m1["uidA"])
	}

	native2, err := buildRecordForRun(schema, tmpl, "run-2", nil)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	m2 := native2.(map[string]any)
	if m1["uidA"] == m2["uidA"] {
		t.Fatalf("the SAME step run TWICE must produce DIFFERENT ${uuid} values (messageIds), got the same: %v", m1["uidA"])
	}
}

// H3a — "${now_ms} in a long field → int64 on the wire ... two ${now_ms} in one record → the same
// value."
func TestHoldout_H3a_NowMsIsInt64OnWireAndStableWithinAStep(t *testing.T) {
	schema := mustSchema(t, "h2h3", h2h3Schema)
	tmpl := markedTemplate(t, map[string]any{"id": "x", "uidA": "u", "uidB": "u", "ts": "${now_ms}", "ts2": "${now_ms}"})

	native, err := buildRecordForRun(schema, tmpl, "run-1", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := native.(map[string]any)
	ts, ok := m["ts"].(int64)
	if !ok {
		t.Fatalf("ts must be a native int64 before encoding, got %T (%v)", m["ts"], m["ts"])
	}
	if m["ts"] != m["ts2"] {
		t.Fatalf("two ${now_ms} occurrences in ONE record must resolve to the SAME value, got ts=%v ts2=%v", m["ts"], m["ts2"])
	}

	// Prove int64-on-the-wire: encode + decode through the real Avro codec.
	body, err := avro.Marshal(schema.Avro(), native)
	if err != nil {
		t.Fatalf("avro.Marshal: %v", err)
	}
	var back map[string]any
	if err := avro.Unmarshal(schema.Avro(), body, &back); err != nil {
		t.Fatalf("avro.Unmarshal: %v", err)
	}
	if backTs, ok := back["ts"].(int64); !ok || backTs != ts {
		t.Fatalf("decoded ts must be the same int64 (%d), got %T %v", ts, back["ts"], back["ts"])
	}
}

// H3b — "${now} (RFC3339) in a long field → refused by path at run time."
func TestHoldout_H3b_NowInLongField_RefusedByPathAtRunTime(t *testing.T) {
	schema := mustSchema(t, "h2h3", h2h3Schema)
	tmpl := markedTemplate(t, map[string]any{"id": "x", "uidA": "u", "uidB": "u", "ts": "${now}", "ts2": 1})

	_, err := buildRecordForRun(schema, tmpl, "run-1", nil)
	if err == nil {
		t.Fatalf("${now} (RFC3339 text) in a `long` field must be refused at run time, got no error")
	}
	if !strings.Contains(err.Error(), "record.ts") {
		t.Fatalf("refusal must name the field PATH record.ts, got: %v", err)
	}
	t.Logf("run-time refusal (expected): %v", err)
}

// H4 — "${API_TOKEN}/${db_password}/${SomeKey} in a record → refused (case-insensitive) ... at run
// time; with the env var SET to a sentinel, the sentinel appears nowhere in the report JSON."
func TestHoldout_H4_SecretLikeEnvVar_RefusedAtRunTime_NeverLeaked(t *testing.T) {
	schema := mustSchema(t, "h2h3", h2h3Schema)
	cases := []struct {
		name, varName, sentinel string
	}{
		{"API_TOKEN upper", "API_TOKEN", "shh-token-a1b2"},
		{"db_password lower", "db_password", "shh-pw-c3d4"},
		{"SomeKey mixed-case containing key", "SomeKey", "shh-key-e5f6"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.varName, c.sentinel)
			tmpl := markedTemplate(t, map[string]any{
				"id": "x", "uidA": "${" + c.varName + "}", "uidB": "u", "ts": 1, "ts2": 1,
			})
			_, err := buildRecordForRun(schema, tmpl, "run-1", nil)
			if err == nil {
				t.Fatalf("${%s} must be refused at run time as a secret-shaped name", c.varName)
			}
			if strings.Contains(err.Error(), c.sentinel) {
				t.Fatalf("the env var's VALUE must never appear in the refusal text, got: %v", err)
			}
		})
	}
}

// H5 — "a NON-secret ${VAR} set to \"visible-7e1a\" in a record → publish works, and
// \"visible-7e1a\" appears nowhere in the report JSON (template only)."
func TestHoldout_H5_NonSecretEnvVar_PublishWorks_ValueNeverInReport(t *testing.T) {
	const sentinel = "visible-7e1a"
	t.Setenv("MY_VISIBLE_VAR", sentinel)
	schema := mustSchema(t, "h2h3", h2h3Schema)
	tmpl := markedTemplate(t, map[string]any{
		"id": "x", "uidA": "${MY_VISIBLE_VAR}", "uidB": "u", "ts": 1, "ts2": 1,
	})
	spec := AMQPSpec{Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL", Exchange: "lab.inbox", RoutingKey: "lab.h5",
		Schema: schema, RecordTemplate: tmpl}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	sr := runOne(t, spec, scenario.BrokerClaim{Accepts: true}, f)
	if sr.Status != "passed" {
		t.Fatalf("publish with a non-secret ${VAR} must succeed: %+v", sr)
	}
	// The wire payload legitimately carries the value (it IS the message) — the promise is about the
	// REPORT (Observed / AssertionsEnforced), never the wire body.
	if strings.Contains(sr.Observed, sentinel) {
		t.Fatalf("the report's Observed text must never carry the record's resolved value, got: %q", sr.Observed)
	}
	for _, a := range sr.AssertionsEnforced {
		if strings.Contains(a, sentinel) {
			t.Fatalf("assertions_enforced must never carry the record's resolved value, got: %q", a)
		}
	}
	if len(f.publishes) != 1 {
		t.Fatalf("want exactly one publish, got %d", len(f.publishes))
	}
}

// H6 — "consume with schema on a text body → failure text says \"does not decode\", status
// failed, not a content mismatch."
func TestHoldout_H6_ConsumeSchemaOnTextBody_DoesNotDecode(t *testing.T) {
	schema := mustSchema(t, "h2h3", h2h3Schema)
	spec := AMQPSpec{Op: "consume", URLEnv: "MSGBUS_ARGUS_TEST_URL", Queue: "lab.q", ConsumeSchema: schema}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("plain text, not avro binary")}
	want, werr := scenario.AMQPStepClaims("consume", []string{"body has id equals x"})
	if werr != nil {
		t.Fatalf("building claim: %v", werr)
	}
	sr := runOneWant(t, spec, want, f)
	if sr.Status != "failed" {
		t.Fatalf("a non-avro body against a declared schema must FAIL, got %q: %+v", sr.Status, sr)
	}
	if !strings.Contains(sr.Observed, "does not decode") {
		t.Fatalf("failure text must say \"does not decode\", got: %q", sr.Observed)
	}
	if strings.Contains(sr.Observed, "content assertion") {
		t.Fatalf("a decode failure must NOT be reported as a content mismatch, got: %q", sr.Observed)
	}
}

// H14 — "a non-msgbus schema (e.g. record Ping{id:string}) round-trips publish → consume with no
// msgbus-specific import on that code path."
const pingSchemaForH14 = `{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"},{"name":"seq","type":"long"}]}`

func TestHoldout_H14_NonMsgbusSchema_PublishConsumeRoundTrip(t *testing.T) {
	schema := mustSchema(t, "ping", pingSchemaForH14)
	tmpl := markedTemplate(t, map[string]any{"id": "abc123", "seq": 42})
	pubSpec := AMQPSpec{Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL", Exchange: "lab.inbox", RoutingKey: "lab.ping",
		Schema: schema, RecordTemplate: tmpl}
	pf := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	pubRes := runOne(t, pubSpec, scenario.BrokerClaim{Accepts: true}, pf)
	if pubRes.Status != "passed" || len(pf.publishes) != 1 {
		t.Fatalf("publish must succeed: %+v (publishes=%d)", pubRes, len(pf.publishes))
	}
	published := pf.publishes[0].body

	conSpec := AMQPSpec{Op: "consume", URLEnv: "MSGBUS_ARGUS_TEST_URL", Queue: "lab.ping", ConsumeSchema: schema}
	cf := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: published}
	want, werr := scenario.AMQPStepClaims("consume", []string{"body has id equals abc123"})
	if werr != nil {
		t.Fatalf("building claim: %v", werr)
	}
	conRes := runOneWant(t, conSpec, want, cf)
	if conRes.Status != "passed" {
		t.Fatalf("consume+decode round trip must pass: %+v", conRes)
	}
}

// H14b (static half) — the generic schema+record code path (avroschema + record.go +
// publishSchemaRecord/judgeBroker's decode branch) must not import msgbus; only the `envelope`
// preset (publishEnvelope) is allowed to. The private module is gone (2026-10-05), so the same
// intent now also covers Argus's in-repo copy, internal/msgenvelope: the generic path must not
// import it either.
func TestHoldout_H14b_GenericPathImportsNoMsgbusPackage(t *testing.T) {
	for _, p := range []string{"record.go", "../avroschema/record.go", "../avroschema/schema.go"} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, banned := range []string{"example/msgbus", "internal/msgenvelope"} {
			if strings.Contains(string(b), banned) {
				t.Fatalf("%s must not import the msgbus package or Argus's copy of it (%q) (design: \"any app's wire format is test data\"), but it does", p, banned)
			}
		}
	}
}

// internal/avroschema/record_test.go keeps its own copy of the envelope schema (avroschema must stay
// app-agnostic and cannot import the envelope package). This test keeps that copy honest: its text
// must equal envelope.SchemaJSONV2.
func TestEnvelopeSchemaCopiesAgree(t *testing.T) {
	b, err := os.ReadFile("../avroschema/record_test.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	const start = "const msgbusV2Schema = `"
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatal("msgbusV2Schema const not found in avroschema/record_test.go")
	}
	rest := src[i+len(start):]
	j := strings.Index(rest, "`")
	if j < 0 {
		t.Fatal("msgbusV2Schema const is not terminated")
	}
	if rest[:j] != envelope.SchemaJSONV2 {
		t.Fatal("avroschema/record_test.go msgbusV2Schema differs from envelope.SchemaJSONV2")
	}
}

// H7 — envelope preset: decoded fields equal the PREVIOUS encoder's output (origin/dev, before this
// change) except messageId/enqueuedAt; headers identical (x-envelope-schema=2).
//
// The old internal/chain/amqp.go (git show origin/dev:internal/chain/amqp.go) built the envelope
// with amqpengine.EncodeEnvelope(envelope.Message{Reply: envelope.Reply{Mode: envelope.ReplyNone},
// Refs: []string{}, ...}) directly — reproduced here verbatim (it still compiles: EncodeEnvelope and
// envelope.Message are unchanged) so this test compares REAL old-encoder output to the new step, not
// a description of it.
func oldEncoderReference(t *testing.T, toInbox, fromAgentID, prompt, intentStr, corr string) (body []byte, headers map[string]any) {
	t.Helper()
	intent := envelope.Intent(intentStr)
	if intent == "" {
		intent = envelope.IntentInform
	}
	body, headers, err := amqpengine.EncodeEnvelope(envelope.Message{
		MessageID:     "old-fixed-id-for-test",
		Kind:          envelope.KindMessage,
		Source:        envelope.SourceMCP,
		OriginTrust:   envelope.TrustAgent,
		FromAgentID:   fromAgentID,
		ToInbox:       toInbox,
		Prompt:        prompt,
		Reply:         envelope.Reply{Mode: envelope.ReplyNone},
		Refs:          []string{},
		CorrelationID: corr,
		EnqueuedAt:    time.Unix(0, 0).UTC(),
		Intent:        intent,
	})
	if err != nil {
		t.Fatalf("old encoder reference: %v", err)
	}
	return body, headers
}

func TestHoldout_H7_EnvelopePreset_MatchesOldEncoder_ExceptMessageIDAndEnqueuedAt(t *testing.T) {
	// corr must be the SAME run correlation id runOne's harness (amqp_test.go's runOneWant) actually
	// uses ("tr-amqp-1", hardcoded there) — the envelope's correlationId defaults to the run's cid.
	const toInbox, fromAgentID, prompt, intent, corr = "agent.hop", "argus-test", "argus-test: hello", "ASK", "tr-amqp-1"

	oldBody, oldHeaders := oldEncoderReference(t, toInbox, fromAgentID, prompt, intent, corr)
	oldMsg, err := envelope.DecodeWithHeaders(oldBody, oldHeaders)
	if err != nil {
		t.Fatalf("decode old reference: %v", err)
	}

	spec := AMQPSpec{Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL", Exchange: "msgbus.inbox", RoutingKey: "inbox.agent.hop",
		Envelope: &AMQPEnvelope{ToInbox: toInbox, FromAgentID: fromAgentID, Intent: intent, Prompt: prompt}}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	sr := runOne(t, spec, scenario.BrokerClaim{Accepts: true}, f)
	if sr.Status != "passed" || len(f.publishes) != 1 {
		t.Fatalf("new envelope publish must succeed: %+v", sr)
	}
	newBody := f.publishes[0].body
	newHeaders := f.publishes[0].props.Headers
	newMsg, err := envelope.DecodeWithHeaders(newBody, newHeaders)
	if err != nil {
		t.Fatalf("decode new envelope: %v", err)
	}

	if newMsg.Kind != oldMsg.Kind || newMsg.Source != oldMsg.Source || newMsg.OriginTrust != oldMsg.OriginTrust ||
		newMsg.FromAgentID != oldMsg.FromAgentID || newMsg.ToInbox != oldMsg.ToInbox || newMsg.Prompt != oldMsg.Prompt ||
		newMsg.Reply != oldMsg.Reply || newMsg.CorrelationID != oldMsg.CorrelationID || newMsg.Intent != oldMsg.Intent {
		t.Fatalf("decoded fields must match the old encoder's output (except messageId/enqueuedAt):\nold=%#v\nnew=%#v", oldMsg, newMsg)
	}
	if len(newMsg.Refs) != len(oldMsg.Refs) {
		t.Fatalf("Refs must match (both empty by default): old=%v new=%v", oldMsg.Refs, newMsg.Refs)
	}
	if newMsg.MessageID == oldMsg.MessageID {
		t.Fatalf("messageId is EXPECTED to differ (fresh per publish) — got the same fixed value, fixture bug")
	}

	// Headers identical: x-envelope-schema=2 on both, and no other stray header the old encoder
	// didn't also set.
	if len(newHeaders) != len(oldHeaders) {
		t.Fatalf("headers must match old encoder's set: old=%v new=%v", oldHeaders, newHeaders)
	}
	for k, v := range oldHeaders {
		if newHeaders[k] != v {
			t.Fatalf("header %q: old=%v new=%v", k, v, newHeaders[k])
		}
	}
	if got := newHeaders[envelope.HeaderSchema]; got != envelope.SchemaVersion2 {
		t.Fatalf("x-envelope-schema header must be %q, got %v", envelope.SchemaVersion2, got)
	}
}

var _ = report.StepResult{} // keep import if unused elsewhere in file after edits

// H12 (array half) — "body has a.0 equals x" works on a decoded array (design's own example is
// `refs.0`).
const h12ArraySchema = `{"type":"record","name":"H12Arr","fields":[{"name":"refs","type":{"type":"array","items":"string"}}]}`

func TestHoldout_H12_ConsumeSchema_BodyClaimIndexesDecodedArray(t *testing.T) {
	schema := mustSchema(t, "h12arr", h12ArraySchema)
	native := map[string]any{"refs": []any{"slack:chan-example-1", "second"}}
	body, err := avro.Marshal(schema.Avro(), native)
	if err != nil {
		t.Fatalf("avro.Marshal: %v", err)
	}
	spec := AMQPSpec{Op: "consume", URLEnv: "MSGBUS_ARGUS_TEST_URL", Queue: "lab.arr", ConsumeSchema: schema}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: body}
	want, werr := scenario.AMQPStepClaims("consume", []string{"body has refs.0 equals slack:chan-example-1"})
	if werr != nil {
		t.Fatalf("building claim: %v", werr)
	}
	sr := runOneWant(t, spec, want, f)
	if sr.Status != "passed" {
		t.Fatalf("`body has refs.0 equals ...` must pass against a decoded array, got %+v", sr)
	}
}

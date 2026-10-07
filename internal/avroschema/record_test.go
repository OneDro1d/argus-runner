package avroschema

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hamba/avro/v2"
)

// pingSchema is design's own acceptance-test fixture (design §9 / requirements §3.6): "a trivial
// .avsc round-trips through a lab queue. This proves nothing msgbus-specific is left in the path."
const pingSchema = `{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"}]}`

func mustSchema(t *testing.T, name, schemaJSON string) *Schema {
	t.Helper()
	s, err := Parse(name, "application/avro", nil, schemaJSON)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return s
}

func encodeDecodeJSON(t *testing.T, s *Schema, recordJSON string) map[string]any {
	t.Helper()
	var template any
	if err := json.Unmarshal([]byte(recordJSON), &template); err != nil {
		t.Fatalf("record json: %v", err)
	}
	native, ferr := BuildNative(s, template, Runtime)
	if ferr != nil {
		t.Fatalf("BuildNative: %v", ferr)
	}
	b, err := avro.Marshal(s.Avro(), native)
	if err != nil {
		t.Fatalf("avro.Marshal: %v", err)
	}
	var decoded map[string]any
	if err := avro.Unmarshal(s.Avro(), b, &decoded); err != nil {
		t.Fatalf("avro.Unmarshal: %v", err)
	}
	view, err := jsonValue(s.Avro(), decoded)
	if err != nil {
		t.Fatalf("jsonValue: %v", err)
	}
	m, ok := view.(map[string]any)
	if !ok {
		t.Fatalf("decoded view is not an object: %#v", view)
	}
	return m
}

func TestNonMsgbusSchemaRoundTrip(t *testing.T) {
	s := mustSchema(t, "ping", pingSchema)
	got := encodeDecodeJSON(t, s, `{"id":"abc123"}`)
	if got["id"] != "abc123" {
		t.Fatalf("got %#v", got)
	}
}

// everyTypeSchema exercises design §3's whole table in one record: null/bool/string/int/long/
// float/double/bytes/fixed/enum/array/map/record, a nullable union (bare value), a 2+-branch
// union (tagged), and a field with a default (omitted from the record).
const everyTypeSchema = `{
  "type": "record", "name": "Everything", "namespace": "test",
  "fields": [
    {"name": "n",        "type": "null"},
    {"name": "b",        "type": "boolean"},
    {"name": "s",        "type": "string"},
    {"name": "i",        "type": "int"},
    {"name": "l",        "type": "long"},
    {"name": "f",        "type": "float"},
    {"name": "d",        "type": "double"},
    {"name": "by",       "type": "bytes"},
    {"name": "fx",       "type": {"type": "fixed", "name": "Fx4", "size": 4}},
    {"name": "en",       "type": {"type": "enum", "name": "Color", "symbols": ["RED", "GREEN"]}},
    {"name": "arr",      "type": {"type": "array", "items": "string"}},
    {"name": "mp",       "type": {"type": "map", "values": "long"}},
    {"name": "rec",      "type": {"type": "record", "name": "Inner", "fields": [{"name": "x", "type": "string"}]}},
    {"name": "opt",      "type": ["null", "string"], "default": null},
    {"name": "tagged",   "type": [{"type":"record","name":"A","fields":[{"name":"a","type":"string"}]},
                                    {"type":"record","name":"B","fields":[{"name":"b","type":"string"}]}]},
    {"name": "withDefault", "type": "string", "default": "fallback"}
  ]
}`

func TestEveryTypeRoundTrip(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{
		"n": null, "b": true, "s": "hi", "i": 7, "l": 70000000000, "f": 1.5, "d": 2.5,
		"by": "aGVsbG8=", "fx": "AAAAAQ==", "en": "RED",
		"arr": ["a","b"], "mp": {"x": 1, "y": 2},
		"rec": {"x": "inner"},
		"opt": "present",
		"tagged": {"test.A": {"a": "hello"}}
	}`
	got := encodeDecodeJSON(t, s, record)
	if got["s"] != "hi" {
		t.Fatalf("string mismatch: %#v", got["s"])
	}
	if fmt.Sprint(got["i"]) != "7" {
		t.Fatalf("int mismatch: %#v", got["i"])
	}
	if fmt.Sprint(got["l"]) != "70000000000" {
		t.Fatalf("long mismatch: %#v", got["l"])
	}
	if got["opt"] != "present" {
		t.Fatalf("nullable union (bare value) mismatch: %#v", got["opt"])
	}
	tagged, ok := got["tagged"].(map[string]any)
	if !ok {
		t.Fatalf("tagged union not a map: %#v", got["tagged"])
	}
	if _, ok := tagged["test.A"]; !ok {
		t.Fatalf("tagged union lost its tag: %#v", tagged)
	}
	if got["withDefault"] != "fallback" {
		t.Fatalf("default not applied: %#v", got["withDefault"])
	}
	if got["by"] != "aGVsbG8=" {
		t.Fatalf("bytes not base64: %#v", got["by"])
	}
}

func TestNullableUnionNullBranch(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{"n":null,"b":true,"s":"hi","i":1,"l":1,"f":1,"d":1,"by":"","fx":"AAAAAA==","en":"RED",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}},"withDefault":"w"}`
	got := encodeDecodeJSON(t, s, record)
	if got["opt"] != nil {
		t.Fatalf("expected nil opt, got %#v", got["opt"])
	}
}

func TestAuthoringTimeAcceptsWholeValuePlaceholder(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{"n":null,"b":true,"s":"hi","i":"${saved.n}","l":"${now_ms}","f":1,"d":1,"by":"","fx":"AAAAAA==","en":"RED",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}},"withDefault":"w"}`
	var template any
	if err := json.Unmarshal([]byte(record), &template); err != nil {
		t.Fatal(err)
	}
	marked := MarkWholeValuePlaceholders(template)
	if _, ferr := BuildNative(s, marked, Authoring); ferr != nil {
		t.Fatalf("authoring-time check should accept a whole-value placeholder: %v", ferr)
	}
}

func TestAuthoringTimeRefusesWrongTypeByPath(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{"n":null,"b":true,"s":"hi","i":"not-a-placeholder-and-not-a-number","l":1,"f":1,"d":1,"by":"","fx":"AAAAAA==","en":"RED",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}},"withDefault":"w"}`
	var template any
	if err := json.Unmarshal([]byte(record), &template); err != nil {
		t.Fatal(err)
	}
	marked := MarkWholeValuePlaceholders(template)
	_, ferr := BuildNative(s, marked, Authoring)
	if ferr == nil {
		t.Fatal("expected a refusal")
	}
	if ferr.Path != "i" {
		t.Fatalf("expected refusal on field path %q, got %q (%v)", "i", ferr.Path, ferr)
	}
}

func TestRuntimeRefusesUnknownEnumSymbolByPath(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{"n":null,"b":true,"s":"hi","i":1,"l":1,"f":1,"d":1,"by":"","fx":"AAAAAA==","en":"MAYBE",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}},"withDefault":"w"}`
	var template any
	if err := json.Unmarshal([]byte(record), &template); err != nil {
		t.Fatal(err)
	}
	_, ferr := BuildNative(s, template, Runtime)
	if ferr == nil {
		t.Fatal("expected a refusal")
	}
	if ferr.Path != "en" {
		t.Fatalf("expected refusal on field path %q, got %q", "en", ferr.Path)
	}
	if ferr.Error() != `record.en: "MAYBE" is not a symbol of enum Color` {
		t.Fatalf("unexpected message: %s", ferr.Error())
	}
}

func TestRuntimeRefusesMissingRequiredFieldByPath(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{"n":null,"b":true,"s":"hi","i":1,"l":1,"f":1,"d":1,"by":"","fx":"AAAAAA==","en":"RED",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}}}`
	// withDefault omitted, but it HAS a default so it's fine; drop a REQUIRED field instead (l).
	record = `{"n":null,"b":true,"s":"hi","i":1,"f":1,"d":1,"by":"","fx":"AAAAAA==","en":"RED",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}},"withDefault":"w"}`
	var template any
	if err := json.Unmarshal([]byte(record), &template); err != nil {
		t.Fatal(err)
	}
	_, ferr := BuildNative(s, template, Runtime)
	if ferr == nil {
		t.Fatal("expected a refusal")
	}
	if ferr.Path != "l" {
		t.Fatalf("expected refusal on field path %q, got %q", "l", ferr.Path)
	}
}

func TestWholeValuePlaceholderTypesIntoLong(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{"n":null,"b":true,"s":"hi","i":1,"l":"${now_ms}","f":1,"d":1,"by":"","fx":"AAAAAA==","en":"RED",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}},"withDefault":"w"}`
	var template any
	if err := json.Unmarshal([]byte(record), &template); err != nil {
		t.Fatal(err)
	}
	resolved, _ := WalkStrings(template, func(str string) (any, error) {
		if IsWholeValuePlaceholder(str) {
			return Placeholder("1727463020123"), nil
		}
		return str, nil
	})
	native, ferr := BuildNative(s, resolved, Runtime)
	if ferr != nil {
		t.Fatalf("BuildNative: %v", ferr)
	}
	m := native.(map[string]any)
	if m["l"] != int64(1727463020123) {
		t.Fatalf("expected typed long, got %#v (%T)", m["l"], m["l"])
	}
}

func TestEmbeddedPlaceholderRefusedInNonStringField(t *testing.T) {
	s := mustSchema(t, "everything", everyTypeSchema)
	record := `{"n":null,"b":true,"s":"hi","i":"prefix-${now_ms}-suffix","l":1,"f":1,"d":1,"by":"","fx":"AAAAAA==","en":"RED",
	           "arr":[],"mp":{},"rec":{"x":"y"},"opt":null,"tagged":{"test.B":{"b":"z"}},"withDefault":"w"}`
	var template any
	if err := json.Unmarshal([]byte(record), &template); err != nil {
		t.Fatal(err)
	}
	marked := MarkWholeValuePlaceholders(template)
	_, ferr := BuildNative(s, marked, Authoring)
	if ferr == nil {
		t.Fatal("expected a refusal")
	}
	if ferr.Path != "i" {
		t.Fatalf("expected refusal on field path %q, got %q", "i", ferr.Path)
	}
}

// msgbusV2Schema is a copy of SchemaJSONV2 from internal/msgenvelope, kept here so
// avroschema's own tests exercise the SAME shapes the `envelope` preset (internal/chain,
// design §7) depends on — a 3-branch union with NO null member (reply), an empty record branch
// (NoReply), a long field with logicalType timestamp-millis (enqueuedAt) and a string field with
// logicalType uuid (messageId) — without avroschema importing that package (it must stay
// app-agnostic, so this copy stays a copy). internal/chain's TestEnvelopeSchemaCopiesAgree checks
// the two texts are equal.
const msgbusV2Schema = `{
  "type": "record", "name": "EnvelopeMessage", "namespace": "argus.envelope.v2",
  "fields": [
    {"name": "messageId",     "type": {"type": "string", "logicalType": "uuid"}},
    {"name": "kind",          "type": {"type": "enum", "name": "Kind", "symbols": ["MESSAGE", "EXPIRED_NOTICE"]}},
    {"name": "source",        "type": {"type": "enum", "name": "Source", "symbols": ["MCP", "WEBHOOK", "SLACK", "SYSTEM"]}},
    {"name": "originTrust",   "type": {"type": "enum", "name": "Trust", "symbols": ["AGENT", "EXTERNAL", "SYSTEM"]}},
    {"name": "fromAgentId",   "type": "string"},
    {"name": "toInbox",       "type": "string"},
    {"name": "prompt",        "type": "string"},
    {"name": "reply",         "type": [
        {"type": "record", "name": "NoReply",      "fields": []},
        {"type": "record", "name": "ExpectsReply", "fields": [{"name": "responseId", "type": "string"}]},
        {"type": "record", "name": "ReplyTo",      "fields": [{"name": "responseId", "type": "string"}]}
    ]},
    {"name": "refs",          "type": {"type": "array", "items": "string"}, "default": []},
    {"name": "correlationId", "type": "string"},
    {"name": "enqueuedAt",    "type": {"type": "long", "logicalType": "timestamp-millis"}},
    {"name": "expiredOriginal", "type": ["null", {"type": "record", "name": "ExpiredRef", "fields": [
        {"name": "messageId", "type": "string"},
        {"name": "toInbox", "type": "string"},
        {"name": "expiredForAgentId", "type": "string"},
        {"name": "enqueuedAt", "type": {"type": "long", "logicalType": "timestamp-millis"}},
        {"name": "ttlMs", "type": "long"}
    ]}], "default": null},
    {"name": "intent", "type": {"type": "enum", "name": "Intent", "symbols": ["INFORM", "ASK", "DECISION_REQUEST", "APPROVAL"]}, "default": "INFORM"}
  ]
}`

func TestMsgbusEnvelopeShapeRoundTrip(t *testing.T) {
	s := mustSchema(t, "msgbus-envelope-v2", msgbusV2Schema)
	record := `{
		"messageId": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		"kind": "MESSAGE", "source": "MCP", "originTrust": "AGENT",
		"fromAgentId": "argus-test", "toInbox": "agent.slack", "prompt": "pulse",
		"reply": {"argus.envelope.v2.NoReply": {}},
		"refs": ["slack:chan-example-1"],
		"correlationId": "tr-abc123",
		"enqueuedAt": 1727463020123,
		"intent": "INFORM"
	}`
	got := encodeDecodeJSON(t, s, record)
	if got["fromAgentId"] != "argus-test" {
		t.Fatalf("fromAgentId mismatch: %#v", got)
	}
	reply, ok := got["reply"].(map[string]any)
	if !ok {
		t.Fatalf("reply not tagged map: %#v", got["reply"])
	}
	if _, ok := reply["argus.envelope.v2.NoReply"]; !ok {
		t.Fatalf("reply lost its NoReply tag: %#v", reply)
	}
	refs, ok := got["refs"].([]any)
	if !ok || len(refs) != 1 || refs[0] != "slack:chan-example-1" {
		t.Fatalf("refs mismatch: %#v", got["refs"])
	}
	if fmt.Sprint(got["enqueuedAt"]) != "1727463020123" {
		t.Fatalf("enqueuedAt mismatch: %#v", got["enqueuedAt"])
	}
	if got["expiredOriginal"] != nil {
		t.Fatalf("expiredOriginal should decode nil: %#v", got["expiredOriginal"])
	}
}

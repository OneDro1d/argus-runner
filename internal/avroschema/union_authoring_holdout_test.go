package avroschema

import "testing"

// H1 — ADVERSARIAL HOLDOUT (not written by the feature's author).
// "a union with 2+ non-null branches written as a bare value → refused by field path
// (authoring + run time)" (design §3: a 2+-branch union's JSON form must be tagged
// {"<type>": value}; only a nullable ["null", T] union may be a bare value).

const twoBranchUnionSchema = `{"type":"record","name":"H1","fields":[
  {"name":"id","type":"string"},
  {"name":"choice","type":["string","long"]}
]}`

func TestHoldout_H1_TwoBranchUnion_BareValue_RefusedAtAuthoring(t *testing.T) {
	schema, err := Parse("h1", "application/avro", nil, twoBranchUnionSchema)
	if err != nil {
		t.Fatalf("schema must parse: %v", err)
	}
	// "choice" is written as a bare string, never tagged {"string": "..."}.
	template := map[string]any{"id": "x", "choice": "bare-value"}
	_, ferr := BuildNative(schema, template, Authoring)
	if ferr == nil {
		t.Fatalf("a bare value against a 2-branch union must be refused at authoring, got no error")
	}
	if ferr.Path != "choice" {
		t.Fatalf("refusal must be BY FIELD PATH \"choice\", got path=%q msg=%q", ferr.Path, ferr.Msg)
	}
	t.Logf("authoring refusal (expected): %s", ferr.Error())
}

func TestHoldout_H1_TwoBranchUnion_BareValue_RefusedAtRunTime(t *testing.T) {
	schema, err := Parse("h1", "application/avro", nil, twoBranchUnionSchema)
	if err != nil {
		t.Fatalf("schema must parse: %v", err)
	}
	// Runtime mode: the record is already "resolved" (no placeholders here at all), still a bare
	// value against a 2-branch union.
	template := map[string]any{"id": "x", "choice": "bare-value"}
	_, ferr := BuildNative(schema, template, Runtime)
	if ferr == nil {
		t.Fatalf("a bare value against a 2-branch union must be refused at run time, got no error")
	}
	if ferr.Path != "choice" {
		t.Fatalf("refusal must be BY FIELD PATH \"choice\", got path=%q msg=%q", ferr.Path, ferr.Msg)
	}
	t.Logf("run-time refusal (expected): %s", ferr.Error())
}

// Control: a TAGGED value against the same 2-branch union must be ACCEPTED — proves the refusal
// above is really about "bare", not about the union itself being unusable.
func TestHoldout_H1_TwoBranchUnion_TaggedValue_Accepted(t *testing.T) {
	schema, err := Parse("h1", "application/avro", nil, twoBranchUnionSchema)
	if err != nil {
		t.Fatalf("schema must parse: %v", err)
	}
	template := map[string]any{"id": "x", "choice": map[string]any{"long": float64(7)}}
	native, ferr := BuildNative(schema, template, Runtime)
	if ferr != nil {
		t.Fatalf("a correctly TAGGED value must be accepted, got: %s", ferr.Error())
	}
	m, ok := native.(map[string]any)
	if !ok {
		t.Fatalf("expected a native map, got %T", native)
	}
	choice, ok := m["choice"].(map[string]any)
	if !ok || choice["long"] != int64(7) {
		t.Fatalf("tagged union value not built correctly: %#v", m["choice"])
	}
}

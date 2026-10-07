package avroschema

import (
	"encoding/base64"
	"testing"

	"github.com/hamba/avro/v2"
)

// H12 — "decoded JSON view: bytes → base64, [\"null\",\"string\"] union → bare value, 2-branch union
// → tagged; `body has a.0 equals x` works on a decoded array." (the array-claim half is exercised
// end-to-end in internal/chain's holdout test, against the real body-assertion grammar; this file
// covers ToJSON's own three shape rules directly.)

const h12Schema = `{"type":"record","name":"H12","fields":[
  {"name":"blob","type":"bytes"},
  {"name":"nick","type":["null","string"],"default":null},
  {"name":"pick","type":["string","long"]}
]}`

func TestHoldout_H12_Bytes_DecodeAsBase64(t *testing.T) {
	schema := mustSchemaH12(t)
	native := map[string]any{"blob": []byte{0xDE, 0xAD, 0xBE, 0xEF}, "nick": nil, "pick": map[string]any{"string": "x"}}
	view, err := ToJSON(schema)(native)
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	m := view.(map[string]any)
	want := base64.StdEncoding.EncodeToString([]byte{0xDE, 0xAD, 0xBE, 0xEF})
	if m["blob"] != want {
		t.Fatalf("bytes must decode as base64 %q, got %#v", want, m["blob"])
	}
}

func TestHoldout_H12_NullableUnion_DecodeAsBareValue(t *testing.T) {
	schema := mustSchemaH12(t)
	native := map[string]any{"blob": []byte{1}, "nick": "hello", "pick": map[string]any{"string": "x"}}
	view, err := ToJSON(schema)(native)
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	m := view.(map[string]any)
	if s, ok := m["nick"].(string); !ok || s != "hello" {
		t.Fatalf("[\"null\",\"string\"] must decode as a BARE value \"hello\", got %#v", m["nick"])
	}

	// The null branch too: still bare (nil), never a tagged {"null": ...}.
	native2 := map[string]any{"blob": []byte{1}, "nick": nil, "pick": map[string]any{"string": "x"}}
	view2, err := ToJSON(schema)(native2)
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	m2 := view2.(map[string]any)
	if v, present := m2["nick"]; !present || v != nil {
		t.Fatalf("the null branch must decode as bare nil, got %#v (present=%v)", v, present)
	}
}

func TestHoldout_H12_TwoBranchUnion_DecodeAsTagged(t *testing.T) {
	schema := mustSchemaH12(t)
	native := map[string]any{"blob": []byte{1}, "nick": nil, "pick": map[string]any{"long": int64(9)}}
	view, err := ToJSON(schema)(native)
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}
	m := view.(map[string]any)
	tagged, ok := m["pick"].(map[string]any)
	if !ok {
		t.Fatalf("a 2-branch union must decode TAGGED (a map), got %T: %#v", m["pick"], m["pick"])
	}
	if v, ok := tagged["long"]; !ok || v != int64(9) {
		t.Fatalf("tagged union must carry {\"long\": 9}, got %#v", tagged)
	}
}

func mustSchemaH12(t *testing.T) *Schema {
	t.Helper()
	s, err := Parse("h12", "application/avro", nil, h12Schema)
	if err != nil {
		t.Fatalf("schema parse: %v", err)
	}
	return s
}

var _ = avro.Null // keep import if the avro package becomes otherwise unused

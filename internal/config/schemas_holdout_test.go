package config

import (
	"strings"
	"testing"
)

// ── ADVERSARIAL HOLDOUT (H9) ─────────────────────────────────────────────────────────────────────
// design §1: "`validate-config` parses every declared schema and names the one that fails. An
// unknown `format` is refused, not ignored." Uses the SAME writeSchemaConfig helper
// internal/config/schemas_test.go already defines (same package), so this exercises the real
// config.Load path, not a hand-built struct.

func TestHoldout_H9_SchemaThatFailsToParse_NamedByLoad(t *testing.T) {
	p := writeSchemaConfig(t, nil, `message_schemas:
  broken:
    inline: 'not { valid avro at all'
`)
	_, err := Load(p)
	if err == nil {
		t.Fatalf("a schema that fails to parse must fail config.Load, got no error")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("the error must NAME the failing schema (%q), got: %v", "broken", err)
	}
	t.Logf("refusal (expected): %v", err)
}

func TestHoldout_H9_UnsupportedFormat_Protobuf_Refused(t *testing.T) {
	p := writeSchemaConfig(t, nil, `message_schemas:
  proto1:
    format: protobuf
    inline: 'syntax = "proto3"; message Ping { string id = 1; }'
`)
	_, err := Load(p)
	if err == nil {
		t.Fatalf("format: protobuf must be REFUSED (not ignored/silently accepted), got no error")
	}
	if !strings.Contains(err.Error(), "protobuf") {
		t.Fatalf("the refusal must name the unsupported format, got: %v", err)
	}
	t.Logf("refusal (expected): %v", err)
}

func TestHoldout_H9_BothPathAndInline_Refused(t *testing.T) {
	p := writeSchemaConfig(t, map[string]string{"schemas/ping.avsc": pingAvsc}, `message_schemas:
  ping:
    path: schemas/ping.avsc
    inline: '`+pingAvsc+`'
`)
	_, err := Load(p)
	if err == nil {
		t.Fatalf("declaring BOTH path and inline must be refused, got no error")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("the refusal must explain exactly-one-of, got: %v", err)
	}
	t.Logf("refusal (expected): %v", err)
}

// Control: neither path nor inline declared must ALSO be refused (the same "exactly one" rule, the
// other direction) — otherwise the check above could be an accidental "at least one" check instead.
func TestHoldout_H9_NeitherPathNorInline_Refused(t *testing.T) {
	p := writeSchemaConfig(t, nil, `message_schemas:
  empty1:
    content_type: application/avro
`)
	_, err := Load(p)
	if err == nil {
		t.Fatalf("declaring NEITHER path nor inline must be refused, got no error")
	}
	t.Logf("refusal (expected): %v", err)
}

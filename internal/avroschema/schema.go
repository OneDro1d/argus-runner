// Package avroschema is the generic schema-driven message support for the `amqp` chain step
// (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28.md). It knows Avro (github.com/hamba/avro/v2)
// and the ONE JSON record rule (design §3); it knows nothing about msgbus, chain steps or
// argus-config — those live in internal/config (where a schema is DECLARED) and internal/chain
// (where a schema is USED by a publish/consume step). Kept separate so "any app's wire format is
// test data" (the requirement's own words) is actually true of the code, not just the docs.
package avroschema

import (
	"fmt"

	"github.com/hamba/avro/v2"
)

// Schema is one parsed message schema: the app's declared name, its wire content-type and the
// declaration headers Argus sets on every publish (design §1), plus the parsed Avro schema itself.
type Schema struct {
	Name        string
	ContentType string
	Headers     map[string]string
	avro        avro.Schema
}

// Parse parses schemaJSON (an Avro schema document, whether it came from a `path:` file or an
// `inline:` string) under name, wrapping the parse error so a caller can name the failing
// declaration (validate-config's "reports a schema that fails to parse by name", design §1).
func Parse(name, contentType string, headers map[string]string, schemaJSON string) (*Schema, error) {
	s, err := avro.Parse(schemaJSON)
	if err != nil {
		return nil, fmt.Errorf("message schema %q does not parse as avro: %w", name, err)
	}
	rs, ok := s.(*avro.RecordSchema)
	if !ok {
		return nil, fmt.Errorf("message schema %q: top-level schema must be a record (got %s)", name, s.Type())
	}
	_ = rs
	return &Schema{Name: name, ContentType: contentType, Headers: headers, avro: s}, nil
}

// Avro returns the parsed avro.Schema, for a caller that needs the library type directly
// (avro.Marshal/avro.Unmarshal) — kept unexported-by-convention (callers should prefer Encode/
// Decode/BuildRecord below) but reachable, since chain.go's envelope preset parses msgbus's own
// SchemaJSONV2 through this same Parse and needs nothing else from it.
func (s *Schema) Avro() avro.Schema { return s.avro }

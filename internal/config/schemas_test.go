package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 §1: `message_schemas` in argus-config.yaml.

func writeSchemaConfig(t *testing.T, extraFiles map[string]string, block string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range extraFiles {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := "project:\n  name: t\ntargets:\n  http:\n    base_url: http://api:8080\n" + block
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const pingAvsc = `{"type":"record","name":"Ping","fields":[{"name":"id","type":"string"}]}`

func TestMessageSchemas_PathLoadedRelativeToConfig(t *testing.T) {
	p := writeSchemaConfig(t, map[string]string{"schemas/ping.avsc": pingAvsc}, `message_schemas:
  ping:
    format: avro
    path: schemas/ping.avsc
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s, err := c.MessageSchema("ping")
	if err != nil {
		t.Fatalf("MessageSchema: %v", err)
	}
	if s.Name != "ping" || s.ContentType != "application/avro" {
		t.Fatalf("unexpected schema: %+v", s)
	}
	files := c.MessageSchemaFiles()
	if string(files["schemas/ping.avsc"]) != pingAvsc {
		t.Fatalf("MessageSchemaFiles did not carry the file's content: %q", files["schemas/ping.avsc"])
	}
}

func TestMessageSchemas_InlineNeedsNoFile(t *testing.T) {
	p := writeSchemaConfig(t, nil, "message_schemas:\n  ping:\n    inline: '"+strings.ReplaceAll(pingAvsc, "'", "")+"'\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := c.MessageSchema("ping"); err != nil {
		t.Fatalf("MessageSchema: %v", err)
	}
	if len(c.MessageSchemaFiles()) != 0 {
		t.Fatalf("an inline schema needs no embedded file: %+v", c.MessageSchemaFiles())
	}
}

func TestMessageSchemas_ParseFailureNamedByKey(t *testing.T) {
	p := writeSchemaConfig(t, map[string]string{"schemas/bad.avsc": `{"type":"record","name":"Bad","fields":[{"name":"id"}]}`},
		"message_schemas:\n  bad-one:\n    path: schemas/bad.avsc\n")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "bad-one") {
		t.Fatalf("refusal must name the schema by key %q: %v", "bad-one", err)
	}
}

func TestMessageSchemas_ExactlyOnePathOrInline(t *testing.T) {
	p := writeSchemaConfig(t, map[string]string{"schemas/ping.avsc": pingAvsc},
		"message_schemas:\n  ping:\n    path: schemas/ping.avsc\n    inline: '{}'\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("declaring both path and inline must be refused; got %v", err)
	}

	p2 := writeSchemaConfig(t, nil, "message_schemas:\n  ping:\n    format: avro\n")
	_, err2 := Load(p2)
	if err2 == nil || !strings.Contains(err2.Error(), "exactly one of") {
		t.Fatalf("declaring neither path nor inline must be refused; got %v", err2)
	}
}

func TestMessageSchemas_UnknownFormatRefused(t *testing.T) {
	p := writeSchemaConfig(t, nil, "message_schemas:\n  ping:\n    format: protobuf\n    inline: '{}'\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `"protobuf"`) {
		t.Fatalf("an unsupported format must be refused by value; got %v", err)
	}
}

func TestMessageSchemas_MissingFileRefused(t *testing.T) {
	p := writeSchemaConfig(t, nil, "message_schemas:\n  ping:\n    path: schemas/nope.avsc\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "schemas/nope.avsc") {
		t.Fatalf("a missing declared file must be refused by path; got %v", err)
	}
}

func TestMessageSchemas_InvalidNameRefused(t *testing.T) {
	p := writeSchemaConfig(t, nil, "message_schemas:\n  Not_Valid:\n    inline: '{}'\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "Not_Valid") {
		t.Fatalf("an invalid schema name must be refused by name; got %v", err)
	}
}

func TestMessageSchemas_UnknownNameRefused(t *testing.T) {
	p := writeSchemaConfig(t, map[string]string{"schemas/ping.avsc": pingAvsc}, `message_schemas:
  ping:
    path: schemas/ping.avsc
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := c.MessageSchema("not-declared"); err == nil {
		t.Fatal("expected a refusal naming the unknown schema")
	}
}

func TestMessageSchemas_AbsentBlockIsFine(t *testing.T) {
	p := writeSchemaConfig(t, nil, "")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := c.MessageSchema("anything"); err == nil {
		t.Fatal("expected a refusal (no message_schemas declared at all)")
	}
}

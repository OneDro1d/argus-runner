package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/avroschema"
)

// ── message_schemas (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28, design §1) ──────────────────
//
// A top-level `message_schemas:` block declares the wire schemas an `amqp` chain step's `schema` +
// `record` (publish) or `schema` (consume) may reference by name — the app's message fields become
// test data in a scenario, never Argus code (the requirement's own words). Optional; when present,
// every entry is checked at LOAD time (the same "load time IS validate time" rule package.go and
// rate_limit.go already follow) — a schema that fails to parse is refused BY NAME here, which is
// what makes `validate-config` catch it before any run does.

// MessageSchemaNamePattern is the only shape a message_schemas key may take (design §1).
const MessageSchemaNamePattern = `^[a-z0-9][a-z0-9-]*$`

var messageSchemaNameRe = regexp.MustCompile(MessageSchemaNamePattern)

// messageSchemaFormats lists the `format:` values this release supports. Design §1: "only avro in
// this release; protobuf / json later (R7)" — an unknown format is refused, not ignored.
var messageSchemaFormats = map[string]bool{"avro": true}

// MessageSchemaDecl is one entry under `message_schemas.<name>` (design §1).
type MessageSchemaDecl struct {
	Format string `yaml:"format"`
	// Exactly one of Path (relative to the config file) or Inline.
	Path        string            `yaml:"path"`
	Inline      string            `yaml:"inline"`
	ContentType string            `yaml:"content_type"`
	Headers     map[string]string `yaml:"headers"`

	// parsed and raw are filled in by validate(); never (un)marshalled.
	parsed *avroschema.Schema `yaml:"-"`
	raw    []byte             `yaml:"-"`
}

// MessageSchemas is the top-level `message_schemas:` map.
type MessageSchemas map[string]*MessageSchemaDecl

// validate parses and checks every declared schema, relative to root (the config file's
// directory) — the same load-time rule Package.validate and RateLimit.validate follow. A nil map
// (none declared) is fine.
func (m MessageSchemas) validate(root string) error {
	if len(m) == 0 {
		return nil
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []string
	for _, name := range names {
		if err := m[name].validate(root, name); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(errs, "; "))
}

func (d *MessageSchemaDecl) validate(root, name string) error {
	if !messageSchemaNameRe.MatchString(name) {
		return fmt.Errorf("message_schemas: %q is not a valid schema name (must match %s)", name, MessageSchemaNamePattern)
	}
	label := "message_schemas." + name
	hasPath := strings.TrimSpace(d.Path) != ""
	hasInline := strings.TrimSpace(d.Inline) != ""
	if hasPath == hasInline {
		return fmt.Errorf("%s: declare exactly one of `path` or `inline`", label)
	}
	format := strings.ToLower(strings.TrimSpace(d.Format))
	if format == "" {
		format = "avro"
	}
	if !messageSchemaFormats[format] {
		return fmt.Errorf("%s: format %q is not supported (only \"avro\" in this release)", label, d.Format)
	}
	d.Format = format
	for k := range d.Headers {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%s.headers: a header name may not be empty", label)
		}
	}
	var schemaJSON []byte
	if hasPath {
		clean, e := declaredPath(root, name+".path", d.Path)
		if e != "" {
			return fmt.Errorf("%s", strings.Replace(e, "package."+name+".path", label, 1))
		}
		d.Path = clean
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(clean)))
		if err != nil {
			return fmt.Errorf("%s: %q does not exist under %s", label, d.Path, root)
		}
		schemaJSON = b
	} else {
		schemaJSON = []byte(d.Inline)
	}
	contentType := d.ContentType
	if contentType == "" {
		contentType = "application/avro"
	}
	d.ContentType = contentType
	parsed, err := avroschema.Parse(name, contentType, d.Headers, string(schemaJSON))
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	d.parsed = parsed
	d.raw = schemaJSON
	return nil
}

// MessageSchema returns the parsed schema declared under name, or an error naming it — the lookup
// an amqp step's `schema:` reference resolves through (design §2).
func (c *Config) MessageSchema(name string) (*avroschema.Schema, error) {
	d, ok := c.MessageSchemas[name]
	if !ok || d == nil {
		known := make([]string, 0, len(c.MessageSchemas))
		for k := range c.MessageSchemas {
			known = append(known, k)
		}
		sort.Strings(known)
		if len(known) == 0 {
			return nil, fmt.Errorf("no message_schemas declared in argus-config.yaml — declare %q under message_schemas to use it", name)
		}
		return nil, fmt.Errorf("message_schemas has no entry %q (declared: %s)", name, strings.Join(known, ", "))
	}
	if d.parsed == nil {
		return nil, fmt.Errorf("message_schemas.%s was never validated (internal error)", name)
	}
	return d.parsed, nil
}

// MessageSchemaFiles returns every declared `path:` schema's cleaned relative path and raw file
// bytes — what render-k8s (and the compose render) embed into the instance ConfigMap alongside
// argus-config.yaml (design §1). An `inline:` schema needs no file and is omitted.
func (c *Config) MessageSchemaFiles() map[string][]byte {
	if len(c.MessageSchemas) == 0 {
		return nil
	}
	out := map[string][]byte{}
	for _, d := range c.MessageSchemas {
		if d == nil || d.Path == "" {
			continue
		}
		out[d.Path] = d.raw
	}
	return out
}

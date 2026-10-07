package avroschema

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hamba/avro/v2"
)

// fixedBytes turns a []byte of the declared size into the Go [N]byte ARRAY value
// github.com/hamba/avro/v2 requires for a `fixed` field (codec_fixed.go only accepts
// reflect.Array, never a slice) — built via reflection since N is only known at schema-parse time.
func fixedBytes(b []byte, size int) any {
	arrType := reflect.ArrayOf(size, reflect.TypeOf(byte(0)))
	v := reflect.New(arrType).Elem()
	reflect.Copy(v, reflect.ValueOf(b))
	return v.Interface()
}

// bytesFromFixed is fixedBytes's inverse: avro.Unmarshal hands a `fixed` field back as a Go [N]byte
// array (boxed as any), never a slice — read it back with reflection regardless of N.
func bytesFromFixed(v any) []byte {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Array {
		return nil
	}
	out := make([]byte, rv.Len())
	reflect.Copy(reflect.ValueOf(out), rv)
	return out
}

// FieldError is a record refusal BY FIELD PATH (design §2: "a misfit is refused by field path, e.g.
// `record.reply.mode: "MAYBE" is not a symbol of enum ReplyMode`"). Path is dotted from the record
// root, WITHOUT a leading "record." — Error() adds that prefix, once, so every caller's message reads
// the same way regardless of how deep the path-building recursion is.
type FieldError struct {
	Path string
	Msg  string
}

func (e *FieldError) Error() string {
	if e.Path == "" {
		return "record: " + e.Msg
	}
	return "record." + e.Path + ": " + e.Msg
}

func fieldErrf(path, format string, a ...any) *FieldError {
	return &FieldError{Path: path, Msg: fmt.Sprintf(format, a...)}
}

func joinPath(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

// placeholder marks a string leaf that was EXACTLY ONE placeholder token in the JSON record (design
// §4: "a JSON string that is exactly one placeholder may stand in a non-string field"). Resolve (in
// internal/chain) produces this wrapper instead of a plain string for such a leaf, so BuildNative
// (below) knows it may parse the resolved text into a non-string field's type — a plain string
// leaf is never eligible for that, whatever it contains (design: "a placeholder inside a longer
// string stays text and is valid only in a string field").
type placeholder string

// Token returns the wrapped text — the ORIGINAL `${…}` token before resolution
// (MarkWholeValuePlaceholders) or the RESOLVED value after it (a run-time resolver re-wraps with
// Placeholder once it has resolved a token, internal/chain/record.go). Exported as an interface
// method (rather than the unexported type) so another package can recognise a Placeholder leaf via
// a type assertion to `interface{ Token() string }` without avroschema exporting its concrete type.
func (p placeholder) Token() string { return string(p) }

// Placeholder wraps a resolved leaf value as a whole-value placeholder — exported so
// internal/chain's resolver (which walks the same template tree) can mark a leaf without either
// package reaching into the other's unexported types.
func Placeholder(resolved string) any { return placeholder(resolved) }

// IsPlaceholder reports whether v is a leaf Resolve marked as a whole-value placeholder, and
// returns its resolved text.
func IsPlaceholder(v any) (string, bool) {
	p, ok := v.(placeholder)
	return string(p), ok
}

// Mode selects what BuildNative is checking: the unresolved TEMPLATE (authoring time — a
// placeholder stands for "a valid value of the field's type", design §4/§8) or the fully RESOLVED
// record (run time — a placeholder's resolved text must actually parse as that type).
type Mode int

const (
	// Authoring: template-shape check only. A placeholder leaf (of either form) is accepted
	// wherever a string field.
	Authoring Mode = iota
	// Runtime: every placeholder leaf's resolved text is parsed into the field's real type; the
	// return value is the avro-native tree BuildNative built, ready for avro.Marshal.
	Runtime
)

// BuildNative walks template (a generic JSON value — map[string]any/[]any/string/float64/bool/nil,
// as encoding/json decodes it, with whole-value placeholder leaves wrapped by Placeholder) against
// schema and produces the Go-native value tree github.com/hamba/avro/v2 requires for
// avro.Marshal(schema.Avro(), native) — applying the ONE JSON record rule (design §3) and the
// placeholder typing rule (design §4) as it goes. In Authoring mode the returned native value is
// meaningless (never encoded); only the error matters.
func BuildNative(schema *Schema, template any, mode Mode) (any, *FieldError) {
	return buildValue(schema.avro, template, "", mode)
}

func buildValue(s avro.Schema, v any, path string, mode Mode) (any, *FieldError) {
	s = deref(s)

	// A whole-value placeholder leaf: authoring accepts it unconditionally ("stands for a valid
	// value of the field's type"); runtime parses its resolved text into s's type.
	if ph, ok := IsPlaceholder(v); ok {
		if mode == Authoring {
			return nil, nil
		}
		return typedFromString(s, ph, path)
	}

	// design §4: "a placeholder inside a longer string stays text and is valid only in a string
	// field" — a PLAIN string (not whole-value, so not eligible for typing) carrying a `${…}` token
	// against a non-string/non-enum/non-bytes/non-fixed field is refused with that reason, rather
	// than the generic "expected a string" a bare type mismatch would give.
	if str, ok := v.(string); ok && strings.Contains(str, "${") {
		if _, isStrLike := stringLikeType(s); !isStrLike {
			return nil, fieldErrf(path, "carries a placeholder embedded in text, which is only valid in a string field (this field is %s) — use a JSON string that is EXACTLY one placeholder to fill a non-string field", s.Type())
		}
	}

	switch t := s.(type) {
	case *avro.NullSchema:
		if v != nil {
			return nil, fieldErrf(path, "expected null, got %s", jsonKind(v))
		}
		return nil, nil
	case *avro.UnionSchema:
		return buildUnion(t, v, path, mode)
	case *avro.RecordSchema:
		return buildRecordValue(t, v, path, mode)
	case *avro.ArraySchema:
		return buildArray(t, v, path, mode)
	case *avro.MapSchema:
		return buildMap(t, v, path, mode)
	case *avro.EnumSchema:
		str, ok := v.(string)
		if !ok {
			return nil, fieldErrf(path, "expected a string enum symbol, got %s", jsonKind(v))
		}
		if !containsStr(t.Symbols(), str) {
			return nil, fieldErrf(path, "%q is not a symbol of enum %s", str, t.Name())
		}
		return str, nil
	case *avro.FixedSchema:
		b, err := base64.StdEncoding.DecodeString(asString(v))
		if err != nil {
			return nil, fieldErrf(path, "expected base64 bytes (fixed, size %d): %s", t.Size(), err)
		}
		if len(b) != t.Size() {
			return nil, fieldErrf(path, "expected %d bytes (fixed), got %d", t.Size(), len(b))
		}
		return fixedBytes(b, t.Size()), nil
	case *avro.PrimitiveSchema:
		return buildPrimitive(t, v, path)
	default:
		return nil, fieldErrf(path, "unsupported schema node %s", s.Type())
	}
}

// stringLikeType reports whether a plain JSON string is ever a legal value for s (directly, or —
// for a nullable union — for its one non-null branch): String, Enum (a symbol) and Bytes/Fixed
// (base64) are all spelled as JSON strings (design §3).
func stringLikeType(s avro.Schema) (avro.Schema, bool) {
	s = deref(s)
	if u, ok := s.(*avro.UnionSchema); ok {
		branches := nonNullBranches(u)
		if len(branches) == 1 {
			return stringLikeType(branches[0])
		}
		return nil, false
	}
	switch s.(type) {
	case *avro.EnumSchema, *avro.FixedSchema:
		return s, true
	}
	if p, ok := s.(*avro.PrimitiveSchema); ok && (p.Type() == avro.String || p.Type() == avro.Bytes) {
		return s, true
	}
	return nil, false
}

func deref(s avro.Schema) avro.Schema {
	if s.Type() == avro.Ref {
		return s.(*avro.RefSchema).Schema()
	}
	return s
}

func buildPrimitive(t *avro.PrimitiveSchema, v any, path string) (any, *FieldError) {
	switch t.Type() {
	case avro.Null:
		if v != nil {
			return nil, fieldErrf(path, "expected null, got %s", jsonKind(v))
		}
		return nil, nil
	case avro.Boolean:
		b, ok := v.(bool)
		if !ok {
			return nil, fieldErrf(path, "expected boolean, got %s", jsonKind(v))
		}
		return b, nil
	case avro.Int:
		n, ok := jsonNumber(v)
		if !ok {
			return nil, fieldErrf(path, "expected an integer (int), got %s", jsonKind(v))
		}
		return int32(n), nil
	case avro.Long:
		n, ok := jsonNumber(v)
		if !ok {
			return nil, fieldErrf(path, "expected an integer (long), got %s", jsonKind(v))
		}
		return int64(n), nil
	case avro.Float:
		n, ok := jsonNumber(v)
		if !ok {
			return nil, fieldErrf(path, "expected a number (float), got %s", jsonKind(v))
		}
		return float32(n), nil
	case avro.Double:
		n, ok := jsonNumber(v)
		if !ok {
			return nil, fieldErrf(path, "expected a number (double), got %s", jsonKind(v))
		}
		return n, nil
	case avro.String:
		str, ok := v.(string)
		if !ok {
			return nil, fieldErrf(path, "expected a string, got %s", jsonKind(v))
		}
		return str, nil
	case avro.Bytes:
		str, ok := v.(string)
		if !ok {
			return nil, fieldErrf(path, "expected base64 bytes, got %s", jsonKind(v))
		}
		b, err := base64.StdEncoding.DecodeString(str)
		if err != nil {
			return nil, fieldErrf(path, "expected base64 bytes: %s", err)
		}
		return b, nil
	default:
		return nil, fieldErrf(path, "unsupported primitive type %s", t.Type())
	}
}

// typedFromString parses a placeholder's RESOLVED text into s's Avro type (design §4's typing
// rule) — the run-time twin of buildPrimitive, but reading a string rather than a decoded JSON
// value, because the source was always "exactly one placeholder" text.
func typedFromString(s avro.Schema, text, path string) (any, *FieldError) {
	switch t := s.(type) {
	case *avro.UnionSchema:
		// A whole-value placeholder resolving into a union is only sensible against a nullable
		// (single non-null branch) union — a 2+-branch union has no way to name its branch from a
		// bare string, so the record must tag it explicitly (design §3) and put the placeholder
		// INSIDE the tagged value instead.
		_, idx := t.Indices()
		if idx < 0 {
			return nil, fieldErrf(path, "a placeholder cannot resolve into a union with no non-null branch")
		}
		branches := nonNullBranches(t)
		if len(branches) != 1 {
			return nil, fieldErrf(path, "a whole-value placeholder cannot select a branch of a union with %d non-null types — tag it explicitly, e.g. {\"<type>\": \"${…}\"}", len(branches))
		}
		if text == "" {
			return nil, nil
		}
		return typedFromString(branches[0], text, path)
	case *avro.EnumSchema:
		if !containsStr(t.Symbols(), text) {
			return nil, fieldErrf(path, "%q is not a symbol of enum %s", text, t.Name())
		}
		return text, nil
	case *avro.FixedSchema:
		b, err := base64.StdEncoding.DecodeString(text)
		if err != nil || len(b) != t.Size() {
			return nil, fieldErrf(path, "resolved value is not %d bytes of base64 (fixed)", t.Size())
		}
		return fixedBytes(b, t.Size()), nil
	case *avro.PrimitiveSchema:
		switch t.Type() {
		case avro.Null:
			if text != "" {
				return nil, fieldErrf(path, "resolved value is not null")
			}
			return nil, nil
		case avro.Boolean:
			b, err := strconv.ParseBool(text)
			if err != nil {
				return nil, fieldErrf(path, "resolved value is not a valid boolean")
			}
			return b, nil
		case avro.Int:
			n, err := strconv.ParseInt(text, 10, 32)
			if err != nil {
				return nil, fieldErrf(path, "resolved value is not a valid int")
			}
			return int32(n), nil
		case avro.Long:
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, fieldErrf(path, "resolved value is not a valid long")
			}
			return n, nil
		case avro.Float:
			n, err := strconv.ParseFloat(text, 32)
			if err != nil {
				return nil, fieldErrf(path, "resolved value is not a valid float")
			}
			return float32(n), nil
		case avro.Double:
			n, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, fieldErrf(path, "resolved value is not a valid double")
			}
			return n, nil
		case avro.String:
			return text, nil
		case avro.Bytes:
			b, err := base64.StdEncoding.DecodeString(text)
			if err != nil {
				return nil, fieldErrf(path, "resolved value is not valid base64 (bytes)")
			}
			return b, nil
		}
	}
	return nil, fieldErrf(path, "a whole-value placeholder cannot resolve into %s", s.Type())
}

func buildUnion(t *avro.UnionSchema, v any, path string, mode Mode) (any, *FieldError) {
	branches := nonNullBranches(t)
	if v == nil {
		if !t.Nullable() && len(t.Types()) > 0 && t.Types()[0].Type() != avro.Null {
			// still allow: some unions have null not first; Nullable() covers "contains null".
		}
		if !containsNull(t) {
			return nil, fieldErrf(path, "null is not a member of this union")
		}
		return nil, nil
	}
	if len(branches) == 1 {
		// Design §3: `["null", T]` — bare value, never tagged.
		return buildValue(branches[0], v, path, mode)
	}
	// 2+ non-null branches: the JSON value MUST be tagged {"<branch>": value} (design §3), unless
	// it is a whole-value placeholder (handled by the caller before reaching here) or itself a
	// map carrying exactly one key naming a branch.
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		names := make([]string, 0, len(branches))
		for _, b := range branches {
			names = append(names, branchName(b))
		}
		return nil, fieldErrf(path, "a union of %d non-null types must be tagged as {\"<type>\": value} (one of %s)", len(branches), strings.Join(names, ", "))
	}
	for k, val := range m {
		for _, b := range branches {
			if branchName(b) == k {
				out, err := buildValue(b, val, joinPath(path, k), mode)
				if err != nil {
					return nil, err
				}
				if mode == Authoring {
					return nil, nil
				}
				return map[string]any{k: out}, nil
			}
		}
		names := make([]string, 0, len(branches))
		for _, b := range branches {
			names = append(names, branchName(b))
		}
		return nil, fieldErrf(path, "%q is not a member of this union (one of %s)", k, strings.Join(names, ", "))
	}
	return nil, nil // unreachable (len(m)==1)
}

func buildRecordValue(t *avro.RecordSchema, v any, path string, mode Mode) (any, *FieldError) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fieldErrf(path, "expected an object (record %s), got %s", t.Name(), jsonKind(v))
	}
	out := make(map[string]any, len(t.Fields()))
	for _, f := range t.Fields() {
		fv, present := m[f.Name()]
		if !present {
			if f.HasDefault() {
				out[f.Name()] = f.Default()
				continue
			}
			return nil, fieldErrf(joinPath(path, f.Name()), "missing (required, type %s)", f.Type().Type())
		}
		built, err := buildValue(f.Type(), fv, joinPath(path, f.Name()), mode)
		if err != nil {
			return nil, err
		}
		out[f.Name()] = built
	}
	return out, nil
}

func buildArray(t *avro.ArraySchema, v any, path string, mode Mode) (any, *FieldError) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fieldErrf(path, "expected an array, got %s", jsonKind(v))
	}
	out := make([]any, len(arr))
	for i, item := range arr {
		built, err := buildValue(t.Items(), item, fmt.Sprintf("%s.%d", path, i), mode)
		if err != nil {
			return nil, err
		}
		out[i] = built
	}
	return out, nil
}

func buildMap(t *avro.MapSchema, v any, path string, mode Mode) (any, *FieldError) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fieldErrf(path, "expected an object (map), got %s", jsonKind(v))
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		built, err := buildValue(t.Values(), val, joinPath(path, k), mode)
		if err != nil {
			return nil, err
		}
		out[k] = built
	}
	return out, nil
}

// ── walking a template for placeholders ─────────────────────────────────────────────────────────

// wholeValueRe matches a JSON string that is EXACTLY one `${…}` placeholder token — design §4's
// "a JSON string that is exactly one placeholder" test.
var wholeValueRe = regexp.MustCompile(`^\$\{[^}]+\}$`)

// IsWholeValuePlaceholder reports whether s is exactly one placeholder token.
func IsWholeValuePlaceholder(s string) bool { return wholeValueRe.MatchString(s) }

// WalkStrings applies fn to every string leaf of a generic JSON tree (map[string]any/[]any/
// string/float64/bool/nil, as encoding/json decodes it, or already carrying Placeholder-wrapped
// leaves), rebuilding the tree with fn's replacements. It is the one recursion both the authoring
// check (mark whole-value placeholders, chain_scenario.go) and the run-time resolver
// (internal/chain/amqp.go: bind ${saved.*}, generate ${uuid}/${now}/${now_ms}) walk — so the two
// can never disagree about which leaves are strings to visit.
func WalkStrings(v any, fn func(s string) (any, error)) (any, error) {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			nv, err := WalkStrings(val, fn)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			nv, err := WalkStrings(val, fn)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	default:
		return v, nil
	}
}

// MarkWholeValuePlaceholders wraps every string leaf that is EXACTLY one placeholder token as a
// Placeholder, leaving every other leaf (a plain string, embedded placeholder included) untouched.
// This is the authoring-time template: BuildNative(schema, MarkWholeValuePlaceholders(template),
// Authoring) accepts a placeholder "as a valid value of the field's type" without resolving it to
// anything real (design §4/§8).
// A Placeholder built by MarkWholeValuePlaceholders carries the ORIGINAL, unresolved `${…}` token
// text (not the resolved value) — a run-time resolver (internal/chain) reads it back with
// IsPlaceholder to know WHICH placeholder a leaf was, resolves it, and re-wraps the result as a new
// Placeholder(resolvedText) so BuildNative(schema, tree, Runtime) still recognises it as eligible
// for whole-value typing (design §4).
func MarkWholeValuePlaceholders(v any) any {
	out, _ := WalkStrings(v, func(s string) (any, error) {
		if IsWholeValuePlaceholder(s) {
			return Placeholder(s), nil
		}
		return s, nil
	})
	return out
}

// ── decode (native -> the §3 JSON view) ─────────────────────────────────────────────────────────

// ToJSON is BuildNative's inverse: it renders a Go-native value avro.Unmarshal produced (a
// map[string]any record) as the ONE JSON shape (design §3) — the same shape a `record` was
// authored in, so `body has <path> …` (the existing body grammar) runs on it unchanged.
func ToJSON(schema *Schema) func(native any) (any, error) {
	return func(native any) (any, error) { return jsonValue(schema.avro, native) }
}

func jsonValue(s avro.Schema, v any) (any, error) {
	s = deref(s)
	switch t := s.(type) {
	case *avro.UnionSchema:
		return jsonUnion(t, v)
	case *avro.RecordSchema:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("record %s: decoded value is not a map", t.Name())
		}
		out := make(map[string]any, len(t.Fields()))
		for _, f := range t.Fields() {
			jv, err := jsonValue(f.Type(), m[f.Name()])
			if err != nil {
				return nil, fmt.Errorf("%s.%w", f.Name(), errW(err))
			}
			out[f.Name()] = jv
		}
		return out, nil
	case *avro.ArraySchema:
		arr, _ := v.([]any)
		out := make([]any, len(arr))
		for i, item := range arr {
			jv, err := jsonValue(t.Items(), item)
			if err != nil {
				return nil, err
			}
			out[i] = jv
		}
		return out, nil
	case *avro.MapSchema:
		m, _ := v.(map[string]any)
		out := make(map[string]any, len(m))
		for k, val := range m {
			jv, err := jsonValue(t.Values(), val)
			if err != nil {
				return nil, err
			}
			out[k] = jv
		}
		return out, nil
	case *avro.EnumSchema:
		return v, nil
	case *avro.FixedSchema:
		return base64.StdEncoding.EncodeToString(bytesFromFixed(v)), nil
	case *avro.PrimitiveSchema:
		switch t.Type() {
		case avro.Bytes:
			b, _ := v.([]byte)
			return base64.StdEncoding.EncodeToString(b), nil
		case avro.Int:
			switch n := v.(type) {
			case int32:
				return n, nil
			case int:
				return n, nil
			}
			return v, nil
		case avro.Long:
			switch n := v.(type) {
			case int64:
				return n, nil
			case int:
				return int64(n), nil
			case time.Time:
				// A long with logicalType timestamp-millis/-micros decodes as time.Time in
				// github.com/hamba/avro/v2's generic receiver even though it also accepts a plain
				// int64 on ENCODE (genericReceiver vs. the native codec are not symmetric here) —
				// design §3 has no logical-type row, so this renders it the same way encoding
				// accepts it: epoch milliseconds, a JSON integer.
				return n.UnixMilli(), nil
			}
			return v, nil
		default:
			return v, nil
		}
	default:
		return v, nil
	}
}

func jsonUnion(t *avro.UnionSchema, v any) (any, error) {
	branches := nonNullBranches(t)
	if v == nil {
		return nil, nil
	}
	if len(branches) == 1 {
		return jsonValue(branches[0], v)
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return nil, fmt.Errorf("decoded union value is not a single-key map")
	}
	for k, val := range m {
		for _, b := range branches {
			if branchName(b) == k {
				jv, err := jsonValue(b, val)
				if err != nil {
					return nil, err
				}
				return map[string]any{k: jv}, nil
			}
		}
	}
	return nil, fmt.Errorf("decoded union value names an unknown branch")
}

// ── shared helpers ───────────────────────────────────────────────────────────────────────────────

func nonNullBranches(t *avro.UnionSchema) []avro.Schema {
	var out []avro.Schema
	for _, b := range t.Types() {
		if b.Type() != avro.Null {
			out = append(out, b)
		}
	}
	return out
}

func containsNull(t *avro.UnionSchema) bool {
	for _, b := range t.Types() {
		if b.Type() == avro.Null {
			return true
		}
	}
	return false
}

// branchName is the tag a 2+-branch union's JSON form uses (design §3: `{"<name>": value}`) —
// hamba/avro's own map-union codec keys a record/enum/fixed branch by its FULL qualified name and a
// primitive branch by its bare type name (avro/v2 codec_union.go's unexported schemaTypeName);
// this reimplements that same rule so BuildNative/ToJSON agree with what avro.Marshal/Unmarshal
// actually do on the wire.
func branchName(s avro.Schema) string {
	s = deref(s)
	if n, ok := s.(avro.NamedSchema); ok {
		return n.FullName()
	}
	return string(s.Type())
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// jsonNumber reads v as a number. encoding/json always decodes a JSON number as float64, so that is
// the common case; the native Go integer/float kinds are also accepted because a caller MAY build a
// template tree directly in Go rather than through json.Unmarshal (internal/chain's `envelope`
// preset, design §7, builds its default record this way — it is not authored JSON text).
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	}
	return 0, false
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// errW lets jsonValue prefix a nested field name onto an inner error without double-wrapping when
// the inner error is already plain (fmt.Errorf("%w", err) needs an error, not a string).
func errW(err error) error { return err }

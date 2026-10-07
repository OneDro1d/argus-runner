// Package agentcfg is the ONLY code permitted to read or write an agent folder's `.mcp.json`
// (VR-A1..A3, VR-A5 of the M3-FX build).
//
// WHY IT EXISTS. Before M3-FX the file was written by a truncating heredoc in the onboarding shell
// script (onboard.sh:1335) under a FIXED server key. Three consequences, all measured:
//
//   - a pre-existing non-Argus MCP server was destroyed, silently;
//   - onboarding a SECOND instance into one folder DELETED the first, because the key never varied;
//   - nothing ever parsed the file, so a hand-broken one was simply overwritten.
//
// Bash has no JSON parser, so the fix is not a better heredoc — it is moving the work to where a
// parser exists. Every mutation here is a MINIMAL SPLICE: the bytes outside the member being added,
// replaced or removed are copied verbatim, so a foreign server keeps its own formatting down to the
// byte. That is VR-A1's actual requirement ("everything else is left byte-identical"), and it is why
// this package does not unmarshal-then-remarshal, which would silently reformat the whole document.
//
// A malformed file is REFUSED and left exactly as it was (VR-A3). Rewriting it would destroy the
// hand edits the user is trying to protect, and "I could not parse this" is not the same answer as
// "this had nothing in it" — the distinction VR-P1 exists to preserve.
//
// NOTE ON COMMENTS: strict JSON has none, and `encoding/json` rejects them. A JSONC-style `.mcp.json`
// therefore lands in the REFUSE path rather than being rewritten — the safe direction, and the one
// that never destroys the comments it cannot represent.
package agentcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrMalformed is returned when the existing file is not usable JSON, or its root / `mcpServers` is
// not an object. The file is never modified on this path.
// The message names no FILE on purpose: this package now edits two of them (`.mcp.json` and
// `.claude/settings.local.json`), and a hardcoded filename produced an error that contradicted the
// path it was reporting on — measured against a malformed settings file, which was told its
// `.mcp.json` was broken. Callers add the path they were given.
var ErrMalformed = errors.New("agentcfg: the file is not usable JSON — fix it by hand; onboarding will not rewrite it")

// fileMode requests owner-only access because the file carries bearer tokens (VR-R12); the
// pre-M3-FX files were measured at -rw-r--r-- in known locations.
//
// It is a REQUEST, not a guarantee, and the difference matters on the platform this product is
// actually operated from. On Windows there are no POSIX mode bits and os.Chmod toggles only the
// read-only attribute, so a file created here reports 0666 and the DIRECTORY ACL is what protects
// the token. TestMerge_FileModeIsOwnerOnly skips there rather than asserting a protection the OS is
// not providing — an assertion that passes by not being enforced is worse than no assertion.
const fileMode = 0o600

// Merge adds or replaces one server entry, leaving every other byte of the document intact.
// It reports whether it CREATED the file, which is the provenance VR-A5 needs: only a file that
// onboarding created may later be deleted.
func Merge(path, key string, server []byte) (created bool, err error) {
	if key == "" {
		return false, errors.New("agentcfg: server key is required")
	}
	if !json.Valid(server) {
		return false, fmt.Errorf("agentcfg: the server entry for %q is not valid JSON", key)
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if werr := writeFile(path, newDocument(key, server)); werr != nil {
			return false, werr
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}

	d, err := scan(raw)
	if err != nil {
		return false, err
	}
	return false, writeFile(path, d.upsert(key, server))
}

// Remove deletes one server entry from the file. deleteIfEmpty carries the provenance from Merge:
// when true AND no servers remain, the FILE itself is removed, because an empty {"mcpServers":{}}
// left behind is exactly the trace VR-A5 exists to prevent. When false the file is a user's own and
// is kept whatever it ends up holding.
//
// A missing file and an absent key are both normal teardown states, not errors — teardown must be
// idempotent. A MALFORMED file is still refused (VR-A3): it is neither rewritten nor deleted.
func Remove(path, key string, deleteIfEmpty bool) (removed, fileDeleted bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}

	d, err := scan(raw)
	if err != nil {
		return false, false, err
	}
	out, ok := d.delete(key)
	if !ok {
		return false, false, nil
	}
	if deleteIfEmpty && len(d.members) == 1 { // the one we just removed was the last
		if err := os.Remove(path); err != nil {
			return true, false, err
		}
		return true, true, nil
	}
	return true, false, writeFile(path, out)
}

// Servers lists the server keys in document order. A missing file has none.
func Servers(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := scan(raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(d.members))
	for _, m := range d.members {
		names = append(names, m.key)
	}
	return names, nil
}

// --- document model: offsets into the ORIGINAL bytes, never a re-serialization ---

type member struct {
	key                        string
	keyStart, valStart, valEnd int
}

type doc struct {
	raw                 []byte
	rootOpen, rootClose int // offsets OF the root '{' and '}'
	srvOpen, srvClose   int // offsets OF the mcpServers '{' and '}'; -1 when the key is absent
	members             []member
	crlf                bool
	indent, closeIndent string
}

func (d *doc) find(key string) int {
	for i, m := range d.members {
		if m.key == key {
			return i
		}
	}
	return -1
}

func (d *doc) nl() string {
	if d.crlf {
		return "\r\n"
	}
	return "\n"
}

// upsert returns the new document bytes with key set to server.
func (d *doc) upsert(key string, server []byte) []byte {
	// (a) the key already exists -> replace ONLY its value. Everything else stays byte-identical.
	if i := d.find(key); i >= 0 {
		return splice(d.raw, d.members[i].valStart, d.members[i].valEnd, server)
	}

	// (b) there is no mcpServers object at all -> add one to the root object.
	if d.srvOpen < 0 {
		entry := fmt.Sprintf(`%s%s"mcpServers": {%s%s%s: %s%s%s}`,
			d.nl(), d.indent, d.nl(), d.indent+d.indent, quote(key), server, d.nl(), d.indent)
		if rootHasMembers(d.raw, d.rootOpen, d.rootClose) {
			entry = "," + entry
		}
		return splice(d.raw, d.rootClose, d.rootClose, []byte(entry+d.nl()))
	}

	// (c) a new member inside an existing mcpServers object.
	if len(d.members) == 0 {
		entry := fmt.Sprintf(`%s%s%s: %s%s%s`, d.nl(), d.indent, quote(key), server, d.nl(), d.closeIndent)
		return splice(d.raw, d.srvOpen+1, d.srvClose, []byte(entry))
	}
	last := d.members[len(d.members)-1]
	entry := fmt.Sprintf(`,%s%s%s: %s`, d.nl(), d.indent, quote(key), server)
	return splice(d.raw, last.valEnd, last.valEnd, []byte(entry))
}

// delete returns the new document bytes with key removed, and whether it was there. The cut spans
// the member AND one adjacent comma, so the result is still valid JSON.
func (d *doc) delete(key string) ([]byte, bool) {
	i := d.find(key)
	if i < 0 {
		return nil, false
	}
	switch {
	case len(d.members) == 1: // empty the object out entirely
		return splice(d.raw, d.srvOpen+1, d.srvClose, nil), true
	case i < len(d.members)-1: // take the member and the comma that FOLLOWS it
		return splice(d.raw, d.members[i].keyStart, d.members[i+1].keyStart, nil), true
	default: // the last member: take the comma that PRECEDES it
		return splice(d.raw, d.members[i-1].valEnd, d.members[i].valEnd, nil), true
	}
}

func splice(raw []byte, from, to int, with []byte) []byte {
	out := make([]byte, 0, len(raw)-(to-from)+len(with))
	out = append(out, raw[:from]...)
	out = append(out, with...)
	return append(out, raw[to:]...)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newDocument(key string, server []byte) []byte {
	return []byte(fmt.Sprintf("{\n  \"mcpServers\": {\n    %s: %s\n  }\n}\n", quote(key), server))
}

// writeFile replaces the file atomically, so a crash mid-write cannot leave an agent folder holding
// a half-written .mcp.json — which would be indistinguishable from the malformed file VR-A3 refuses.
func writeFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mcp.json.tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, fileMode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// --- scanning: locate spans with a REAL parser, never a regex ---

func scan(raw []byte) (*doc, error) {
	if !json.Valid(raw) {
		return nil, ErrMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil {
		return nil, ErrMalformed
	}
	if delim, ok := t.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("%w: the root is not a JSON object", ErrMalformed)
	}

	d := &doc{
		raw:         raw,
		rootOpen:    int(dec.InputOffset()) - 1,
		srvOpen:     -1,
		srvClose:    -1,
		crlf:        bytes.Contains(raw, []byte("\r\n")),
		indent:      "    ",
		closeIndent: "  ",
	}

	for {
		prev := int(dec.InputOffset())
		t, err := dec.Token()
		if err != nil {
			return nil, ErrMalformed
		}
		if delim, ok := t.(json.Delim); ok && delim == '}' {
			d.rootClose = int(dec.InputOffset()) - 1
			break
		}
		key, _ := t.(string)
		if key != "mcpServers" {
			if err := readValue(dec); err != nil {
				return nil, ErrMalformed
			}
			continue
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, ErrMalformed
		}
		if delim, ok := vt.(json.Delim); !ok || delim != '{' {
			return nil, fmt.Errorf("%w: mcpServers is not an object", ErrMalformed)
		}
		d.srvOpen = int(dec.InputOffset()) - 1
		_ = prev
		for {
			mprev := int(dec.InputOffset())
			mt, err := dec.Token()
			if err != nil {
				return nil, ErrMalformed
			}
			if delim, ok := mt.(json.Delim); ok && delim == '}' {
				d.srvClose = int(dec.InputOffset()) - 1
				break
			}
			mkey, _ := mt.(string)
			ks := indexFrom(raw, mprev, '"')
			vs := valueStart(raw, int(dec.InputOffset()))
			if err := readValue(dec); err != nil {
				return nil, ErrMalformed
			}
			d.members = append(d.members, member{key: mkey, keyStart: ks, valStart: vs, valEnd: int(dec.InputOffset())})
		}
	}
	if len(d.members) > 0 {
		d.indent = indentOf(raw, d.members[0].keyStart)
	}
	if d.srvOpen >= 0 {
		d.closeIndent = indentOf(raw, d.srvClose)
	}
	return d, nil
}

// readValue consumes exactly one JSON value, including a nested object or array.
func readValue(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil // a scalar is already fully consumed
	}
	depth := 1
	for depth > 0 {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

func indexFrom(raw []byte, from int, c byte) int {
	for i := from; i < len(raw); i++ {
		if raw[i] == c {
			return i
		}
	}
	return from
}

// valueStart walks past the ':' and any whitespace after a member key.
func valueStart(raw []byte, from int) int {
	for i := from; i < len(raw); i++ {
		switch raw[i] {
		case ' ', '\t', '\r', '\n', ':':
			continue
		default:
			return i
		}
	}
	return from
}

// indentOf returns the whitespace between the line start and off, so an inserted member lines up
// with the ones already there rather than imposing this package's taste on someone's file.
func indentOf(raw []byte, off int) string {
	start := off
	for start > 0 && raw[start-1] != '\n' {
		start--
	}
	for i := start; i < off; i++ {
		if raw[i] != ' ' && raw[i] != '\t' {
			return string(raw[start:i])
		}
	}
	return string(raw[start:off])
}

func rootHasMembers(raw []byte, open, close int) bool {
	for i := open + 1; i < close && i < len(raw); i++ {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return true
		}
	}
	return false
}

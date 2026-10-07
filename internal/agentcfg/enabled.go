package agentcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// The second file an agent folder holds: `.claude/settings.local.json`.
//
// WHY IT IS HERE. `.mcp.json` registers a server; `enabledMcpjsonServers` is what makes the client
// TRUST it. Onboarding has always written both, and until now it wrote this one with a truncating
// `printf > file` — the exact defect `.mcp.json` was rescued from, on a file that is arguably worse
// to destroy: settings.local.json can hold the user's own `permissions`, `hooks` and `env`, none of
// which onboarding put there and all of which it was silently deleting.
//
// The mechanism is the same as agentcfg's: locate spans with a real parser, splice bytes, leave
// everything outside the edit byte-identical, and REFUSE a file that cannot be parsed rather than
// rewrite it. What differs is only the shape — a top-level array of strings instead of an object of
// objects.

// EnabledServersKey is the member this file manages. Nothing else in settings.local.json is ours.
const EnabledServersKey = "enabledMcpjsonServers"

// ErrNotAnArray is returned when the member exists but is not a JSON array. Like ErrMalformed, the
// file is left exactly as it was: a hand-edited value we do not understand is a reason to stop, not
// a reason to overwrite.
var ErrNotAnArray = errors.New("agentcfg: enabledMcpjsonServers is not an array — fix it by hand; onboarding will not rewrite it")

// EnableServer ensures name appears in the file's enabledMcpjsonServers array.
//
// Returns created=true only when the FILE did not exist, which is the provenance that decides whether
// teardown may later delete it — the same rule Merge follows for .mcp.json.
//
// Already present is a no-op that touches nothing, not a rewrite that happens to produce the same
// bytes: re-running onboarding must not change a file's mtime for no reason, because a changed mtime
// is a signal somebody may be watching.
func EnableServer(path, name string) (created bool, err error) {
	if name == "" {
		return false, errors.New("agentcfg: server name is required")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if werr := writeFile(path, newSettings(name)); werr != nil {
			return false, werr
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	a, err := scanArray(raw, EnabledServersKey)
	if err != nil {
		return false, err
	}
	if a.has(name) {
		return false, nil
	}
	return false, writeFile(path, a.add(name))
}

// DisableServer removes name from the array.
//
// deleteIfEmpty carries the provenance from EnableServer, and the file is removed only when it is
// BOTH empty of our entry and empty of everything else — a settings file still holding the user's
// permissions or hooks is theirs, whatever we put in it once.
func DisableServer(path, name string, deleteIfEmpty bool) (removed, fileDeleted bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	a, err := scanArray(raw, EnabledServersKey)
	if err != nil {
		return false, false, err
	}
	if !a.has(name) {
		return false, false, nil
	}
	out := a.remove(name)

	// Only OUR member left, and it is now empty, and onboarding created the file: nothing of the
	// user's is in it, so leaving `{"enabledMcpjsonServers":[]}` behind is the same dead trace VR-A5
	// removes from .mcp.json.
	if deleteIfEmpty && len(a.elems) == 1 && a.soleTopLevelMember {
		if rerr := os.Remove(path); rerr != nil {
			return true, false, rerr
		}
		return true, true, nil
	}
	return true, false, writeFile(path, out)
}

// EnabledServers lists the names currently enabled, in document order.
func EnabledServers(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := scanArray(raw, EnabledServersKey)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(a.elems))
	for _, e := range a.elems {
		out = append(out, e.str)
	}
	return out, nil
}

func newSettings(name string) []byte {
	return []byte(fmt.Sprintf("{\n  %s: [%s]\n}\n", quote(EnabledServersKey), quote(name)))
}

// --- the array document: spans into the ORIGINAL bytes ---

type elem struct {
	str        string
	start, end int
}

type arrayDoc struct {
	raw                 []byte
	rootOpen, rootClose int
	arrOpen, arrClose   int // offsets OF '[' and ']'; -1 when the member is absent
	elems               []elem
	// soleTopLevelMember is true when OUR key is the only thing in the root object — the condition
	// under which deleting the file destroys nothing of the user's.
	soleTopLevelMember bool
	crlf               bool
	indent             string
}

func (a *arrayDoc) nl() string {
	if a.crlf {
		return "\r\n"
	}
	return "\n"
}

func (a *arrayDoc) has(name string) bool {
	for _, e := range a.elems {
		if e.str == name {
			return true
		}
	}
	return false
}

func (a *arrayDoc) add(name string) []byte {
	// (a) the member is absent entirely -> add it to the root object.
	if a.arrOpen < 0 {
		entry := fmt.Sprintf(`%s%s%s: [%s]`, a.nl(), a.indent, quote(EnabledServersKey), quote(name))
		if rootHasMembers(a.raw, a.rootOpen, a.rootClose) {
			entry = "," + entry
		}
		return splice(a.raw, a.rootClose, a.rootClose, []byte(entry+a.nl()))
	}
	// (b) an empty array -> place the first element inside it.
	if len(a.elems) == 0 {
		return splice(a.raw, a.arrOpen+1, a.arrClose, []byte(quote(name)))
	}
	// (c) append after the last element, matching whatever separator style is already there.
	last := a.elems[len(a.elems)-1]
	return splice(a.raw, last.end, last.end, []byte(", "+quote(name)))
}

func (a *arrayDoc) remove(name string) []byte {
	for i, e := range a.elems {
		if e.str != name {
			continue
		}
		switch {
		case len(a.elems) == 1: // empty the array out
			return splice(a.raw, a.arrOpen+1, a.arrClose, nil)
		case i < len(a.elems)-1: // take this element and the comma that FOLLOWS it
			return splice(a.raw, e.start, a.elems[i+1].start, nil)
		default: // the last element: take the comma that PRECEDES it
			return splice(a.raw, a.elems[i-1].end, e.end, nil)
		}
	}
	return a.raw
}

// scanArray locates the named top-level member and, if it is an array of strings, every element's
// byte span. Anything it cannot represent is refused rather than approximated.
func scanArray(raw []byte, key string) (*arrayDoc, error) {
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

	a := &arrayDoc{
		raw: raw, rootOpen: int(dec.InputOffset()) - 1,
		arrOpen: -1, arrClose: -1,
		crlf:   bytes.Contains(raw, []byte("\r\n")),
		indent: "  ",
	}

	topLevel := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return nil, ErrMalformed
		}
		if delim, ok := t.(json.Delim); ok && delim == '}' {
			a.rootClose = int(dec.InputOffset()) - 1
			break
		}
		k, _ := t.(string)
		topLevel++
		if k != key {
			if err := readValue(dec); err != nil {
				return nil, ErrMalformed
			}
			continue
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, ErrMalformed
		}
		if delim, ok := vt.(json.Delim); !ok || delim != '[' {
			return nil, ErrNotAnArray
		}
		a.arrOpen = int(dec.InputOffset()) - 1
		for {
			eprev := int(dec.InputOffset())
			et, err := dec.Token()
			if err != nil {
				return nil, ErrMalformed
			}
			if delim, ok := et.(json.Delim); ok && delim == ']' {
				a.arrClose = int(dec.InputOffset()) - 1
				break
			}
			s, ok := et.(string)
			if !ok {
				// A non-string element (a number, an object) is something this package does not model.
				// Refusing is the only honest answer: rewriting would silently drop it.
				return nil, ErrNotAnArray
			}
			a.elems = append(a.elems, elem{str: s, start: indexFrom(raw, eprev, '"'), end: int(dec.InputOffset())})
		}
	}
	a.soleTopLevelMember = topLevel == 1 && a.arrOpen >= 0
	if a.arrOpen >= 0 {
		a.indent = indentOf(raw, indexBack(raw, a.arrOpen, '"'))
	}
	return a, nil
}

// indexBack finds the last occurrence of c at or before from — used to locate the member KEY's
// opening quote from the array's '[', so an inserted member matches the file's own indentation.
func indexBack(raw []byte, from int, c byte) int {
	for i := from; i >= 0; i-- {
		if raw[i] == c {
			return i
		}
	}
	return from
}

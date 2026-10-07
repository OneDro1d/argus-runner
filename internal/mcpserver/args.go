package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// VR10-S1 (V28-015): an MCP tool refuses every argument it does not declare.
//
// Shared by BOTH planes because control imports this package: the dispatcher (server.go) calls
// UnknownArgs on the CALLER's bytes before PreCheck, before the run lock, before a run id exists and
// before injectRunID, and answers with RefuseUnknown. The tool's own InputSchema is the accepted-key
// list, so one check covers every tool that closes its object.
//
// Only TOP-LEVEL keys are policed in this build (§0.10): every tool declares string properties only,
// so an unknown nested key cannot be told from a value. The tools/list tests on both planes fail the
// day a tool declares an object-typed property, which is where that limit becomes visible.

// maxSuggestDistance is the Levenshtein distance up to which an accepted key is offered as "did you
// mean" (S1-a). Beyond it no suggestion is made: a wrong hint is worse than none.
const maxSuggestDistance = 2

// UnknownArgs returns the caller-supplied top-level keys that the tool's InputSchema does not declare,
// in the CALLER's order (S1-b), plus the declared keys (required first, then alphabetical — the order
// every refusal lists them in, and the tie-break order for the nearest match).
//
// Keys beginning with "_" are ignored: a conforming MCP client may attach its own notes that way
// (`_meta` and similar); they are not ours and mean nothing to us (D2).
//
// The check enforces the schema, it does not override it: a schema that does not close its object
// (`additionalProperties` absent or true) is OPEN by JSON Schema's own rule and yields no unknown
// keys. Both plane builders close theirs. The local router's proxy schemas stay open on purpose — it
// forwards the caller's bytes unchanged and the upstream plane, which owns the real contract, refuses.
//
// err is non-nil only when the arguments are not a JSON object at all; the caller then has nothing
// to police here and the tool's own guard answers as before.
func UnknownArgs(schema map[string]any, raw json.RawMessage) (unknown, accepted []string, err error) {
	accepted, _ = declaredArgs(schema)
	keys, err := callerKeys(raw)
	if err != nil {
		return nil, nil, err
	}
	if !schemaClosed(schema) {
		return nil, accepted, nil
	}
	declared := make(map[string]bool, len(accepted))
	for _, a := range accepted {
		declared[a] = true
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "_") || declared[k] {
			continue
		}
		unknown = append(unknown, k)
	}
	return unknown, accepted, nil
}

// RequiredArgs returns the schema's `required` list (empty when it has none). The builders store it
// as []string; a schema decoded from JSON carries []any — both are read.
func RequiredArgs(schema map[string]any) []string {
	switch r := schema["required"].(type) {
	case []string:
		return append([]string(nil), r...)
	case []any:
		out := make([]string, 0, len(r))
		for _, v := range r {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// RefuseUnknown builds the tool error for a call that carried undeclared keys (D7): every unknown key
// named in the caller's order, the tool named, the nearest accepted key suggested when one is within
// maxSuggestDistance, and the accepted list with the required ones marked. The structured fields ride
// alongside the sentence so a client can act on them without parsing prose.
//
// A suggestion is never an acceptance (D6): the outcome is always the error plane and the caller must
// send the call again.
func RefuseUnknown(tool string, unknown, accepted, required []string) *Outcome {
	isRequired := make(map[string]bool, len(required))
	for _, r := range required {
		isRequired[r] = true
	}
	shown := make([]string, 0, len(accepted))
	for _, a := range accepted {
		if isRequired[a] {
			shown = append(shown, a+" (required)")
		} else {
			shown = append(shown, a)
		}
	}
	didYouMean := map[string]string{}
	for _, u := range unknown {
		if n, ok := nearest(u, accepted); ok {
			didYouMean[u] = n
		}
	}

	quoted := make([]string, len(unknown))
	for i, u := range unknown {
		quoted[i] = strconv.Quote(u)
	}
	var sb strings.Builder
	if len(unknown) == 1 {
		sb.WriteString("unknown argument ")
	} else {
		sb.WriteString("unknown arguments ")
	}
	sb.WriteString(strings.Join(quoted, ", "))
	sb.WriteString(" for " + tool)
	if len(didYouMean) > 0 {
		var hints []string
		for _, u := range unknown {
			n, ok := didYouMean[u]
			if !ok {
				continue
			}
			if len(unknown) == 1 {
				hints = append(hints, strconv.Quote(n))
			} else {
				hints = append(hints, strconv.Quote(n)+" for "+strconv.Quote(u))
			}
		}
		sb.WriteString(" — did you mean " + strings.Join(hints, ", ") + "?")
	} else {
		sb.WriteString(".")
	}
	list := strings.Join(shown, ", ")
	if list == "" {
		list = "(none)"
	}
	sb.WriteString(" Accepted arguments: " + list + ". The call did not run; send it again with accepted arguments only.")

	payload := map[string]any{
		"error": sb.String(), "tool": tool, "unknown": unknown, "accepted": accepted, "required": required,
	}
	if len(didYouMean) > 0 {
		payload["did_you_mean"] = didYouMean
	}
	o := ToolErr(payload)
	return &o
}

// schemaClosed reports whether the schema closes its object (`additionalProperties: false`).
func schemaClosed(schema map[string]any) bool {
	ap, ok := schema["additionalProperties"].(bool)
	return ok && !ap
}

// declaredArgs lists the schema's declared keys: the required ones first, in `required` order, then
// the rest alphabetically. Go maps carry no declaration order, so this IS "schema order" everywhere
// the refusal text and the nearest-match tie-break need one.
func declaredArgs(schema map[string]any) (accepted, required []string) {
	required = RequiredArgs(schema)
	props, _ := schema["properties"].(map[string]any)
	seen := make(map[string]bool, len(props))
	for _, r := range required {
		if !seen[r] {
			accepted = append(accepted, r)
			seen[r] = true
		}
	}
	var rest []string
	for k := range props {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(accepted, rest...), required
}

// callerKeys reads the top-level keys of the caller's argument object in the order they were sent.
// No arguments, or a JSON null, means no keys. Anything that is not an object is an error.
func callerKeys(raw json.RawMessage) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	var keys []string
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// nearest returns the accepted key closest to key when it is within maxSuggestDistance. Ties go to
// the first accepted key in order.
func nearest(key string, accepted []string) (string, bool) {
	best, bestD := "", maxSuggestDistance+1
	for _, a := range accepted {
		if d := levenshtein(key, a); d < bestD {
			best, bestD = a, d
		}
	}
	return best, bestD <= maxSuggestDistance
}

// levenshtein is the edit distance between two strings (insert, delete, substitute; one each).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

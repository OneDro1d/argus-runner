package chain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ─────────────────────────────────────────────────────────────────────────────────────────
// CHAIN STEP CAPTURE — thread one step's response into a later step's args.
//
// Before this, the chain executor resolved ALL step args once, up front, from ${cid}/${VAR}
// (argus/chain_scenario.go: parseChainSpec → resolveVars), and nothing carried a step's RESPONSE
// forward. Against a SUT that mints ids server-side — Memstore generates a document id per write —
// a self-contained write → read-that-exact-doc chain was simply INEXPRESSIBLE, which is why three
// consecutive triage rounds reported the same two consequences:
//   - scenarios pinned hand-seeded fixture ids instead. One environment reset destroyed the
//     fixture and SEVEN of Memstore's eight failures fell out of it at once.
//   - CLEANUP could not delete what a chain created → 197 leftover libraries across 15 runs.
//
// The contract:
//   save:  {"type":"mcp","name":"write-doc",…,"save":{"docId":"id"}}
//   use:   {"type":"mcp","name":"read-back",…,"args":{"document_id":"${saved.docId}"}}
//
// `${saved.…}` carries a dot on purpose: the up-front varRe (`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)
// cannot match it, so it survives that pass untouched and is bound at run time instead. No change
// to the existing resolution order was needed.
// ─────────────────────────────────────────────────────────────────────────────────────────

// savedRefRe matches a ${saved.<var>} reference. <var> is letters/digits/_/- (no dots, so the
// variable name can never be confused with the path syntax used on the save side).
//
// ⛔ V31-003: it compiles scenario.SavedRefPattern rather than its own copy. The validator refuses a
// check carrying a placeholder nothing fills in, and it treats ${saved.<var>} as fillable — so the
// two grammars MUST be one, or the validator could accept a reference this runtime leaves literal.
var savedRefRe = regexp.MustCompile(scenario.SavedRefPattern)

// bindSaved substitutes every ${saved.<var>} in s from vars. It returns an error naming EVERY
// unresolved variable rather than passing the literal through: a literal `${saved.docId}` reaching
// the SUT surfaces as a confusing downstream complaint ("document_id must be a valid UUID") that
// blames the SUT for a scenario-authoring mistake — precisely the misattribution this whole line
// of work exists to prevent. Fail at the boundary, name the variable, say how to fix it.
func bindSaved(s string, vars map[string]string) (string, error) {
	var missing []string
	out := savedRefRe.ReplaceAllStringFunc(s, func(m string) string {
		name := savedRefRe.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		// JSON-escape: the captured value is spliced into a JSON args document, and a SUT is free
		// to return quotes/backslashes in an id or name.
		b, err := json.Marshal(v)
		if err != nil {
			return m
		}
		return strings.Trim(string(b), `"`)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unresolved ${saved.%s} — no earlier step saved %q; add `\"save\":{%q:\"<path>\"}` to the step that produces it",
			strings.Join(missing, "}, ${saved."), missing[0], missing[0])
	}
	return out, nil
}

// captureFrom extracts the value at `path` from an MCP response envelope, as a string.
//
// It looks in the two places a SUT actually puts its payload, in order:
//  1. the JSON *inside* result.content[0].text — the common MCP shape, where the tool's real
//     response is a JSON document carried as text;
//  2. the `result` object itself — for SUTs that return structured results directly.
//
// Path is dot-separated; a numeric segment indexes an array: `id`, `doc.id`, `items.0.id`.
// ok=false when the envelope is unparseable or the path is absent — the caller turns that into a
// failure of the SAVING step, so the break is reported where the authoring mistake is rather than
// two steps later when something else gets an empty id.
func captureFrom(raw json.RawMessage, path string) (string, bool) {
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Result) == 0 {
		return "", false
	}
	// (1) through content[0].text
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(env.Result, &res) == nil && len(res.Content) > 0 && res.Content[0].Text != "" {
		var inner any
		if json.Unmarshal([]byte(res.Content[0].Text), &inner) == nil {
			if v, ok := walkPath(inner, path); ok {
				return v, true
			}
		}
	}
	// (2) the result object itself
	var outer any
	if json.Unmarshal(env.Result, &outer) == nil {
		if v, ok := walkPath(outer, path); ok {
			return v, true
		}
	}
	return "", false
}

// captureFromJSON extracts the value at `path` from a PLAIN JSON response body — an http step's
// answer, which carries no MCP envelope to unwrap first (contrast captureFrom, above, which reads
// through `result`/`result.content[0].text`). Same dot-path grammar, same walkPath, same
// ok=false-on-absent contract, so a save failure reads identically across both engines.
func captureFromJSON(raw []byte, path string) (string, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	return walkPath(v, path)
}

// captureRegex (P4) saves the FIRST capture group of the FIRST match of sp.Regex in
// text — for a text body such as Prometheus exposition. fail is "" on success, else the by-name
// failure text (an unusable regex is refused the same way the validator refuses it: SaveProblem).
func captureRegex(text, varName string, sp scenario.SaveSpec) (val, fail string) {
	if why := scenario.SaveProblem(varName, sp); why != "" {
		return "", "save failed: " + why
	}
	re := regexp.MustCompile(sp.Regex) // SaveProblem compiled it
	m := re.FindStringSubmatch(text)
	if m == nil || len(m) < 2 {
		return "", "save failed: the regex for `" + varName + "` matched nothing in this step's response"
	}
	return m[1], ""
}

// saveFromHTTP resolves ONE save entry against an http step's plain response body.
func saveFromHTTP(raw []byte, varName string, sp scenario.SaveSpec) (val, fail string) {
	if sp.Regex != "" || sp.Invalid != "" {
		return captureRegex(string(raw), varName, sp)
	}
	v, ok := captureFromJSON(raw, sp.Path)
	if !ok {
		return "", "save failed: no value at path " + strconv.Quote(sp.Path) +
			" for variable " + strconv.Quote(varName) + " in this step's response body"
	}
	return v, ""
}

// saveFromMCP resolves ONE save entry against an mcp step's response envelope. A regex reads the
// result's content text (every content[].text, joined by newlines), else the raw envelope.
func saveFromMCP(raw json.RawMessage, varName string, sp scenario.SaveSpec) (val, fail string) {
	if sp.Regex != "" || sp.Invalid != "" {
		text := string(raw)
		var env struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &env) == nil {
			var parts []string
			for _, c := range env.Result.Content {
				parts = append(parts, c.Text)
			}
			if len(parts) > 0 {
				text = strings.Join(parts, "\n")
			}
		}
		return captureRegex(text, varName, sp)
	}
	v, ok := captureFrom(raw, sp.Path)
	if !ok {
		return "", "save failed: no value at path " + strconv.Quote(sp.Path) +
			" for variable " + strconv.Quote(varName) +
			" in this step's response (the path is read through result.content[0].text, else result)"
	}
	return v, ""
}

// walkPath descends a decoded JSON value by a dot path, returning the leaf as a string.
func walkPath(v any, path string) (string, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return "", false
			}
			cur = node[i]
		default:
			return "", false
		}
	}
	return scalarString(cur)
}

// scalarString renders a JSON leaf as the string that will be spliced into a later step's args.
// A container is refused: splicing an object into a string field would produce nonsense, and the
// author almost certainly meant a deeper path.
func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		// json numbers decode as float64; render integers without a trailing ".0"
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case nil:
		return "", false
	default:
		return "", false
	}
}

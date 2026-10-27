package chain

import (
	"bytes"
	"encoding/json"
	"errors"
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
	return bindSavedIn(s, vars, false)
}

// bindSavedBody is bindSaved for the two places a JSON value can stand: an http step's body and an mcp step's
// args. A variable saved with the `json` form is spliced RAW there, and when the reference is
// the whole string value (`"${saved.x}"`, the only way to write one inside a JSON document that must parse
// before it is bound) the quotes around it go too, so the value arrives unquoted. A text variable is bound
// exactly as bindSaved binds it, quotes kept.
func bindSavedBody(s string, vars map[string]string) (string, error) {
	return bindSavedIn(s, vars, true)
}

// savedRefQuotedRe matches a reference with its surrounding JSON quotes first, else bare.
var savedRefQuotedRe = regexp.MustCompile(`"` + scenario.SavedRefPattern + `"|` + scenario.SavedRefPattern)

// jsonKindPrefix marks, in the capture store, that a variable was saved with the `json` form. The store is a
// plain map[string]string that every step type reads, and a reserved key that no variable name can spell
// (savedRefRe admits letters, digits, `_` and `-` only) carries the kind without changing a signature that
// dozens of callers share. The marker's value is "1", below every redaction floor, and it is never a variable.
const jsonKindPrefix = "\x00json:"

// isJSONSaved reports whether name was saved with the `json` form.
func isJSONSaved(vars map[string]string, name string) bool {
	_, ok := vars[jsonKindPrefix+name]
	return ok
}

// refsJSONSaved reports whether tmpl names a variable saved with the `json` form.
func refsJSONSaved(tmpl string, vars map[string]string) bool {
	for _, m := range savedRefRe.FindAllStringSubmatch(tmpl, -1) {
		if isJSONSaved(vars, m[1]) {
			return true
		}
	}
	return false
}

// bindSavedIn is the one binder. rawBody false: a JSON-saved variable is REFUSED, by name (url, headers,
// claims, an amqp field: the validator refused it too, this is the run-time backstop). rawBody true: a JSON-saved
// variable is spliced raw.
func bindSavedIn(s string, vars map[string]string, rawBody bool) (string, error) {
	var missing, misplaced []string
	re := savedRefRe
	if rawBody {
		re = savedRefQuotedRe
	}
	out := re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		name, quoted := sub[1], strings.HasPrefix(m, `"`)
		if rawBody && name == "" { // the bare alternative matched: its name is the second group
			name = sub[2]
		}
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		if isJSONSaved(vars, name) {
			if !rawBody {
				misplaced = append(misplaced, name)
				return m
			}
			return v // raw: the quotes of a whole-value reference are dropped with the match
		}
		// JSON-escape: the captured value is spliced into a JSON args document, and a SUT is free
		// to return quotes/backslashes in an id or name.
		b, err := json.Marshal(v)
		if err != nil {
			return m
		}
		esc := strings.Trim(string(b), `"`)
		if quoted {
			return `"` + esc + `"`
		}
		return esc
	})
	if len(missing) > 0 {
		return "", unresolvedSavedError(missing)
	}
	if len(misplaced) > 0 {
		return "", errors.New(scenario.SavedJSONPlacementSentence("this step", misplaced[0]))
	}
	return out, nil
}

// unresolvedSavedError is the sentence for ${saved.<var>} references no earlier step saved (shared by every binder).
func unresolvedSavedError(missing []string) error {
	return fmt.Errorf("unresolved ${saved.%s} — no earlier step saved %q; add `\"save\":{%q:\"<path>\"}` to the step that produces it",
		strings.Join(missing, "}, ${saved."), missing[0], missing[0])
}

// checkBoundJSON is the guard on a raw splice: when the template was a JSON document before binding (its
// references read as `null`) and a JSON-saved variable went into it, the bound text must still be a JSON
// document. A body that stopped being one (the variable sat inside a longer string, or its value is not a JSON
// value in that position) is not sent: the step reads not-measured with a sentence naming the variable.
// A template that references no JSON-saved variable is never judged here, so nothing existing changes.
func checkBoundJSON(tmpl, bound string, vars map[string]string, what string) error {
	var names []string
	for _, m := range savedRefRe.FindAllStringSubmatch(tmpl, -1) {
		if isJSONSaved(vars, m[1]) {
			names = append(names, m[1])
		}
	}
	if len(names) == 0 || json.Valid([]byte(bound)) {
		return nil
	}
	if !json.Valid([]byte(savedRefRe.ReplaceAllString(tmpl, "null"))) {
		return nil // the template was not a JSON document to begin with (a text body): nothing to hold it to
	}
	return fmt.Errorf("%s is not valid JSON once ${saved.%s} is placed in it — a JSON-saved variable replaces a whole JSON value (write \"${saved.%s}\" or ${saved.%s} where a value stands), it is not text inside a longer string",
		what, names[0], names[0], names[0])
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
	if sp.JSON {
		return saveJSON(varName, sp, [][]byte{raw}, " in this step's response body")
	}
	v, ok := captureFromJSON(raw, sp.Path)
	if !ok {
		return "", "save failed: " + noScalarAt(varName, sp.Path, [][]byte{raw}, " in this step's response body")
	}
	return v, ""
}

// recordCapture stores one saved value in the step's captures and, for the `json` form, the kind marker the
// binder reads (isJSONSaved). Every step type that saves goes through here, so the marker cannot be forgotten.
func recordCapture(captured map[string]string, name string, sp scenario.SaveSpec, val string) {
	captured[name] = val
	if sp.JSON {
		captured[jsonKindPrefix+name] = "1"
	}
}

// saveJSON saves the value at sp.Path as JSON text: exactly the bytes the SUT wrote for it,
// whitespace removed (json.Compact), so a number keeps every digit (`12345678901234567890`, `0.10`) because
// nothing here ever decodes a number: the value is cut out of the document by key and index, never parsed
// into a float64. docs are the documents the path is tried against, in order (an mcp answer has two).
func saveJSON(varName string, sp scenario.SaveSpec, docs [][]byte, where string) (val, fail string) {
	for _, d := range docs {
		if v, ok := jsonAtPath(d, sp.Path); ok {
			if len(v) > scenario.MaxJSONSavedBytes {
				return "", fmt.Sprintf("save failed: the JSON value for variable %s is %d bytes, over the %d-byte limit of a saved value (save a smaller part of the answer)",
					strconv.Quote(varName), len(v), scenario.MaxJSONSavedBytes)
			}
			return v, ""
		}
	}
	return "", "save failed: no value at path " + strconv.Quote(sp.Path) +
		" for variable " + strconv.Quote(varName) + where
}

// jsonAtPath cuts the value at a dot path out of a JSON document and returns it compact, untouched
// (`$` is the whole document). Same path grammar as walkPath: an object key, or an array index as a number.
func jsonAtPath(doc []byte, path string) (string, bool) {
	cur := json.RawMessage(doc)
	if path != "$" {
		for _, seg := range strings.Split(path, ".") {
			t := bytes.TrimSpace(cur)
			if len(t) == 0 {
				return "", false
			}
			switch t[0] {
			case '{':
				var m map[string]json.RawMessage
				if json.Unmarshal(t, &m) != nil {
					return "", false
				}
				next, ok := m[seg]
				if !ok {
					return "", false
				}
				cur = next
			case '[':
				var a []json.RawMessage
				if json.Unmarshal(t, &a) != nil {
					return "", false
				}
				i, err := strconv.Atoi(seg)
				if err != nil || i < 0 || i >= len(a) {
					return "", false
				}
				cur = a[i]
			default:
				return "", false
			}
		}
	}
	var buf bytes.Buffer
	if json.Compact(&buf, cur) != nil {
		return "", false
	}
	return buf.String(), true
}

// noScalarAt is the failure sentence of the string form (a scalar as text). When the path DOES resolve, to an
// object, an array or null, the sentence says which, and that `{"json": "<path>"}` saves it as JSON; when it
// resolves to nothing it is the sentence it always was.
func noScalarAt(varName, path string, docs [][]byte, where string) string {
	for _, d := range docs {
		if v, ok := jsonAtPath(d, path); ok {
			kind := ""
			switch {
			case strings.HasPrefix(v, "{"):
				kind = "an object"
			case strings.HasPrefix(v, "["):
				kind = "an array"
			case v == "null":
				kind = "null"
			}
			if kind != "" {
				return "the value at path " + strconv.Quote(path) + " for variable " + strconv.Quote(varName) +
					" is " + kind + ", which the string form cannot save as text — save it as JSON with " +
					`"save": {` + strconv.Quote(varName) + `: {"json": ` + strconv.Quote(path) + `}}`
			}
		}
	}
	return "no value at path " + strconv.Quote(path) + " for variable " + strconv.Quote(varName) + where
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
	const where = " in this step's response (the path is read through result.content[0].text, else result)"
	if sp.JSON {
		return saveJSON(varName, sp, mcpDocuments(raw), where)
	}
	v, ok := captureFrom(raw, sp.Path)
	if !ok {
		return "", "save failed: " + noScalarAt(varName, sp.Path, mcpDocuments(raw), where)
	}
	return v, ""
}

// mcpDocuments are the documents captureFrom tries a path against, in its order: the JSON inside
// result.content[0].text, then the result object itself. The `json` save form reads through the same two, so
// a path means the same thing in both forms and `$` is the document the string form's paths start from.
func mcpDocuments(raw json.RawMessage) [][]byte {
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Result) == 0 {
		return nil
	}
	var docs [][]byte
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(env.Result, &res) == nil && len(res.Content) > 0 && res.Content[0].Text != "" &&
		json.Valid([]byte(res.Content[0].Text)) {
		docs = append(docs, []byte(res.Content[0].Text))
	}
	return append(docs, []byte(env.Result))
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

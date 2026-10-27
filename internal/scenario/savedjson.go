package scenario

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A VARIABLE SAVED WITH `{"json": "<path>"}` CARRIES A JSON VALUE, AND CAN ONLY STAND WHERE ONE CAN.
//
// The string form saves a scalar as TEXT and a later step splices it JSON-string-escaped, which is right for
// an id in a url or a header and wrong for an object. The `json` form saves the value's own JSON text and a
// later step's body or args splices it RAW. That is only meaningful in a body or in args: in a url or a header
// the spliced text would be nonsense, and in a claim it would put a structure where a comparison value goes.
// So the placement is refused at AUTHORING, by name, and the validator (Validate) and the executor's preflight
// (argus.runChainScenarioWithFaults) both ask THIS one function, so the two doors cannot disagree.

// MaxJSONSavedBytes bounds one JSON-saved value. A saved value is held in the capture store and spliced into
// every later request that names it; 64 KiB is far past any id list or resource a check threads on, and keeps a
// hostile or runaway answer from being copied into every later step.
const MaxJSONSavedBytes = 65536

// savedAnyRe finds every ${saved.<var>} in a piece of text; it is the runtime's grammar (SavedRefPattern).
var savedAnyRe = regexp.MustCompile(SavedRefPattern)

// SavedJSONPlacementSentence is the ONE sentence for a JSON-saved variable placed where it cannot stand.
// where reads "the url", "the header \"X\"" or "a claim".
func SavedJSONPlacementSentence(where, varName string) string {
	return fmt.Sprintf("%s uses ${saved.%s}, which was saved with the {\"json\": …} form — a JSON-saved variable can "+
		"only be placed in a body or args; to use the value here, save a scalar with the string form "+
		"(\"save\": {\"<var>\": \"<path>\"}) in a step of its own", where, varName)
}

// SavedJSONIssue is one refusal: the step it belongs to and the sentence (which does not repeat the step name).
type SavedJSONIssue struct {
	Step string
	Why  string
}

// SavedJSONProblems judges, for a whole chain, everything the `json` save form adds to the rules:
//   - one variable has ONE kind: saved as text in one step and with the `json` form in another is refused;
//   - a JSON-saved variable is named in a step's url, in a header, or in a claim: refused (SavedJSONPlacementSentence).
//
// The kind is chain-wide, so a reference BEFORE the saving step is judged by the same rule. Order of the result
// is the order of the steps, then of the places inside a step.
func SavedJSONProblems(steps []ChainStep, claims map[string][]string) []SavedJSONIssue {
	jsonBy, textBy := map[string]string{}, map[string]string{}
	for _, st := range steps {
		for name, sp := range st.Save {
			if sp.Invalid != "" {
				continue
			}
			if sp.JSON {
				if _, ok := jsonBy[name]; !ok {
					jsonBy[name] = st.Name
				}
			} else if _, ok := textBy[name]; !ok {
				textBy[name] = st.Name
			}
		}
	}
	var out []SavedJSONIssue
	mixed := make([]string, 0)
	for name := range jsonBy {
		if _, ok := textBy[name]; ok {
			mixed = append(mixed, name)
		}
	}
	sort.Strings(mixed)
	for _, name := range mixed {
		out = append(out, SavedJSONIssue{Step: textBy[name], Why: fmt.Sprintf(
			"variable %q is saved as text here and with the {\"json\": …} form in step %q — one variable has one kind; "+
				"use a second name for the other form", name, jsonBy[name])})
	}
	if len(jsonBy) == 0 {
		return out
	}
	refused := func(step, where, text string) {
		seen := map[string]bool{}
		for _, m := range savedAnyRe.FindAllStringSubmatch(text, -1) {
			if _, isJSON := jsonBy[m[1]]; isJSON && !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, SavedJSONIssue{Step: step, Why: SavedJSONPlacementSentence(where, m[1])})
			}
		}
	}
	for _, st := range steps {
		if st.Type == "http" {
			refused(st.Name, "the url", st.URL)
		}
		if st.Type == "db" {
			// a param is data, and a JSON structure is not a text value to bind. (The query text takes
			// no saved value at all: DBStepQueryPlaceholderProblems.)
			params, _ := DBStepParams(st.Params)
			for i, p := range params {
				refused(st.Name, fmt.Sprintf("the db step's param $%d", i+1), p)
			}
		}
		if st.Type == "logs" {
			// `contains` is text the line must hold, and a JSON structure is not a text value.
			refused(st.Name, "the logs step's `contains`", st.Contains)
		}
		if st.Type == "bus" {
			// a `where` value is text the message must hold, and a JSON structure is not a text value.
			where, _ := BusStepWhere(st.Where)
			for path, vals := range where {
				for _, v := range vals {
					refused(st.Name, fmt.Sprintf("the bus step's `where` value of %q", path), v)
				}
			}
		}
		hdrs, _ := HeaderValues(st.Headers)
		keys := make([]string, 0, len(hdrs))
		for k := range hdrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			refused(st.Name, "the header "+fmt.Sprintf("%q", k), hdrs[k])
		}
		for _, c := range claims[st.Name] {
			refused(st.Name, "a claim ("+strings.TrimSpace(c)+")", c)
		}
	}
	return out
}

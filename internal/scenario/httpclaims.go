package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// — ONE PARSER FOR AN `http` CHAIN STEP'S CLAIMS, USED BY THE VALIDATOR AND THE EXECUTOR.
//
// Before this, Validate ran ParseBodyAsserts only on bullets that OPEN with `body`, and a chain bullet
// opens with `step <name>:`. Claim grammar was checked at write time for `amqp` steps (AMQPStepClaims)
// and `mcp` steps (argus.ExpectProblems) but never for `http` steps, so a malformed numeric claim was
// accepted by author_validate_scenario / author_write_scenario and then REFUSED by the executor
// before any step ran. argus.httpStepExpect now delegates here, and validate.go calls it per step, so
// the two cannot disagree again (TestHTTPStepClaims_ValidatorAndExecutorAgree).

// HTTPStepClaims turns a chain http step's claims into (wantStatus, bodyWant): the `status=<code>` form
// (DeclaredStatuses, VR12-E7) and the `body …` forms (ParseBodyAsserts, VR12-E8). wantStatus 0 means no
// status claim was declared. A claim that is neither form, or a body claim that does not parse, is
// refused BY NAME — never silently read as "expect success".
func HTTPStepClaims(claims []string) (wantStatus int, bodyWant []BodyAssert, err error) {
	// `unreachable` is judged by its own step (chain.UnreachableHTTPStep); alone it
	// declares no status and no body claim, beside anything else it is refused.
	if StepClaimsUnreachable(claims) {
		if why := unreachableClaimProblem(claims); why != "" {
			return 0, nil, errors.New(why)
		}
		return 0, nil, nil
	}
	for _, c := range claims {
		if ClaimsToBeStatus(c) || ClaimsToBeBodyAssert(c) || ClaimsToBeElapsed(c) {
			continue
		}
		return 0, nil, fmt.Errorf("EXPECT bullet %q is not a recognised http-step assertion — write "+
			"`status=<code>`, `elapsed < <N>ms` or a body assertion (`body has <field> containing <value>`, `body contains "+
			"<value>`, …)", c)
	}
	// `status=200/201` is refused, not read as `status=200` with the 201 dropped. For a chain
	// http step this is the one door — the validator (per step) and the executor (httpStepExpect) both come here.
	if err = CheckDeclaredStatuses(claims); err != nil {
		return 0, nil, err
	}
	// the timing claim is validated here, by the parser the executor uses.
	if _, err = HTTPStepElapsed(claims); err != nil {
		return 0, nil, err
	}
	wantStatus, _ = DeclaredStatuses(claims)
	bodyWant, berrs := ParseBodyAsserts(claims)
	if len(berrs) > 0 {
		return 0, nil, berrs[0]
	}
	return wantStatus, bodyWant, nil
}

// ── P4 — `save` ────────────────────────────────────────────────────────────────────────────────────

// SaveSpec is ONE entry of a chain step's `save`: a JSON dot-path into the response (the wire form
// `"n": "count"`, unchanged), a regex over a text body (`"n": {"regex": "total (\\d+)"}`), whose FIRST
// capture group of the FIRST match is saved, or a JSON value saved AS JSON (`"n": {"json": "items"}`).
// Invalid carries a wire shape that is none of the three, so the validator can refuse
// it BY NAME (decoding never fails the whole step with a bare Go type error).
type SaveSpec struct {
	Path  string
	Regex string
	// JSON is the `{"json": "<path>"}` form: Path is then the path whose value is saved as JSON text (an
	// object, an array, a string with its quotes, a number with every digit, true/false/null); the single
	// character `$` is the whole response. A flag rather than "Path is non-empty", so an empty
	// `{"json": ""}` is told apart from the plain string form and refused by name (SaveProblem).
	JSON bool
	// From names the document the entry reads: "" = the step's response as on any step,
	// SaveFromExtracted = the document an mcp step's `extract` cut out. Written `{"json": …, "from": "extracted"}`.
	From    string
	Invalid string
}

// SaveFromExtracted is the one value `from` accepts.
const SaveFromExtracted = "extracted"

// saveShapes names the three legal shapes of a save entry; the Invalid sentence carries it and nothing else does.
const saveShapes = "must be a JSON path string, an object {\"json\": \"<path, or $ for the whole response>\"} " +
	"(saves the value as JSON) or an object {\"regex\": \"<pattern with one capture group>\"}"

// UnmarshalJSON accepts a string (Path) or an object whose only key is "json" or "regex" holding a string.
func (s *SaveSpec) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = SaveSpec{Path: str}
		return nil
	}
	bad := saveShapes
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		*s = SaveSpec{Invalid: bad}
		return nil
	}
	// the one-key shapes are the long-standing ones; a second key is allowed only when it is `from`
	from := ""
	if rawFrom, has := obj["from"]; has {
		if len(obj) != 2 || json.Unmarshal(rawFrom, &from) != nil {
			*s = SaveSpec{Invalid: bad}
			return nil
		}
		delete(obj, "from")
		if from == "" {
			from = "(empty)" // present but empty: SaveProblem refuses it by name
		}
	}
	if len(obj) != 1 {
		*s = SaveSpec{Invalid: bad}
		return nil
	}
	if raw, ok := obj["json"]; ok {
		var p string
		if json.Unmarshal(raw, &p) != nil {
			*s = SaveSpec{Invalid: bad}
			return nil
		}
		*s = SaveSpec{Path: p, JSON: true, From: from}
		return nil
	}
	raw, ok := obj["regex"]
	var re string
	if !ok || json.Unmarshal(raw, &re) != nil {
		*s = SaveSpec{Invalid: bad}
		return nil
	}
	*s = SaveSpec{Regex: re, From: from}
	return nil
}

// MarshalJSON writes the wire form back (Path as a string, Regex and JSON as their objects).
func (s SaveSpec) MarshalJSON() ([]byte, error) {
	if s.Regex != "" {
		m := map[string]string{"regex": s.Regex}
		if s.From != "" {
			m["from"] = s.From
		}
		return json.Marshal(m)
	}
	if s.JSON {
		m := map[string]string{"json": s.Path}
		if s.From != "" {
			m["from"] = s.From
		}
		return json.Marshal(m)
	}
	return json.Marshal(s.Path)
}

// SaveProblem is the ONE judgement of a save entry, shared by the validator and the executor's second
// door: "" when it is usable, else the reason, naming the variable. A regex must compile (Go RE2) and
// carry exactly one capture group — the value to save. A json path must not be empty.
func SaveProblem(varName string, sp SaveSpec) string {
	if sp.Invalid != "" {
		return fmt.Sprintf("save for variable %q %s", varName, sp.Invalid)
	}
	if sp.From != "" && sp.From != SaveFromExtracted {
		return fmt.Sprintf("save for variable %q declares \"from\" with a value other than %q - %q is the only document a save can "+
			"read besides the step's own response (omit \"from\" for the response)", varName, SaveFromExtracted, SaveFromExtracted)
	}
	if sp.JSON {
		if strings.TrimSpace(sp.Path) == "" {
			return fmt.Sprintf("save for variable %q declares an empty json path — write {\"json\": \"<path>\"}, "+
				"or {\"json\": \"$\"} for the whole response", varName)
		}
		return ""
	}
	if sp.Regex == "" {
		return ""
	}
	re, err := regexp.Compile(sp.Regex)
	if err != nil {
		return fmt.Sprintf("save for variable %q declares a regex that does not compile (%v)", varName, err)
	}
	if n := re.NumSubexp(); n != 1 {
		return fmt.Sprintf("save for variable %q declares a regex with %d capture group(s) — it must have "+
			"exactly one, the value to save (use `(?:…)` for grouping that should not capture)", varName, n)
	}
	return ""
}

// SaveProblems lists SaveProblem for every entry of a step's save, in variable-name order.
func SaveProblems(save map[string]SaveSpec) []string {
	names := make([]string, 0, len(save))
	for k := range save {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		if why := SaveProblem(k, save[k]); why != "" {
			out = append(out, why)
		}
	}
	return out
}

// savedThresholdProblems is P2's static half: a numeric claim whose threshold is `${saved.<var>}` is
// refused at write time when NO EARLIER step of the chain saves <var>. Whether the value is a number
// can only be known at run time, where the step fails by name.
func savedThresholdProblems(steps []ChainStep, claims map[string][]string) []string {
	var out []string
	saved := map[string]bool{}
	for _, st := range steps {
		if st.Type == "mcp" || st.Type == "http" || st.Type == "amqp" || st.Type == "db" || st.Type == "logs" || st.Type == "bus" {
			for _, c := range claims[st.Name] {
				if !ClaimsToBeBodyAssert(c) {
					continue
				}
				asserts, _ := parseBodyAsserts([]string{c}, st.Type == "mcp")
				for _, a := range asserts {
					if !mcp.IsNumericOp(a.Op) {
						continue
					}
					name, whole := SavedRefWhole(strings.TrimSpace(a.Value))
					if whole && !saved[name] {
						out = append(out, fmt.Sprintf("step %q: the claim %q compares against ${saved.%s}, but no "+
							"earlier step saves %q — add `\"save\":{%q:\"<path>\"}` to a step that runs before it",
							st.Name, c, name, name, name))
					}
				}
			}
		}
		for k := range st.Save {
			saved[k] = true
		}
	}
	return out
}

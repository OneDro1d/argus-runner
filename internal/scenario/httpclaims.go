package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// — ONE PARSER FOR AN `http` CHAIN STEP'S CLAIMS, USED BY THE VALIDATOR AND THE EXECUTOR.
//
// Before this, Validate ran ParseBodyAsserts only on bullets that OPEN with `body`, and a chain bullet
// opens with `step <name>:`. Claim grammar was checked at write time for `amqp` steps (AMQPStepClaims)
// and `mcp` steps (argus.ExpectProblems) but never for `http` steps, so a malformed numeric claim was
// accepted by author__validate_scenario / author__write_scenario and then REFUSED by the executor
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
		if ClaimsToBeStatus(c) || ClaimsToBeBodyAssert(c) {
			continue
		}
		return 0, nil, fmt.Errorf("EXPECT bullet %q is not a recognised http-step assertion — write "+
			"`status=<code>` or a body assertion (`body has <field> containing <value>`, `body contains "+
			"<value>`, …)", c)
	}
	wantStatus, _ = DeclaredStatuses(claims)
	bodyWant, berrs := ParseBodyAsserts(claims)
	if len(berrs) > 0 {
		return 0, nil, berrs[0]
	}
	return wantStatus, bodyWant, nil
}

// ── P4 — `save` ────────────────────────────────────────────────────────────────────────────────────

// SaveSpec is ONE entry of a chain step's `save`: either a JSON dot-path into the response (the wire
// form `"n": "count"`, unchanged) or a regex over a text body (`"n": {"regex": "total (\\d+)"}`), whose
// FIRST capture group of the FIRST match is saved. Invalid carries a wire shape that is neither, so
// the validator can refuse it BY NAME (decoding never fails the whole step with a bare Go type error).
type SaveSpec struct {
	Path    string
	Regex   string
	Invalid string
}

// UnmarshalJSON accepts a string (Path) or an object whose only key is "regex" holding a string.
func (s *SaveSpec) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = SaveSpec{Path: str}
		return nil
	}
	bad := "must be a JSON path string or an object {\"regex\": \"<pattern with one capture group>\"}"
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || len(obj) != 1 {
		*s = SaveSpec{Invalid: bad}
		return nil
	}
	raw, ok := obj["regex"]
	var re string
	if !ok || json.Unmarshal(raw, &re) != nil {
		*s = SaveSpec{Invalid: bad}
		return nil
	}
	*s = SaveSpec{Regex: re}
	return nil
}

// MarshalJSON writes the wire form back (Path as a string, Regex as the object).
func (s SaveSpec) MarshalJSON() ([]byte, error) {
	if s.Regex != "" {
		return json.Marshal(map[string]string{"regex": s.Regex})
	}
	return json.Marshal(s.Path)
}

// SaveProblem is the ONE judgement of a save entry, shared by the validator and the executor's second
// door: "" when it is usable, else the reason, naming the variable. A regex must compile (Go RE2) and
// carry exactly one capture group — the value to save.
func SaveProblem(varName string, sp SaveSpec) string {
	if sp.Invalid != "" {
		return fmt.Sprintf("save for variable %q %s", varName, sp.Invalid)
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
		if st.Type == "mcp" || st.Type == "http" || st.Type == "amqp" {
			for _, c := range claims[st.Name] {
				if !ClaimsToBeBodyAssert(c) {
					continue
				}
				asserts, _ := ParseBodyAsserts([]string{c})
				for _, a := range asserts {
					if a.Op != BodyGT && a.Op != BodyGTE && a.Op != BodyLT && a.Op != BodyLTE {
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

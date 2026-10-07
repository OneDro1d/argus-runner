package chain

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// / — `${saved.<var>}` IN A STEP'S CLAIMS, FOR EVERY STEP TYPE.
//
// Only MCPStep bound the capture store into its claims; an http step compared `body contains
// ${saved.x}` against the literal placeholder text, and an amqp consume step likewise. One binder now
// serves all three. The claim as WRITTEN is what a report shows (assertions_enforced): the bound copy
// exists only for the judge, so a saved value is never printed.

// savedNotNumberError is the by-name failure of a numeric comparison whose threshold came from the
// capture store and is not a number. It names the VARIABLE and never the value (the value is the
// SUT's answer; the builder hat reads this text).
type savedNotNumberError struct{ name string }

func (e savedNotNumberError) Error() string {
	return "the saved value of `" + e.name + "` is not a number — the numeric comparison against it " +
		"cannot be made (the value is held out)"
}

func isNumericBodyOp(op string) bool {
	switch op {
	case mcp.BodyGTOp, mcp.BodyGTEOp, mcp.BodyLTOp, mcp.BodyLTEOp:
		return true
	}
	return false
}

// bindBodyAsserts returns a per-execution COPY of asserts with ${saved.<var>} bound into every Value
// (bindSaved: the args' grammar, the args' error naming every unbound variable). A numeric comparison
// whose threshold is one whole ${saved.<var>} must bind to a number, else savedNotNumberError.
func bindBodyAsserts(asserts []mcp.BodyAssert, vars map[string]string) ([]mcp.BodyAssert, error) {
	if len(asserts) == 0 {
		return asserts, nil
	}
	out := make([]mcp.BodyAssert, len(asserts))
	copy(out, asserts)
	for i := range out {
		written := out[i].Value
		v, err := bindSaved(written, vars)
		if err != nil {
			return nil, err
		}
		if isNumericBodyOp(out[i].Op) {
			if name, whole := scenario.SavedRefWhole(written); whole {
				f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
				if perr != nil || math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, savedNotNumberError{name: name}
				}
			}
		}
		out[i].Value = v
	}
	return out, nil
}

// keepSavedThresholdsAsWritten returns bound with every numeric assertion whose threshold is a whole
// ${saved.<var>} put back AS WRITTEN (from written), so a report never shows the saved value.
func keepSavedThresholdsAsWritten(written, bound mcp.Expect) mcp.Expect {
	if len(written.Body) != len(bound.Body) {
		return bound
	}
	out := bound
	out.Body = make([]mcp.BodyAssert, len(bound.Body))
	copy(out.Body, bound.Body)
	for i, w := range written.Body {
		if _, whole := scenario.SavedRefWhole(w.Value); whole && isNumericBodyOp(w.Op) {
			out.Body[i] = w
		}
	}
	return out
}

// needsBodyAsserts is the rule-3 question ("can this step be given its inputs at all?") for a step's
// claims: an UNSAVED variable means not-measured, but a saved value that is merely not a number is a
// FAILURE of the step (it fired its inputs and they were wrong), so that error is let through here
// and surfaces from Run.
func needsBodyAsserts(asserts []mcp.BodyAssert, vars map[string]string) error {
	_, err := bindBodyAsserts(asserts, vars)
	var nn savedNotNumberError
	if errors.As(err, &nn) {
		return nil
	}
	return err
}

// ─────────────────────────────────────────────────────────────────────────────────────────
// / — THE PER-CLAIM RECORD OF A FAILED STEP (report.StepResult.FailedClaims).
//
// A step that failed on its claims used to keep one joined `expected` and one observed sentence whose
// closing words sent the author to "the failure record" for the per-claim detail — a record that
// did not exist. failedBodyClaims builds it, from the SAME judgement the step made (mcp.BodyAssert
// Observation is bodyAssertMiss plus what it saw), so a claim is listed exactly when the judge
// counted it as a miss, and a claim that held is never listed.
//
// ⛔ AUTHOR-ONLY. The entries carry the SUT's observed values; redactExpected nils the field for the
// product hat and nothing else copies it (see report.StepResult.FailedClaims). What is written here is
// the claim AS WRITTEN — the `written` slice, never the bound copy — and an observed text that
// contains the value a claim's `${saved.<var>}` was bound to is shown as the placeholder instead.
// ─────────────────────────────────────────────────────────────────────────────────────────

// chainClaimsNote closes the Observed text of a chain step's content-claim miss. It replaces
// mcp.ClaimsHeldOutNote, which points at failure.expected: for a chain step that only joins the
// claims, and the per-claim record is the step's failed_claims.
const chainClaimsNote = "(the asserted value is held out; the test hat reads the claims that did not hold, with what was observed, in this step's failed_claims)"

// chainBodyNote rewrites an mcp judge's content-miss Observed text for a chain step.
func chainBodyNote(observed string) string {
	return strings.Replace(observed, mcp.ClaimsHeldOutNote, chainClaimsNote, 1)
}

// failedBodyClaims lists, in written order and capped at report.MaxFailedClaims, each of the step's
// content claims that did not hold against text. written and bound are the same claims before and
// after ${saved.<var>} binding (bindBodyAsserts keeps the length); vars is the capture store the
// binding read; scrub (may be nil) cleans an observed value of anything that must not be printed
// (the amqp step's broker URL).
func failedBodyClaims(text string, written, bound []mcp.BodyAssert, vars map[string]string, scrub func(string) string) []report.FailedClaim {
	if len(written) != len(bound) {
		return nil
	}
	var out []report.FailedClaim
	for i := range bound {
		if len(out) >= report.MaxFailedClaims {
			break
		}
		miss, observed := mcp.BodyAssertObservation(text, bound[i])
		if !miss {
			continue
		}
		if scrub != nil {
			observed = scrub(observed)
		}
		// A numeric claim's whole-placeholder threshold was validated as a finite number by
		// bindBodyAsserts, so that variable is not a credential: its observed number is shown as is
		// (replacing it would hide an equal value and garble a longer one). Every other saved
		// variable is redacted as before.
		own := ownNumericSaved(written[i])
		observed = redactSavedIn(observed, written[i], vars, own)
		observed = redactEverySaved(observed, vars, own)
		out = append(out, report.FailedClaim{
			Claim:    enforcedAssertions(mcp.Expect{Body: []mcp.BodyAssert{written[i]}})[0],
			Observed: report.TruncateObserved(observed),
		})
	}
	return out
}

// ownNumericSaved names the saved variable a numeric claim's threshold is, when the threshold is one
// whole ${saved.<var>}; "" for any other claim.
func ownNumericSaved(written mcp.BodyAssert) string {
	if !isNumericBodyOp(written.Op) {
		return ""
	}
	if name, whole := scenario.SavedRefWhole(written.Value); whole {
		return name
	}
	return ""
}

// minRedactedSavedLen is the shortest saved value redactEverySaved replaces when the claim does not
// reference it. A saved value can be a credential an earlier step captured (a login token), and an
// unscoped claim's observed text is the whole answer, which may echo it. Shorter values (a count, a
// status) are left alone: replacing every "3" in an answer would garble it, and a value that short is
// not a credential.
const minRedactedSavedLen = 6

// redactEverySaved puts back, as its placeholder, every saved value of at least minRedactedSavedLen
// bytes, whichever claim it belongs to, longest first so a value inside a longer one does not break
// the longer one's replacement. Plain and JSON-escaped spellings, as redactSavedIn. exempt (may be "")
// is the one variable left alone: the claim's own numeric threshold.
func redactEverySaved(observed string, vars map[string]string, exempt string) string {
	names := make([]string, 0, len(vars))
	for name, v := range vars {
		if len(v) >= minRedactedSavedLen && name != exempt {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if len(vars[names[i]]) != len(vars[names[j]]) {
			return len(vars[names[i]]) > len(vars[names[j]])
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		v, placeholder := vars[name], "${saved."+name+"}"
		observed = strings.ReplaceAll(observed, v, placeholder)
		if b, err := json.Marshal(v); err == nil {
			if esc := strings.Trim(string(b), `"`); esc != v {
				observed = strings.ReplaceAll(observed, esc, placeholder)
			}
		}
	}
	return observed
}

// redactSavedIn puts back, as its placeholder, every occurrence of the value a `${saved.<var>}` in
// the claim was bound to — in both its plain and its JSON-escaped spelling (bindSaved splices the
// escaped one). Done BEFORE the observed value is cut to its length limit, so a value that straddles
// the cut is never half-shown. exempt (may be "") is a variable left alone, as in redactEverySaved.
func redactSavedIn(observed string, written mcp.BodyAssert, vars map[string]string, exempt string) string {
	for _, m := range savedRefRe.FindAllStringSubmatch(written.Value, -1) {
		name := m[1]
		v, ok := vars[name]
		if !ok || v == "" || name == exempt {
			continue
		}
		placeholder := "${saved." + name + "}"
		observed = strings.ReplaceAll(observed, v, placeholder)
		if b, err := json.Marshal(v); err == nil {
			if esc := strings.Trim(string(b), `"`); esc != v {
				observed = strings.ReplaceAll(observed, esc, placeholder)
			}
		}
	}
	return observed
}

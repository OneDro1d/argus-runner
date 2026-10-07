package scenario

import (
	"fmt"
	"regexp"
)

// V31-003 (VR13-CID) — WHICH PLACEHOLDERS A CHECK MAY CARRY, AND WHO FILLS THEM IN.
//
// A check's value is PRINTED IN THE REPORT — `assertions_enforced` and `failure.expected` both carry
// it — so the set is deliberately tiny and an environment variable is not in it: filling `${TOKEN}`
// into a check would publish the token. The run-scoped id IS in it, because a scenario that writes
// `marker-${cid}` and reads it back is the pattern the authoring skill teaches.

// CorrelationIDPlaceholders are the two names of the scenario's correlation id — the ONLY placeholders a check may carry, apart
// from a chain's ${saved.<var>}. internal/argus fills them in (fillCorrelationID); Validate refuses anything else.
var CorrelationIDPlaceholders = []string{"${cid}", "${correlation_id}"}

// Cid8Placeholder (AC-D20) is a SHORT, DERIVED form of the correlation id: 8 lowercase hex
// characters, deterministic per run. It exists for a resource name a SUT bounds tightly — a Coder
// workspace name is at most 32 chars, `[a-z0-9-]` — where the full `tr-<run_id>-<scenario_id>-<hex>`
// id would overflow it. internal/argus derives it (correlationid.go); it resolves everywhere
// ${cid}/${correlation_id} do, and Validate treats it exactly the same way in a check.
const Cid8Placeholder = "${cid8}"

// SavedRefPattern is the ${saved.<var>} grammar the chain runtime binds. internal/chain/capture.go compiles THIS
// string (capture.go:34 changes to regexp.MustCompile(scenario.SavedRefPattern)), so the validator can never accept
// a reference the runtime would leave literal.
const SavedRefPattern = `\$\{saved\.([A-Za-z0-9_-]+)\}`

var (
	checkPlaceholderRe = regexp.MustCompile(`\$\{[^}]*\}?`) // an unclosed `${cid` is caught too
	savedRefWholeRe    = regexp.MustCompile(`^` + SavedRefPattern + `$`)
)

// SavedRefWhole reports whether s is, in its entirety, ONE `${saved.<var>}` reference, and returns the
// variable name. It is the test for a numeric claim's threshold: `${saved.n}` is
// bound at run time, but `${saved.n}0` or `1${saved.n}` is neither a number nor a whole reference.
func SavedRefWhole(s string) (string, bool) {
	m := savedRefWholeRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

const unfilledPlaceholderMessage = "`%s` in a check is never filled in — only `${cid}`, `${correlation_id}`, `${cid8}` and, in a chain, `${saved.<var>}` are. An environment variable is never put into a check, because its value would be printed in the report."

// UnfilledPlaceholders returns every ${…} in text that nothing fills in — anything but ${cid}, ${correlation_id} and,
// in a chain, a well-formed ${saved.<var>} — in order, exactly as written (an unclosed one included).
func UnfilledPlaceholders(text string, chain bool) []string {
	var out []string
	for _, ph := range checkPlaceholderRe.FindAllString(text, -1) {
		if contains(CorrelationIDPlaceholders, ph) || ph == Cid8Placeholder || (chain && savedRefWholeRe.MatchString(ph)) {
			continue
		}
		out = append(out, ph)
	}
	return out
}

// UnfilledPlaceholderError is the one message the validator and the chain runtime give (fix step 5a).
func UnfilledPlaceholderError(ph string) string { return fmt.Sprintf(unfilledPlaceholderMessage, ph) }

// checkPlaceholderErrors is V31-003: a `### Runnable` bullet carrying a ${…} that nothing fills in is refused.
//
// ⚠ RUNNABLE ONLY. A `### Non-runnable` line is documentation — nothing fills it in because nothing
// runs it — and refusing an author for writing "the token is read from ${TOKEN}" under the heading
// that exists for exactly that kind of sentence would be the rule biting its own purpose.
func checkPlaceholderErrors(s *Scenario, text string, expLine int) []Error {
	var errs []Error
	chain := contains(s.Tags, ChainTag)
	for _, b := range s.RunnableExpect() {
		for _, ph := range UnfilledPlaceholders(b, chain) {
			errs = append(errs, Error{Line: lineContaining(text, bulletText(b), expLine), Message: UnfilledPlaceholderError(ph)})
		}
	}
	return errs
}

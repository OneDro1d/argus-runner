package scenario

import (
	"fmt"
	"strings"
)

// — THE `unreachable` CLAIM OF A CHAIN http STEP.
//
// Argus had no way to assert "this connection must fail": an unreachable target is `error` /
// `not-measured`, never a pass, deliberately. `- step <name>: unreachable` is the one place a
// transport failure is the EXPECTED answer. It passes only when NO HTTP response arrived because of a
// transport failure (connect timeout, refused, reset, no route); any HTTP status fails it, and so does
// a DNS "no such host" (a typo must not read as a blocked path). The run-time judge is
// internal/chain.UnreachableHTTPStep; this file is the shared grammar, so the validator and the
// executor cannot disagree about what the claim is.

// UnreachableClaim is the claim's one spelling (matched case-insensitively, whitespace-trimmed).
const UnreachableClaim = "unreachable"

// ClaimsToBeUnreachable reports whether a claim is exactly the `unreachable` form.
func ClaimsToBeUnreachable(claim string) bool {
	return strings.EqualFold(strings.TrimSpace(claim), UnreachableClaim)
}

// StepClaimsUnreachable reports whether any of a step's claims is the `unreachable` form.
func StepClaimsUnreachable(claims []string) bool {
	for _, c := range claims {
		if ClaimsToBeUnreachable(c) {
			return true
		}
	}
	return false
}

// unreachableClaimProblem is the ONE judgement of an http step's `unreachable` claim, shared by
// HTTPStepClaims (validator and executor both call it): "" when the step does not claim it or claims it
// alone, else the reason.
func unreachableClaimProblem(claims []string) string {
	if !StepClaimsUnreachable(claims) {
		return ""
	}
	if len(claims) > 1 {
		return fmt.Sprintf("`unreachable` cannot be combined with another claim on the same step (it says no "+
			"HTTP response arrived at all, so a status or body claim beside it could never hold); this step "+
			"declares %d claims", len(claims))
	}
	return ""
}

// unreachableStepErrors refuses what the claim may not sit beside: `poll` and `always` on its own
// step, and the claim itself on any step that is not an `http` step (where it would have no assertion
// operator and be read as prose — a claim that proves nothing).
func unreachableStepErrors(steps []ChainStep, claims map[string][]string, trigLine int) []Error {
	var errs []Error
	positiveBefore := false // a step that does NOT claim `unreachable`: only such a step opens the run-time guard
	for _, st := range steps {
		if !StepClaimsUnreachable(claims[st.Name]) {
			positiveBefore = true
			continue
		}
		if !positiveBefore {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q claims `unreachable` but no earlier step "+
				"of the chain is a positive one: the claim is judged only after an earlier step reached the system, so "+
				"this step could only ever read not-measured. Put a step that reaches the system before it", st.Name)})
		}
		if st.Type != "http" {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q is a %q step: `unreachable` is a "+
				"claim for an http step only — on any other step it would be read as prose and prove nothing "+
				"", st.Name, st.Type)})
			continue
		}
		if st.Poll != nil {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q claims `unreachable` and also declares "+
				"`poll` — a poll retries until the claim holds; an unreachable claim is judged once", st.Name)})
		}
		if st.Always {
			errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q claims `unreachable` and also declares "+
				"`always` — a cleanup step is exempt from the earlier-step gate the claim depends on", st.Name)})
		}
	}
	return errs
}

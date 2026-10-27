package scenario

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// THE `elapsed` CLAIM OF A CHAIN http STEP.
//
// `- step <name>: elapsed < 500ms` (or `<=`; `500 ms` and `1s` are also spelled) says the step's ONE HTTP
// request answered inside the bound. It measures from just before the request is sent to the moment the
// whole response body has been read (chain.HTTPStep), and with `poll` it judges the LAST attempt, the one
// whose claims are judged, never the poll's total.
//
// ⛔ The bound is an EXPECTED value. It lives in the claim text (author-only, like every claim) and in the
// failed-claim record; it never reaches a builder-readable string. This file is only the shared grammar, so
// the validator and the executor cannot disagree about what the claim is.

// MaxElapsedBoundMs is the largest bound a claim may declare: ten minutes, the same ceiling as a poll.
const MaxElapsedBoundMs = 600000

// ElapsedClaim is a parsed `elapsed` claim.
type ElapsedClaim struct {
	MaxMs     int  // the bound, in milliseconds
	Inclusive bool // `<=` (true) or `<` (false)
}

// String renders the claim the way assertions_enforced and failed_claims show it (author-only).
func (c ElapsedClaim) String() string {
	op := "<"
	if c.Inclusive {
		op = "<="
	}
	return fmt.Sprintf("elapsed %s %d ms", op, c.MaxMs)
}

// Holds reports whether a measured duration satisfies the claim.
// It compares the unrounded duration, so `<= 500ms` does not pass 500.4 ms because the report rounds it to 500.
func (c ElapsedClaim) Holds(took time.Duration) bool {
	bound := time.Duration(c.MaxMs) * time.Millisecond
	if c.Inclusive {
		return took <= bound
	}
	return took < bound
}

var (
	elapsedOpenRe = regexp.MustCompile(`(?i)^\s*elapsed\b`)
	elapsedRest   = regexp.MustCompile(`^([0-9]+)\s*([A-Za-z]*)$`)
)

// ClaimsToBeElapsed reports whether a claim OPENS with the word `elapsed` — it is claiming to be a timing
// claim, whether or not it parses as one (the same contract as ClaimsToBeBodyAssert).
func ClaimsToBeElapsed(claim string) bool { return elapsedOpenRe.MatchString(claim) }

// StepClaimsElapsed reports whether any of a step's claims is an `elapsed` claim.
func StepClaimsElapsed(claims []string) bool {
	for _, c := range claims {
		if ClaimsToBeElapsed(c) {
			return true
		}
	}
	return false
}

// ParseElapsedClaim reads one `elapsed` claim, refusing anything that is not the form BY NAME.
func ParseElapsedClaim(claim string) (ElapsedClaim, error) {
	clause := strings.TrimSpace(claim)
	rest := strings.TrimSpace(clause[len("elapsed"):])
	var c ElapsedClaim
	switch {
	case strings.HasPrefix(rest, "<="):
		c.Inclusive = true
		rest = strings.TrimSpace(rest[2:])
	case strings.HasPrefix(rest, "<"):
		rest = strings.TrimSpace(rest[1:])
	default:
		return c, fmt.Errorf("EXPECT bullet %q declares an `elapsed` claim with an operator this grammar does not support — "+
			"a timing claim is an upper bound: write `elapsed < <N>ms` or `elapsed <= <N>ms` (also `<N> ms` and `<N>s`)", clause)
	}
	m := elapsedRest.FindStringSubmatch(rest)
	if m == nil {
		return c, fmt.Errorf("EXPECT bullet %q declares an `elapsed` bound that is not a positive whole number followed by a "+
			"unit — write `elapsed < 500ms`, `elapsed < 500 ms` or `elapsed <= 2s`", clause)
	}
	n, _ := strconv.Atoi(m[1])
	switch strings.ToLower(m[2]) {
	case "ms":
	case "s":
		n *= 1000
	case "":
		return c, fmt.Errorf("EXPECT bullet %q declares an `elapsed` bound with no unit — write `ms` or `s` (e.g. `elapsed < 500ms`)", clause)
	default:
		return c, fmt.Errorf("EXPECT bullet %q declares an `elapsed` bound in the unit %q, which this grammar does not support — "+
			"use `ms` or `s`", clause, m[2])
	}
	if n <= 0 || n > MaxElapsedBoundMs {
		return c, fmt.Errorf("EXPECT bullet %q declares an `elapsed` bound of %d ms — it must be a positive whole number of at most %d ms",
			clause, n, MaxElapsedBoundMs)
	}
	c.MaxMs = n
	return c, nil
}

// HTTPStepElapsed returns the step's `elapsed` claim, nil when it declares none. A second one is refused:
// two bounds on one measurement are one bound and a contradiction waiting to be written.
func HTTPStepElapsed(claims []string) (*ElapsedClaim, error) {
	var found *ElapsedClaim
	for _, c := range claims {
		if !ClaimsToBeElapsed(c) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("a step declares more than one `elapsed` claim (%q is the second) — a step measures one request, "+
				"so it takes one bound", strings.TrimSpace(c))
		}
		ec, err := ParseElapsedClaim(c)
		if err != nil {
			return nil, err
		}
		found = &ec
	}
	return found, nil
}

// elapsedStepErrors refuses the claim on any step that is not an `http` step (where it would be read as
// prose and prove nothing), naming the step.
func elapsedStepErrors(steps []ChainStep, claims map[string][]string, trigLine int) []Error {
	var errs []Error
	for _, st := range steps {
		if st.Type == "http" || !StepClaimsElapsed(claims[st.Name]) {
			continue
		}
		errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf("step %q is a %q step: `elapsed` is a claim for an http "+
			"step only — it measures the one HTTP request of the step, and on any other step it would be read as prose and prove "+
			"nothing", st.Name, st.Type)})
	}
	return errs
}

package scenario

import (
	"regexp"
	"strconv"
	"strings"
)

// statusDeclRe is the DECLARED status form, ANCHORED at the start of the bullet:
//
//   - status=202      -> the expected status
//   - status2=409     -> the expected status of a SECOND request with a shared Idempotency-Key
//
// VR12-E7. It replaces a scrape that read every bullet for "status"/"code" plus any 3-digit number
// and treated a SECOND hit as "fire the request twice". That scrape was not a theoretical hazard:
// PERM-001-unauth-order-401 carries three bullets describing three VARIANTS of one unauthorized
// request, each ending "status=401" — the scrape found three 401s and the runner has been sending
// that request TWICE, with an Idempotency-Key, on every run.
//
// Anchoring is what makes the form declared rather than inferred: a sentence that merely MENTIONS a
// status no longer sets one. Measured before the change: 31 of the 33 scenarios with a status in
// their runnable bullets already used this exact form; the two that did not are ORDE-013 (a genuine
// idempotency test) and PERM-001 (the defect), and both were migrated with that commit.
//
// ⛔ IT LIVES HERE, NOT IN internal/argus. VR12-E6 refuses a code-judged scenario that declares no
// status, and tier 2 must read the declaration with the EXACT reader the runtime uses — a validator
// with its own copy of this regex would accept forms the runner ignores, or refuse forms it honours.
var statusDeclRe = regexp.MustCompile(`(?i)^\s*status(2)?\s*[=:]\s*([1-5][0-9][0-9])\b`)

// DeclaredStatuses reads the declared status forms from the RUNNABLE bullets only.
// second is 0 when no `status2=` was declared — and a second request is sent ONLY when it was.
func DeclaredStatuses(runnable []string) (first, second int) {
	for _, e := range runnable {
		m := statusDeclRe.FindStringSubmatch(strings.TrimSpace(e))
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		if m[1] == "2" {
			if second == 0 {
				second = n
			}
			continue
		}
		if first == 0 {
			first = n
		}
	}
	return first, second
}

// ClaimsToBeStatus reports whether a single bullet/clause OPENS with the declared-status form
// (`status=`/`status2=`), matching statusDeclRe — the same "does this clause claim to be mine"
// classifier ClaimsToBeBodyAssert and ClaimsToBeChainClaim are (AC-D20: an http chain step's claim
// is either this or a body assertion; anything else is refused rather than silently ignored).
func ClaimsToBeStatus(clause string) bool {
	return statusDeclRe.MatchString(strings.TrimSpace(clause))
}

// DeclaresStatus reports whether the scenario declares an expected first status in `### Runnable`.
func DeclaresStatus(s *Scenario) bool {
	if s == nil {
		return false
	}
	first, _ := DeclaredStatuses(s.RunnableExpect())
	return first > 0
}

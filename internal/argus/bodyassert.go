package argus

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-E8 (V29-017) — the body-assertion GRAMMAR moved to internal/scenario, so that R3 (an
// unparseable body bullet is refused) reaches ALL THREE author paths instead of only the one that
// calls argus.ExpectProblems. See the note at the top of internal/scenario/bodyassert.go for why
// that placement is the whole point. This package keeps only what is argus-specific.

type BodyAssert = mcp.BodyAssert

const (
	BodyContains = mcp.BodyContainsOp
	BodyMatches  = mcp.BodyMatchesOp
	BodyExists   = mcp.BodyExistsOp
)

// ParseBodyAsserts delegates to the one implementation (VR12-E8 R5: ONE parser, every path).
func ParseBodyAsserts(bullets []string) ([]BodyAssert, []error) {
	return scenario.ParseBodyAsserts(bullets)
}

// bodyAssertObserved is the REALITY-ONLY observed message for a status-layer body-assertion
// failure. VR-C8: observed is shown to BOTH hats, so it must NEVER echo the test's expected
// body value (the expected lives only in failure.expected, test-hat). It states what happened
// (status matched, body didn't satisfy the assertion) without reproducing the asserted value.
const bodyAssertObserved = "status matched but the response body did not satisfy the scenario's body assertion (the asserted value is held out; see failure.expected with the test hat)"

// NoDeclaredStatusObserved is what a response-code-judged scenario gets when it declares no
// `status=` bullet (V29-016). It names the ABSENCE of a declaration, never a value, so it leaks
// no holdout: there is nothing to leak — the author wrote no expectation.
//
// ⚠ IT IS NOT A SUT VERDICT. The scenario is reported as `error`, not `failed`, for the same
// reason RO-04 classifies a dead rig as `error`: nothing about the SUT was measured, and calling
// it a failure sends a triager into the SUT for a defect that lives in the scenario file.
const NoDeclaredStatusObserved = "the scenario declares no expected status, so no response-code verdict can be made — add a `status=<code>` bullet under `### Runnable` (V29-016)"

// AssertsNothingObserved is the twin of the rule above, for every engine (V31-002 R10). A scenario
// with no `### Runnable` bullet declares nothing to compare, so there is nothing to measure — and
// until 0.3.32 an mcp or chain scenario in that state RAN and PASSED (measured 2026-09-12: `passed`,
// one request, no failure). Only the JMeter path caught it, through NoDeclaredStatusObserved.
//
// ⛔ IT IS NOT A FORMAT RULE, and the distinction is the owner's (DEC-1: "Refuse is a job of
// Validation and Writing, not running"). It states what was MEASURED — nothing — from one condition
// on the parsed file, and holds no copy of the validator's rules. A rule change can therefore never
// re-judge a catalogue that already runs.
const AssertsNothingObserved = "the scenario declares no runnable check, so nothing could be compared — add at least one bullet under `### Runnable` (V31-002)"

// hasBodyAssert reports whether any bullet declares a body assertion. It gates the hard verdict
// overwrite driven by the BODY-ASSERT-FAIL marker (argus.go), so it must agree with the parser
// exactly — a disagreement would overwrite a verdict for an assertion nobody enforced.
func hasBodyAssert(bullets []string) bool {
	asserts, _ := ParseBodyAsserts(bullets)
	return len(asserts) > 0
}

// bodyCheckTokenRe is the ONE machine token templates/http-ingestion.jmx ends a numbered body-check
// failure with: " [argus-body-check=N]", N being the i of the
// failing `expect.body.i.*` entry (1-based, the same N DeriveProps writes). Anchored at the END of the
// message: anything else in the line (a label, an op name) is never read.
var bodyCheckTokenRe = regexp.MustCompile(`\[argus-body-check=([0-9]{1,4})\]$`)

// failedBodyCheckFrom names the body check that failed, for the TEST hat (report.Failure.FailedBodyCheck).
//
// ⛔ VR-C8: it reads ONLY the structured index from the template's message and maps it back to the
// bullet from the PARSED scenario. It never parses the message for a field name or a value: a name
// parsed out of a value string carries the value. No token (an older template, the legacy scalar
// fallback), a token that is not at the end, or an index outside the scenario's body checks names
// nothing — the failure is reported exactly as before.
func failedBodyCheckFrom(failMsg string, runnable []string) *report.FailedBodyCheck {
	m := bodyCheckTokenRe.FindStringSubmatch(strings.TrimSpace(failMsg))
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return nil
	}
	bullets := bodyCheckBullets(runnable)
	if n > len(bullets) {
		return nil
	}
	return &report.FailedBodyCheck{Index: n, Bullet: bullets[n-1]}
}

// bodyCheckBullets lists, in order, the runnable bullets that become a numbered body check — exactly
// the ones ParseBodyAsserts turns into an assertion (one bullet yields at most one; a bullet it
// refuses yields none and takes no number), so position i here is DeriveProps' `expect.body.i`.
// Each is returned as written, without its list dash.
func bodyCheckBullets(runnable []string) []string {
	var out []string
	for _, b := range runnable {
		if asserts, _ := ParseBodyAsserts([]string{b}); len(asserts) == 1 {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(b), "-")))
		}
	}
	return out
}

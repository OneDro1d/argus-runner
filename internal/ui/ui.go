// Package ui is the M2.5 D4 glue for the web-UI testing path: the runner spawns a
// VENDORED Playwright spec (testkit/ui — no private dev-kit dependency) as an OS
// process (AC-13: no JMeter/.jmx template is involved — see internal/argus.UITag)
// and this package maps the process outcome to a verdict. UI scenarios are judged
// on rendered-DOM assertions + the "no backend 4xx/5xx during the flow" assertion
// (in the spec); a harness failure is reported DISTINCTLY as an execution failure,
// never a silent green (VR-L3).
package ui

import (
	"regexp"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// VendorDir is the in-repo vendored Playwright/Clerk UI kit (relative to repo root).
const VendorDir = "testkit/ui"

// Command is what the OS-Process-Sampler runs for a UI scenario: a vendored Playwright
// spec against the SUT's SPA (APP_URL passed via env). The sampler captures exit code +
// stdout/stderr; results/trace artifacts land under testkit/ui/test-results.
func Command(specPaths ...string) []string {
	// V29-021: variadic, because a `ui` scenario may name more than one spec — and because the
	// declared-assertions spec runs BESIDE the author's own, never instead of it.
	return append([]string{"npm", "run", "test:live", "--"}, specPaths...)
}

// Classify maps the OS-Process-Sampler outcome to a verdict (VR-L3 / UC-62). VR-L3 is
// explicit: a process that "exits non-zero OR exits 0 but produces no trace/results
// artifact" is a DISTINCT EXECUTION failure — "absent results != pass". So the artifact
// check comes FIRST, regardless of the exit code:
//   - NO results artifact (any exit code) -> execution/harness failure (browser launch,
//     OOM, missing dep, OR exit 0 with no test run / all skipped) — report.StatusError,
//     never a silent green and never collapsed into a SUT failure
//   - exit 0 WITH a results artifact       -> passed (a test actually ran + left a trace)
//   - exit != 0 WITH a results artifact    -> SUT failure (a DOM assertion or backend 4xx/5xx fired)
func Classify(exitCode int, hasResults bool, stderr string) (status, observed string) {
	switch {
	case !hasResults:
		return report.StatusError, "UI EXECUTION failure (no Playwright results artifact — the SUT was not exercised: harness/browser-launch, or exited 0 with no test run / all skipped): " + firstLine(stderr)
	case exitCode == 0:
		return "passed", "UI spec passed (rendered-DOM asserts + no backend 4xx/5xx during the flow)"
	default:
		return "failed", "UI spec failed (a DOM assertion or a backend 4xx/5xx during the flow) — see diagnostics.log / trace.zip"
	}
}

// firstLine is stderr's first line, with colour codes stripped BEFORE the line is cut: Playwright
// opens stderr with codes on a line of their own, and cutting first returned an empty "first line"
// and dropped the real error (#140 follow-up).
func firstLine(s string) string {
	s = strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

package argus

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ── the reactive pause (VR10-R1 / V28-009, owner D8) ──────────────────────────
//
// MEASURED (Memstore, 2026-08-26): a 120/min limiter turned four scenarios red with the SAME observed
// string a genuine bug produces — and everything the run fired after the first throttle was fired
// into a limiter that was still refusing, so the rest of the run was poisoned too.
//
// The fix the owner chose over proactive pacing: when the SUT says "slow down", STOP. Wait for the
// time the SUT itself asked for, re-fire that one scenario once, and carry on.
//
//	⛔ THE WAIT IS AT RUN LEVEL, BETWEEN SCENARIOS — never inside a scenario's own timeout clock.
//	   Waiting 48s inside a 30s scenario budget does not measure anything; it manufactures a
//	   timeout red, which is the same defect wearing a different hat.
//
// Caps, because waiting is not free (owner D9): three pauses cover Memstore's measured 48s
// retry_after, and past that the pack is simply too large for the limit — SPLITTING it is the right
// answer, not waiting longer. At a cap the run stops pausing, classifies the rest as they come, and
// says so in the summary.
//
// PER RUN = PER EXECUTOR PROCESS (SA §0.14 R1-c). Replicas exist for the control plane, not for a
// run: each process paces itself and nothing coordinates across them.

// rateLimitSleep is the run-level wall-clock pause. A var so a test can prove the run waited 48
// seconds without spending 48 seconds proving it.
var rateLimitSleep = time.Sleep

// rateLimitSpec translates the SUT's declaration into what the judge needs. nil in, nil out — with
// no `rate_limit:` block every verdict is exactly what it was before this build (VR10-R1-3).
func rateLimitSpec(c *config.Config) *mcp.RateLimitSpec {
	if c == nil || c.RateLimit == nil {
		return nil
	}
	return &mcp.RateLimitSpec{
		BodyContains:    c.RateLimit.Signature.BodyContains,
		RetryAfterField: c.RateLimit.Signature.RetryAfterField,
		DefaultPause:    c.RateLimit.Window(),
	}
}

// pacer holds ONE run's pause budget.
type pacer struct {
	rl        *config.RateLimit
	pauses    int
	totalWait time.Duration
	capHit    bool
}

func newPacer(c *config.Config) *pacer {
	if c == nil || c.RateLimit == nil {
		return nil
	}
	return &pacer{rl: c.RateLimit}
}

// handle is called with a scenario result the SUT refused for rate-limiting. It pauses (within the
// caps), re-fires the scenario ONCE via `rerun`, and returns the row the report will carry.
//
// SA §0.14 R1-a: the RETRY's outcome is the row's status — a retry that passes is a PASS, with the
// retry and the wait recorded beside it. Throttled again → `errored` ("rate-limited twice") and no
// further retry: a second refusal is the SUT telling us the pack is too big for its limit.
func (p *pacer) handle(res report.ScenarioResult, rerun func() report.ScenarioResult) report.ScenarioResult {
	if p == nil || !res.RateLimited {
		return res
	}
	wait, ok := p.budget(time.Duration(res.RetryAfterMs) * time.Millisecond)
	if !ok {
		// A cap is reached. Owner D3: when the wait does not fit the budget, do not wait — classify
		// and move on. The row keeps the not-measured status it already has.
		p.capHit = true
		return res
	}
	rateLimitSleep(wait)
	p.pauses++
	p.totalWait += wait
	pausedMs := int(wait.Milliseconds()) + res.PausedMs
	if !p.rl.RetryOnce() {
		res.PausedMs = pausedMs
		return res
	}
	out := rerun()
	out.PausedMs = pausedMs
	out.RateLimitedRetries = res.RateLimitedRetries + 1
	if out.RateLimited {
		out.Failure = &report.Failure{Observed: mcp.RateLimitedTwiceObserved(time.Duration(out.RetryAfterMs) * time.Millisecond)}
	}
	return out
}

// budget returns how long this pause may actually last, and whether it may happen at all. The
// requested wait is CLAMPED to what is left of max_total_wait — the run's budget is the run's, not
// the SUT's to spend.
func (p *pacer) budget(want time.Duration) (time.Duration, bool) {
	if p.pauses >= p.rl.MaxPauses() {
		return 0, false
	}
	remaining := p.rl.MaxTotalWait() - p.totalWait
	if remaining <= 0 {
		return 0, false
	}
	if want <= 0 {
		want = p.rl.Window()
	}
	if want > remaining {
		want = remaining
	}
	return want, true
}

// stamp writes what the pause mechanism did onto the summary an operator reads. Everything is
// omitempty, so a run that never met a rate limit produces a byte-identical report (VR10-R1-9).
func (p *pacer) stamp(s *report.Summary) {
	if p == nil {
		return
	}
	s.RateLimitPauses = p.pauses
	if p.capHit {
		s.RateLimitCapHit = true
		s.Note = fmt.Sprintf("rate-limit pause cap reached (%d/%s): split the pack", p.rl.MaxPauses(), config.Seconds(p.rl.MaxTotalWait()))
	}
}

// ── the HTTP path (VR10-R1-12, owner D12) ────────────────────────────────────
//
// On HTTP the signature is the STATUS CODE — 429 plus Retry-After — and `body_contains` is not
// consulted (SA §0.14 R1-e: 429 is unambiguous). The declaration still gates everything: without a
// `rate_limit:` block a 429 stays exactly what it is today (VR10-R1-3).
//
// ⛔ ONLY a scenario that did NOT get what it asked for. A scenario that EXPECTS 429 is TESTING the
// limiter and passed — reclassifying it would delete the whole Rate Limiting test layer.

// rateLimitedMarker is what templates/http-ingestion.jmx writes into the JMeter failureMessage when
// the SUT answers 429, carrying the response's Retry-After header verbatim. The .jtl has no header
// column, so the assertion is the only place the header can be read.
const rateLimitedMarker = "RATE-LIMITED: retry_after="

// http1123 is the HTTP-date form of Retry-After (the other legal form is a count of seconds).
const http1123 = http.TimeFormat

var retryAfterMarkerRe = regexp.MustCompile(regexp.QuoteMeta(rateLimitedMarker) + `([^,\n]*)`)

// httpRateLimited reports whether this .jtl is the SUT refusing us for going too fast, and for how
// long it asked us to wait.
func httpRateLimited(rl *config.RateLimit, codes []int, jtlPath string) (bool, time.Duration) {
	if rl == nil {
		return false, 0
	}
	found := false
	for _, c := range codes {
		if c == http.StatusTooManyRequests {
			found = true
			break
		}
	}
	if !found {
		return false, 0
	}
	_, failMsg := readJTLSuccess(jtlPath)
	raw := ""
	if m := retryAfterMarkerRe.FindStringSubmatch(failMsg); m != nil {
		raw = strings.TrimSpace(m[1])
	}
	return true, parseRetryAfter(raw, rl.Window())
}

// parseRetryAfter reads a Retry-After value — a count of SECONDS or an HTTP-date, both legal
// (SA §0.14 R1-f). Anything unreadable falls back to the declared window: the SUT is throttling
// either way, so "we could not parse the header" must not become "do not pause".
func parseRetryAfter(raw string, fallback time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return fallback
}

// ── the pre-run estimate (VR10-R1-10, owner D11) ─────────────────────────────

// RequestsEstimated is how many requests a scenario pack will send at the SUT — SCENARIOS PLUS
// CHAIN STEPS, because a chain is several calls, not one, and the whole point of the number is to
// be comparable with a declared `rate_limit.requests`.
//
// It is an ESTIMATE and says so: a retried scenario fires one more, and the caps mean the run can
// stop pausing. It exists to answer "might this pack hit the limit?", not to be exact.
func RequestsEstimated(scenariosDir string) int {
	total := 0
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		total += requestsFor(d.Scenario)
	}
	return total
}

// requestsFor counts the requests ONE scenario fires: every chain step, the two samplers of an
// HTTP idempotency scenario (two expected codes on HTTP Ingestion — the dedicated template fires
// the same request twice), and otherwise one.
func requestsFor(s *scenario.Scenario) int {
	if contains(s.Tags, ChainTag) {
		if steps, err := parseChainSpec(s.Trigger.Payload, "cid"); err == nil && len(steps) > 0 {
			return len(steps)
		}
		return 1
	}
	// VR12-E7: two requests are expected ONLY when `status2=` is DECLARED in the runnable bullets.
	// This used to count however many 3-digit numbers turned up in EXPECT, so PERM-001's three
	// prose variants of ONE request made the expected count 3.
	if _, second := extractDeclaredStatuses(s.RunnableExpect()); second > 0 && PrimaryLayer(s) == "HTTP Ingestion" {
		return 2
	}
	return 1
}

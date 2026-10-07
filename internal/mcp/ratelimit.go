package mcp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ── the declared rate limit, on the tool plane (VR10-R1 / V28-009) ────────────
//
// MEASURED (Memstore, 2026-08-26, run 20260826T004001136): the limiter answers HTTP 200 with a
// perfectly normal MCP result whose isError is true and whose text is `{"error":"rate_limited",
// "retry_after":48}`. The judge could only say "responder returned result.isError:true" — the same
// sentence as a genuine "document not found" — so a rate limit was reported as a product defect.
//
// ⛔ NO HEURISTIC (owner D1). Nothing here fires unless the SUT DECLARED the signature in its
// argus-config: a guess that matched a genuine bug mentioning "rate limit" would HIDE a real
// defect, which is strictly worse than the problem being fixed.

// RateLimitSpec is the SUT's declaration, as the judge needs it. The argus run loop builds it from
// config.RateLimit; the mcp package deliberately does not import config (the judge is a pure
// function of a response plus a declaration).
type RateLimitSpec struct {
	// BodyContains is the stable text in this SUT's refusal. EMPTY = never matches: an empty
	// substring is contained in everything, and "every tool error is a throttle" is the exact
	// false-negative this row exists to prevent.
	BodyContains string
	// RetryAfterField is the JSON field carrying the retry-after SECONDS; optional.
	RetryAfterField string
	// DefaultPause is used when the SUT gave no retry_after at all — the declared window
	// (SA §0.14 R1-h: per: minute → 60s, per: second → 1s).
	DefaultPause time.Duration
}

// RateLimitedObserved is the reality-only observed line of a throttled scenario (VR10-R1-6): it
// names the rate limit, the retry-after value, and the fact that NOTHING WAS MEASURED. That last
// clause is the point — `errored` means not measured, and the operator must read it as such.
func RateLimitedObserved(retryAfter time.Duration) string {
	return fmt.Sprintf("SUT rate-limited this request (retry_after %ds) — the scenario was not measured", int(retryAfter.Seconds()))
}

// RateLimitedTwiceObserved is the same statement for a scenario that was throttled AGAIN on its
// one retry (SA §0.14 R1-a): it stays `errored` and is not retried further.
func RateLimitedTwiceObserved(retryAfter time.Duration) string {
	return fmt.Sprintf("SUT rate-limited this request twice (retry_after %ds) — the scenario was not measured", int(retryAfter.Seconds()))
}

// rateLimited reports whether this response IS the SUT's declared throttle, and for how long it
// asked us to wait. The rule (SA §1.4.R1): JSONRPCError == nil && IsError && the concatenated
// result.content text contains the declared BodyContains. A protocol-plane error is never a
// throttle (it is a different plane), and a SUCCESSFUL result is never one either.
func rateLimited(r CallResult, spec *RateLimitSpec) (bool, time.Duration) {
	if spec == nil || spec.BodyContains == "" {
		return false, 0
	}
	if r.JSONRPCError != nil || !r.IsError {
		return false, 0
	}
	text := ContentText(r)
	if !strings.Contains(text, spec.BodyContains) {
		return false, 0
	}
	return true, retryAfterFrom(text, spec)
}

// retryAfterFrom reads the declared retry-after field out of the refusal text. The SUT writes it
// as a JSON number or a quoted string; both are read. Absent (or undeclared) → the declared
// window, so the run still paces itself rather than hammering the limiter.
func retryAfterFrom(text string, spec *RateLimitSpec) time.Duration {
	if spec.RetryAfterField != "" {
		re := regexp.MustCompile(`"` + regexp.QuoteMeta(spec.RetryAfterField) + `"\s*:\s*"?([0-9]+(?:\.[0-9]+)?)"?`)
		if m := re.FindStringSubmatch(text); m != nil {
			if secs, err := strconv.ParseFloat(m[1], 64); err == nil && secs > 0 {
				return time.Duration(secs * float64(time.Second))
			}
		}
	}
	return spec.DefaultPause
}

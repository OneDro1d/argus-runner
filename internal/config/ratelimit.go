package config

import (
	"fmt"
	"strings"
	"time"
)

// ── rate_limit (VR10-R1 / V28-009) ───────────────────────────────────────────
//
// The SUT DECLARES its limit and how it says "you are going too fast"; Argus never guesses
// (owner D1). Measured 2026-08-26: a throttled Memstore scenario was scored `failed`, byte-for-byte
// the same as a real "document not found" — a rate limit was being reported as a product defect,
// and every scenario after the first throttle was poisoned.
//
// ⛔ NOT `targets.rate_limiting` (removed in this build, V28-019 owns that concept). That key was
// about TESTING a limiter; this block is about RESPECTING one. They must not share a namespace.
//
// Unknown keys anywhere under `rate_limit` — including under `signature:` and `pause:` — are
// refused BY NAME at load, the rule V28-012 gave `targets`: a key that parses and reaches nothing
// looks supported, which is the worst way to be wrong.

// RateLimitSignature is HOW this SUT says "too fast": the stable text in its refusal, and
// (optionally) the field carrying the retry-after seconds.
type RateLimitSignature struct {
	BodyContains    string `yaml:"body_contains"`
	RetryAfterField string `yaml:"retry_after_field"` // optional; seconds
}

// RateLimitPause are the per-instance overrides for the run-level pause. All three are optional;
// an omitted (or zero) value takes the global default.
//
// ⛔ PER RUN = PER EXECUTOR PROCESS (SA §0.14 R1-c). A run is executed by ONE executor process —
// replicas exist for the control plane, not for a run — so each process paces itself and there is
// no cross-process coordination to expect.
type RateLimitPause struct {
	MaxPauses    int           `yaml:"max_pauses"`     // default 3 per run
	MaxTotalWait time.Duration `yaml:"max_total_wait"` // default 180s per run
	RetryOnce    *bool         `yaml:"retry_once"`     // default true
}

// RateLimit is the top-level `rate_limit:` block of an argus-config.yaml.
type RateLimit struct {
	Requests  int                `yaml:"requests"`
	Per       string             `yaml:"per"` // "minute" | "second" — anything else refused at load
	Signature RateLimitSignature `yaml:"signature"`
	Pause     *RateLimitPause    `yaml:"pause"`
}

// The global defaults (owner D9): three pauses cover Memstore's measured 48s retry_after; beyond
// that the pack is simply too large for the limit and SPLITTING is the right answer, not waiting.
const (
	defaultMaxPauses    = 3
	defaultMaxTotalWait = 180 * time.Second
)

// PerMinute / PerSecond are the only two windows a SUT may declare.
const (
	PerMinute = "minute"
	PerSecond = "second"
)

// MaxPauses is how many times ONE run may pause (default 3).
func (r *RateLimit) MaxPauses() int {
	if r != nil && r.Pause != nil && r.Pause.MaxPauses > 0 {
		return r.Pause.MaxPauses
	}
	return defaultMaxPauses
}

// MaxTotalWait is the total wall-clock ONE run may spend paused (default 180s).
func (r *RateLimit) MaxTotalWait() time.Duration {
	if r != nil && r.Pause != nil && r.Pause.MaxTotalWait > 0 {
		return r.Pause.MaxTotalWait
	}
	return defaultMaxTotalWait
}

// RetryOnce reports whether a throttled scenario is re-fired once after the pause (default true).
func (r *RateLimit) RetryOnce() bool {
	if r != nil && r.Pause != nil && r.Pause.RetryOnce != nil {
		return *r.Pause.RetryOnce
	}
	return true
}

// Window is the declared limit's window — and the fallback pause when the SUT's refusal carries no
// retry_after at all (SA §0.14 R1-h): `per: minute` → 60s, `per: second` → 1s.
func (r *RateLimit) Window() time.Duration {
	if r != nil && r.Per == PerSecond {
		return time.Second
	}
	return time.Minute
}

// validate refuses a malformed declaration LOUDLY at load, which is validate time
// (`validate-config` loads the file) — VR10-R1-1.
func (r *RateLimit) validate() error {
	if r == nil {
		return nil
	}
	var errs []string
	if r.Requests <= 0 {
		errs = append(errs, fmt.Sprintf("rate_limit.requests is %d — declare how many requests this SUT accepts per window (a positive number)", r.Requests))
	}
	if r.Per != PerMinute && r.Per != PerSecond {
		errs = append(errs, fmt.Sprintf("rate_limit.per is %q — accepted: %s, %s", r.Per, PerMinute, PerSecond))
	}
	// Gate-2 F5: without the refusal's text the detector can never fire, and a throttled answer would be
	// judged as a product failure — the exact false red this block exists to remove. Declared = complete.
	if strings.TrimSpace(r.Signature.BodyContains) == "" {
		errs = append(errs, "rate_limit.signature.body_contains is required — the stable text of this SUT's refusal (an MCP tool-error text or the HTTP 429 body); without it a throttled answer would be judged as a product failure")
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// Exceeds reports whether a pack of `estimated` requests would break this limit in ONE window.
//
// It compares against the STRICTER (sliding-window) reading of `per`, because the row's part F is
// explicit — pace to the stricter reading unless the SUT declares which it means — and the cost of
// being wrong is asymmetric: an unnecessary warning costs a line of text, a missing one costs a
// poisoned run.
func (r *RateLimit) Exceeds(estimated int) bool {
	return r != nil && r.Requests > 0 && estimated > r.Requests
}

// Warning is the D11 pre-run line, shown ONLY when a limit is declared AND the pack would exceed
// it. INFORMATION ONLY (owner, corrected 2026-09-05): it is never a question, it never blocks, and
// the run starts immediately — the operator is told what MAY happen and the pause mechanism handles
// it if it does.
func (r *RateLimit) Warning(estimated int) string {
	if !r.Exceeds(estimated) {
		return ""
	}
	return fmt.Sprintf(
		"this pack sends ~%d requests; the SUT declares %d/%s. If the limit is hit, the run will pause for the SUT's retry-after (up to %d times, %s total) and retry once. Information only — the run is not blocked.",
		estimated, r.Requests, r.Per, r.MaxPauses(), Seconds(r.MaxTotalWait()))
}

// Seconds renders a cap the way the operator declared it — `180s`, not Go's `3m0s`. The number in
// the message must be the number in the YAML, or the reader has to convert to check us.
func Seconds(d time.Duration) string { return fmt.Sprintf("%ds", int(d.Seconds())) }

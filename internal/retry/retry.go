// Package retry is VR-F2b: ONE retry policy for every control-plane call, in one implementation.
//
// ── WHY ONE ─────────────────────────────────────────────────────────────────────────────────────
//
// The rule says "ONE retry policy for EVERY control-plane call in onboarding (~10 of them)", and
// VR-F6 then says a direct run may only offer the local scenario set "when the CP is unavailable
// after VR-F2b's retries". Two callers in two packages — internal/onboard (the onboarding CP calls)
// and internal/runner (the executor's catalog sync) — so the policy lives in neither of them.
//
// Written separately they would drift, and the drift would be invisible: nothing fails when one
// caller waits 8 seconds and the other waits 30. What breaks is the PROMISE — "we tried properly
// before telling you it is down" — and that promise is what VR-F6's refusal rests on. If the retries
// are weaker than advertised, the refusal is a lie about how hard we tried.
//
// ── THE POLICY, verbatim from VR-F2b ────────────────────────────────────────────────────────────
//
//	4 attempts · waits 1 s / 3 s / 8 s · 10 s per-attempt timeout · 45 s overall budget
//	retry transport errors, timeouts and 5xx; NEVER 401/403 or any other 4xx
//
// The 4xx exclusion is not an optimisation. A 401 means the token was rejected and a 403 means it
// was not allowed — repeating the request cannot change either, and repeating it four times turns
// one clear "your credential is wrong" into 45 seconds of silence followed by the same answer.
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The policy. Exported so a test can assert against the numbers rather than restate them, and so a
// caller can quote the budget in a message it shows a human ("tried for 45s").
const (
	Attempts   = 4
	PerAttempt = 10 * time.Second
	Budget     = 45 * time.Second
)

// Waits are the pauses BETWEEN attempts — one shorter than Attempts, because nothing waits after the
// last one.
var Waits = []time.Duration{1 * time.Second, 3 * time.Second, 8 * time.Second}

// fatal marks an error as NOT worth retrying. Wrapped rather than sentinel-compared so the original
// error survives for the caller's message.
type fatal struct{ err error }

func (f fatal) Error() string { return f.err.Error() }
func (f fatal) Unwrap() error { return f.err }

// Fatal marks err as non-retryable: Do returns it immediately, without waiting and without further
// attempts. Use it for anything a repeat cannot fix — a rejected credential, a malformed request, a
// response that parsed but said no.
func Fatal(err error) error {
	if err == nil {
		return nil
	}
	return fatal{err}
}

// IsFatal reports whether err was marked non-retryable.
func IsFatal(err error) bool {
	var f fatal
	return errors.As(err, &f)
}

// HTTPRetryable classifies an HTTP status for this policy: 5xx and 0 (no response reached us at all)
// are worth another go; every 4xx is not; 2xx/3xx are not failures.
//
// A caller that has a status code should use THIS rather than writing `>= 500`, so the one place the
// rule lives is the one place it can be changed.
func HTTPRetryable(status int) bool {
	return status == 0 || status >= 500
}

// Do runs fn until it succeeds, fn returns a Fatal error, the attempts run out, or the overall
// budget expires — whichever comes first.
//
// Each attempt gets its own context with a PerAttempt deadline, so one wedged request cannot consume
// the whole budget while looking like it is still working. That is the difference between "the
// control plane is slow" and "the control plane is gone", and only the second should end in a
// refusal.
//
// The error returned is the LAST attempt's error, wrapped with how many attempts were made, so a
// message shown to a human can say what was tried without the caller counting.
func Do(ctx context.Context, fn func(ctx context.Context) error) error {
	return doWith(ctx, time.Now, time.Sleep, fn)
}

// DoNoWait is Do with the pauses removed: the same attempt count, the same per-attempt deadline, the
// same Fatal handling and the same classification — everything except the sleeping.
//
// It exists as a TEST SEAM. Asserting that a control-plane call actually retries a 503 is worth
// having, and with the real policy each such assertion costs 12 seconds of doing nothing. A test that
// slow gets deleted or skipped eventually, and then the retry is unverified — which is how VR-F2b's
// last enforcement ended up in a package nothing imported.
//
// Not for production use: without the waits a retry loop is a tight hammer on a struggling server.
func DoNoWait(ctx context.Context, fn func(ctx context.Context) error) error {
	return doWith(ctx, time.Now, func(time.Duration) {}, fn)
}

// doWith is Do with an injectable clock and sleep, so tests exercise the real attempt/budget logic
// without spending 45 seconds proving it. The waits are part of the policy; making a test wait them
// out would only prove that time passes.
//
// The CLOCK is injectable for one reason: without it the budget guard below cannot be tested at all.
// 1+3+8 = 12 s never approaches a 45 s budget, so a test with a real clock passes whether or not the
// guard exists — which is a test that proves nothing while looking like coverage.
func doWith(ctx context.Context, now func() time.Time, sleep func(time.Duration), fn func(ctx context.Context) error) error {
	deadline := now().Add(Budget)
	var last error
	for attempt := 1; attempt <= Attempts; attempt++ {
		// The budget bounds when new work may START. Without this the guard below only stopped us
		// SLEEPING past the deadline, and a slow control plane still bought attempts with time it did
		// not have: 4 attempts x 10 s + 12 s of waits is 52 s, comfortably past 45. Found by this
		// package's own budget test, which is the reason the clock is injectable.
		//
		// Worst case is therefore the budget plus ONE attempt — an attempt that starts inside the
		// budget is allowed to finish, bounded by PerAttempt. Cutting a request off mid-flight to
		// honour the ceiling exactly would throw away work that was about to succeed.
		if attempt > 1 && !now().Before(deadline) {
			return fmt.Errorf("%w (gave up after %d attempt(s): the %s budget was spent)", last, attempt-1, Budget)
		}
		if err := ctx.Err(); err != nil {
			if last != nil {
				return fmt.Errorf("%w (gave up after %d attempt(s): %v)", err, attempt-1, last)
			}
			return err
		}
		actx, cancel := context.WithTimeout(ctx, PerAttempt)
		err := fn(actx)
		cancel()
		if err == nil {
			return nil
		}
		if IsFatal(err) {
			return errors.Unwrap(err)
		}
		last = err
		if attempt == Attempts {
			break
		}
		wait := Waits[attempt-1]
		// Do not start a wait we cannot finish inside the budget: sleeping 8 s to leave 200 ms for
		// the last attempt spends the time and buys nothing. Stop and report honestly instead.
		if now().Add(wait).After(deadline) {
			return fmt.Errorf("%w (gave up after %d attempt(s); the %s budget would expire during the next wait)",
				last, attempt, Budget)
		}
		sleep(wait)
	}
	return fmt.Errorf("%w (gave up after %d attempts over %s)", last, Attempts, Budget)
}

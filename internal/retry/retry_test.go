package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// VR-F2b — the policy, asserted against the NUMBERS the rule states rather than against whatever the
// implementation happens to do.
//
// This matters more than a retry helper usually would, because VR-F6's refusal rests on it: when a
// direct run tells someone "the control plane is unavailable, choose between waiting and running
// your local set", the honesty of that sentence is exactly how hard we actually tried. Retries
// weaker than advertised make the refusal a lie about the effort.

func TestPolicy_MatchesTheRule(t *testing.T) {
	if Attempts != 4 {
		t.Errorf("Attempts = %d, want 4 (VR-F2b)", Attempts)
	}
	if PerAttempt != 10*time.Second {
		t.Errorf("PerAttempt = %v, want 10s (VR-F2b)", PerAttempt)
	}
	if Budget != 45*time.Second {
		t.Errorf("Budget = %v, want 45s (VR-F2b)", Budget)
	}
	want := []time.Duration{time.Second, 3 * time.Second, 8 * time.Second}
	if len(Waits) != len(want) {
		t.Fatalf("Waits = %v, want %v", Waits, want)
	}
	for i := range want {
		if Waits[i] != want[i] {
			t.Errorf("Waits[%d] = %v, want %v", i, Waits[i], want[i])
		}
	}
	// One fewer wait than attempts: nothing waits after the last try.
	if len(Waits) != Attempts-1 {
		t.Errorf("%d waits for %d attempts — the last attempt must not be followed by a sleep", len(Waits), Attempts)
	}
}

func TestDo_SucceedsWithoutRetryingWhenItWorks(t *testing.T) {
	calls := 0
	var slept []time.Duration
	err := doWith(context.Background(), time.Now, func(d time.Duration) { slept = append(slept, d) },
		func(context.Context) error { calls++; return nil })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("called %d times, want 1 — a success must not be retried", calls)
	}
	if len(slept) != 0 {
		t.Errorf("slept %v on a first-attempt success", slept)
	}
}

func TestDo_RetriesToTheLimitAndWaitsTheStatedPauses(t *testing.T) {
	calls := 0
	var slept []time.Duration
	err := doWith(context.Background(), time.Now, func(d time.Duration) { slept = append(slept, d) },
		func(context.Context) error { calls++; return errors.New("connection refused") })
	if err == nil {
		t.Fatal("a persistently failing call returned success")
	}
	if calls != Attempts {
		t.Errorf("called %d times, want %d", calls, Attempts)
	}
	if len(slept) != len(Waits) {
		t.Fatalf("slept %v, want %v", slept, Waits)
	}
	for i := range Waits {
		if slept[i] != Waits[i] {
			t.Errorf("wait %d = %v, want %v", i, slept[i], Waits[i])
		}
	}
	// The message must carry the cause AND the effort, because it becomes what a human reads.
	if !contains(err.Error(), "connection refused") {
		t.Errorf("the final error lost the underlying cause: %v", err)
	}
	if !contains(err.Error(), "4 attempts") {
		t.Errorf("the final error does not say how many attempts were made: %v", err)
	}
}

func TestDo_RecoversOnALaterAttempt(t *testing.T) {
	calls := 0
	err := doWith(context.Background(), time.Now, func(time.Duration) {}, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("503")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("a call that recovered on attempt 3 returned %v", err)
	}
	if calls != 3 {
		t.Errorf("called %d times, want 3 — it must stop as soon as it works", calls)
	}
}

// THE ONE THAT MATTERS FOR CREDENTIALS. A rejected token cannot be fixed by asking again, and four
// attempts turn one clear answer into 45 seconds of silence followed by the same answer.
func TestDo_FatalStopsImmediately(t *testing.T) {
	calls := 0
	var slept []time.Duration
	sentinel := errors.New("401 the token was rejected")
	err := doWith(context.Background(), time.Now, func(d time.Duration) { slept = append(slept, d) },
		func(context.Context) error { calls++; return Fatal(sentinel) })
	if calls != 1 {
		t.Errorf("a Fatal error was retried %d times — repeating cannot change a rejected credential", calls)
	}
	if len(slept) != 0 {
		t.Errorf("slept %v before giving up on a Fatal error", slept)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the original error unwrapped so the caller can report it", err)
	}
	if IsFatal(err) {
		t.Error("the fatal wrapper leaked to the caller — it is an internal marker, not part of the message")
	}
}

func TestDo_EachAttemptGetsItsOwnDeadline(t *testing.T) {
	var deadlines []time.Duration
	_ = doWith(context.Background(), time.Now, func(time.Duration) {}, func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("an attempt ran with NO deadline — one wedged request would eat the whole budget " +
				"while looking like it is still working")
			return errors.New("x")
		}
		deadlines = append(deadlines, time.Until(d).Round(time.Second))
		return errors.New("x")
	})
	for i, d := range deadlines {
		if d != PerAttempt {
			t.Errorf("attempt %d deadline = %v, want %v", i+1, d, PerAttempt)
		}
	}
}

// A cancelled parent stops the loop rather than working through the remaining attempts. The message
// still reports what was tried — "cancelled" alone would hide that the CP was also failing.
func TestDo_HonoursACancelledParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := doWith(ctx, time.Now, func(time.Duration) {}, func(context.Context) error {
		calls++
		cancel()
		return errors.New("503")
	})
	if calls != 1 {
		t.Errorf("called %d times after the parent was cancelled, want 1", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a context.Canceled", err)
	}
	if !contains(err.Error(), "503") {
		t.Errorf("the cancellation hid the underlying failure: %v", err)
	}
}

// THE BUDGET IS A REAL CEILING, and this test exists in its second form.
//
// The first version asserted that the total sleep did not exceed the budget — and 1+3+8 = 12 s never
// approaches 45 s, so it passed whether or not the guard existed. A test that cannot fail is not
// coverage; it is a comment that costs CPU. It needed a fake clock, which is why doWith takes one.
//
// It then FOUND SOMETHING: the guard only stopped us SLEEPING past the deadline, so a slow control
// plane still bought attempts with time it did not have. Both halves are pinned below.

// A wait that would run past the deadline is not started.
func TestDo_StopsRatherThanSleepingPastTheBudget(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	var slept []time.Duration
	calls := 0
	err := doWith(context.Background(),
		func() time.Time { return clock },
		func(d time.Duration) { slept = append(slept, d); clock = clock.Add(d) },
		func(context.Context) error {
			calls++
			clock = clock.Add(20 * time.Second) // a slow, failing attempt
			return errors.New("i/o timeout")
		})
	if err == nil {
		t.Fatal("expected failure")
	}
	// t0 -> a1 ends 20s, wait 1s; a2 ends 41s, wait 3s; a3 ends 64s and the 8s wait would not fit.
	if calls != 3 {
		t.Errorf("made %d attempts, want 3 — the fourth needed an 8s wait the budget could not cover", calls)
	}
	if len(slept) != 2 {
		t.Errorf("slept %v, want the 1s and 3s waits only", slept)
	}
	if !contains(err.Error(), "budget") {
		t.Errorf("the message does not say the budget ran out: %v", err)
	}
	if !contains(err.Error(), "i/o timeout") {
		t.Errorf("the message lost the underlying cause: %v", err)
	}
}

// AND an attempt is not STARTED once the budget is spent. This is the half the first version of the
// test missed: without it, 4 attempts at the 10s per-attempt ceiling plus 12s of waits is 52s.
func TestDo_DoesNotStartAnAttemptAfterTheBudgetIsSpent(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	calls := 0
	err := doWith(context.Background(),
		func() time.Time { return clock },
		func(d time.Duration) { clock = clock.Add(d) },
		func(context.Context) error {
			calls++
			clock = clock.Add(25 * time.Second)
			return errors.New("connection refused")
		})
	// t0 -> a1 ends 25s, wait 1s -> 26s; a2 ends 51s, which is past the 45s deadline, so a3 is never
	// started even though the 3s wait would have "fitted" by the old check alone.
	if calls != 2 {
		t.Errorf("made %d attempts, want 2 — the budget was already spent before the third", calls)
	}
	if err == nil || !contains(err.Error(), "budget") {
		t.Errorf("err = %v, want a message naming the budget", err)
	}
}

// The mirror case: attempts that are fast leave room for all four, so a cheap-but-failing control
// plane still gets the full policy rather than being cut short by an over-eager guard.
func TestDo_FastFailuresStillGetEveryAttempt(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	calls := 0
	_ = doWith(context.Background(),
		func() time.Time { return clock },
		func(d time.Duration) { clock = clock.Add(d) },
		func(context.Context) error {
			calls++
			clock = clock.Add(50 * time.Millisecond)
			return errors.New("connection refused")
		})
	if calls != Attempts {
		t.Errorf("made %d attempts, want %d — fast failures fit inside the budget easily", calls, Attempts)
	}
}

func TestHTTPRetryable_ClassifiesTheWayTheRuleSays(t *testing.T) {
	for _, s := range []int{0, 500, 502, 503, 504} {
		if !HTTPRetryable(s) {
			t.Errorf("status %d should be retryable (transport failure or 5xx)", s)
		}
	}
	for _, s := range []int{200, 201, 204, 400, 401, 403, 404, 409, 422} {
		if HTTPRetryable(s) {
			t.Errorf("status %d must NOT be retried.\n"+
				"  401/403 especially: the token was rejected or not allowed, and asking again four\n"+
				"  times turns one clear answer into 45 seconds of silence and the same answer.", s)
		}
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

package runner

import (
	"testing"
	"time"
)

// VR6-W1 / SA §0.4 — the SUT probe runs on ITS OWN clock, not the poll's.
//
// ── WHY THE PROBE CANNOT RIDE THE POLL'S CADENCE ──────────────────────────────────────────────────
//
// The PO asked for "at the poll interval, not tighter". THERE IS NO POLL INTERVAL. The executor
// LONG-polls: PollResponse is an assignment or a ~25 s timeout (federation/wire.go), so a poll returns
// when work arrives OR after ~25 s. On a busy instance whose polls return immediately, "once per poll"
// would dial the SUT continuously. HeartbeatEvery (30 s) is the mid-run heartbeat, a different mechanism
// on a different trigger.
//
// So: at most once per SUTProbeEvery, result attached to whichever poll goes out next.
//
// ── AND WHY IT MUST BE NON-FATAL ──────────────────────────────────────────────────────────────────
//
// A probe failure must NEVER affect the poll. The poll IS the heartbeat (D-FED.4) — if a flaky SUT could
// break it, a struggling SUT would make its own executor read as dead, and the page would show STALE
// beside UNREACHABLE when only one of the two is true. Worse, it would do so through the exact channel
// the operator needs to see the SUT problem on.

func ptrTo(b bool) *bool { return &b }

func TestSUTProbe_FirstObserveProbes(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	calls := 0
	p := &sutProbe{every: time.Minute, now: func() time.Time { return now },
		probe: func() *bool { calls++; return ptrTo(true) }}

	got, at := p.Observe()
	if calls != 1 {
		t.Fatalf("probe ran %d times on the first Observe, want 1", calls)
	}
	if got == nil || !*got {
		t.Error("the first Observe did not return the probe's answer")
	}
	if at == nil || !at.Equal(now) {
		t.Errorf("checked-at = %v, want %v", at, now)
	}
}

// ⚠ THE CADENCE. A second Observe inside the window must NOT dial.
func TestSUTProbe_ASecondObserveInsideTheWindowDoesNotProbe(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	calls := 0
	p := &sutProbe{every: time.Minute, now: func() time.Time { return now },
		probe: func() *bool { calls++; return ptrTo(true) }}

	p.Observe()
	now = now.Add(59 * time.Second)
	p.Observe()

	if calls != 1 {
		t.Fatalf("probe ran %d times across two Observes 59 s apart, want 1. A long poll that returns "+
			"immediately would otherwise dial the SUT on every return", calls)
	}
}

// 🚩 AND IT MUST RETURN THE ORIGINAL TIMESTAMP, not the current time. A cached answer restamped as fresh
// is a fabricated measurement, and the whole point of carrying SUTCheckedAt is that the page can tell a
// current reading from a stale one. Restamping would make every reading look current forever.
func TestSUTProbe_ACachedAnswerKeepsItsOriginalTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	first := now
	p := &sutProbe{every: time.Minute, now: func() time.Time { return now },
		probe: func() *bool { return ptrTo(true) }}

	p.Observe()
	now = now.Add(45 * time.Second)
	_, at := p.Observe()

	if at == nil || !at.Equal(first) {
		t.Fatalf("checked-at = %v, want the ORIGINAL %v. Restamping a cached answer as current makes "+
			"a 20-hour-old reading indistinguishable from a fresh one, which is precisely what the "+
			"page uses this field to tell apart", at, first)
	}
}

func TestSUTProbe_TheWindowElapsingProbesAgain(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	calls := 0
	p := &sutProbe{every: time.Minute, now: func() time.Time { return now },
		probe: func() *bool { calls++; return ptrTo(true) }}

	p.Observe()
	now = now.Add(61 * time.Second)
	p.Observe()

	if calls != 2 {
		t.Fatalf("probe ran %d times across two Observes 61 s apart, want 2", calls)
	}
}

// A fresh nil must REPLACE a cached true. Continuing to report a stale true because the current answer
// is "I could not tell" is how a dead SUT stays green.
func TestSUTProbe_ANotMeasuredResultReplacesAnEarlierTrue(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	answer := ptrTo(true)
	p := &sutProbe{every: time.Minute, now: func() time.Time { return now },
		probe: func() *bool { return answer }}

	p.Observe()
	answer = nil
	now = now.Add(61 * time.Second)
	got, at := p.Observe()

	if got != nil {
		t.Fatalf("a probe that now reports NOT MEASURED still returned %v — a stale true was kept "+
			"because the current answer was an unknown", *got)
	}
	if at == nil || !at.Equal(now) {
		t.Errorf("checked-at = %v, want %v: 'not measured, as of now' is itself a measurement and "+
			"must carry its own time", at, now)
	}
}

// ⚠ BEST-EFFORT, NON-FATAL. A panicking probe must not take the poll down with it.
func TestSUTProbe_APanickingProbeIsSurvivedAndReportsNotMeasured(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	p := &sutProbe{every: time.Minute, now: func() time.Time { return now },
		probe: func() *bool { panic("the SUT's config is malformed") }}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a panicking probe escaped Observe: %v.\nThe poll IS the heartbeat — a flaky SUT "+
				"that can break it would make its own executor read as dead", r)
		}
	}()
	got, _ := p.Observe()
	if got != nil {
		t.Errorf("a probe that panicked produced a verdict (%v) instead of NOT MEASURED", *got)
	}
}

// Before anything has been probed the answer is nil and there is NO timestamp — never a fabricated
// false, and never a time nothing happened at.
func TestSUTProbe_ADisabledProbeReportsNothingRatherThanFalse(t *testing.T) {
	p := &sutProbe{every: time.Minute, now: time.Now, probe: nil}
	got, at := p.Observe()
	if got != nil {
		t.Errorf("with no probe configured the result was %v, want nil", *got)
	}
	if at != nil {
		t.Errorf("with no probe configured a timestamp %v was reported for a check that never ran", at)
	}
}

// A zero interval must NOT mean "probe on every poll" — that is the §0.4 failure mode arriving through a
// forgotten field rather than a decision.
func TestSUTProbe_AZeroIntervalDefaultsRatherThanProbingContinuously(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	calls := 0
	p := &sutProbe{every: 0, now: func() time.Time { return now },
		probe: func() *bool { calls++; return ptrTo(true) }}

	p.Observe()
	now = now.Add(30 * time.Second)
	p.Observe()

	if calls != 1 {
		t.Fatalf("probe ran %d times in 30 s with every=0, want 1 (the default floor)", calls)
	}
}

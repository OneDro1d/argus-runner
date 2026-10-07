package runner

import (
	"context"
	"testing"
	"time"
)

// The defect this pins (2026-07-24, found by the UC161 cross-replica fence race): the run-direct
// path builds an Executor and calls startHeartbeat WITHOUT going through defaults(), which is the
// only place HeartbeatEvery was defaulted. So HeartbeatEvery stayed 0 and startHeartbeat did
// time.NewTicker(0) →
//
//   panic: non-positive interval for NewTicker
//     internal/runner/executor.go:74 startHeartbeat.func2
//
// The winning replica of the fence race crashed at that panic. The federated Loop() path never hit
// it because Loop calls defaults(); the direct path does not. The ticker-interval invariant belongs
// where the ticker is created, so the fix guards startHeartbeat itself — every caller is protected,
// not just the two that exist today.
//
// A non-nil Client is required for startHeartbeat to reach the ticker (a nil Client early-returns).
// The default is 30s, which never fires inside these sub-second tests, so the client is never called.

func TestStartHeartbeat_zeroIntervalDoesNotPanic(t *testing.T) {
	e := &Executor{Client: &Client{}} // HeartbeatEvery deliberately left 0 — the run-direct state
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("startHeartbeat panicked on a zero interval: %v", r)
		}
	}()
	stop := e.startHeartbeat(context.Background(), "20260724T120000000", "", "full")
	stop()
	if e.HeartbeatEvery <= 0 {
		t.Errorf("HeartbeatEvery still non-positive (%v) — the ticker guard did not default it", e.HeartbeatEvery)
	}
}

// A non-nil Client with an explicit interval must keep it — the guard defaults only a non-positive value.
func TestStartHeartbeat_keepsAnExplicitInterval(t *testing.T) {
	e := &Executor{Client: &Client{}, HeartbeatEvery: 7 * time.Second}
	stop := e.startHeartbeat(context.Background(), "20260724T120000000", "", "full")
	stop()
	if e.HeartbeatEvery != 7*time.Second {
		t.Errorf("HeartbeatEvery = %v, want 7s (the guard must not override an explicit value)", e.HeartbeatEvery)
	}
}

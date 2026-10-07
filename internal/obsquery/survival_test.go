package obsquery

import (
	"fmt"
	"testing"
	"time"
)

// fakeProm is a PromQuery test double keyed by (query, unix-seconds-of-`at`) -> value.
type fakeProm struct {
	values map[string]float64
	err    error
}

func (f *fakeProm) InstantValue(query string, at time.Time) (float64, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	key := fmt.Sprintf("%s@%d", query, at.Unix())
	v, ok := f.values[key]
	return v, ok, nil
}

func window() SurvivalWindow {
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	return SurvivalWindow{
		BaselineStart: base, BaselineEnd: base.Add(1 * time.Minute),
		RunStart: base.Add(1 * time.Minute), RunEnd: base.Add(2 * time.Minute),
	}
}

// AC-11 core case: a throttled pod (the run window's throttle count is higher than the baseline's)
// yields DEGRADED, by windowed delta.
func TestSurvivalPlaneRead_ThrottledPodYieldsDegraded(t *testing.T) {
	w := window()
	throttleQ := fmt.Sprintf(DefaultThrottleQuery, "orderservice-.*")
	restartQ := fmt.Sprintf(DefaultRestartQuery, "orderservice-.*")
	q := &fakeProm{values: map[string]float64{
		fmt.Sprintf("%s@%d", throttleQ, w.RunEnd.Unix()):      12,
		fmt.Sprintf("%s@%d", throttleQ, w.BaselineEnd.Unix()): 0,
		fmt.Sprintf("%s@%d", restartQ, w.RunEnd.Unix()):       0,
		fmt.Sprintf("%s@%d", restartQ, w.BaselineEnd.Unix()):  0,
	}}
	got := SurvivalPlaneRead(q, "orderservice-.*", w)
	if !got.Available {
		t.Fatalf("Available = false, want true: %+v", got)
	}
	if !got.Degraded {
		t.Fatalf("Degraded = false, want true (throttle delta +12): %+v", got)
	}
	if got.Reason == "" {
		t.Errorf("Reason must explain the delta")
	}
}

// A pod that was ALREADY throttling before the run (baseline == run) is not degraded BY the run —
// the windowed delta is 0.
func TestSurvivalPlaneRead_PreExistingThrottleNotDegraded(t *testing.T) {
	w := window()
	throttleQ := fmt.Sprintf(DefaultThrottleQuery, "orderservice-.*")
	restartQ := fmt.Sprintf(DefaultRestartQuery, "orderservice-.*")
	q := &fakeProm{values: map[string]float64{
		fmt.Sprintf("%s@%d", throttleQ, w.RunEnd.Unix()):      5,
		fmt.Sprintf("%s@%d", throttleQ, w.BaselineEnd.Unix()): 5,
		fmt.Sprintf("%s@%d", restartQ, w.RunEnd.Unix()):       0,
		fmt.Sprintf("%s@%d", restartQ, w.BaselineEnd.Unix()):  0,
	}}
	got := SurvivalPlaneRead(q, "orderservice-.*", w)
	if got.Degraded {
		t.Fatalf("a pod already throttling before the run must not be DEGRADED by the run: %+v", got)
	}
}

// A restarting pod (no throttle signal) also yields DEGRADED.
func TestSurvivalPlaneRead_RestartingPodYieldsDegraded(t *testing.T) {
	w := window()
	throttleQ := fmt.Sprintf(DefaultThrottleQuery, "orderservice-.*")
	restartQ := fmt.Sprintf(DefaultRestartQuery, "orderservice-.*")
	q := &fakeProm{values: map[string]float64{
		fmt.Sprintf("%s@%d", throttleQ, w.RunEnd.Unix()):      0,
		fmt.Sprintf("%s@%d", throttleQ, w.BaselineEnd.Unix()): 0,
		fmt.Sprintf("%s@%d", restartQ, w.RunEnd.Unix()):       1,
		fmt.Sprintf("%s@%d", restartQ, w.BaselineEnd.Unix()):  0,
	}}
	got := SurvivalPlaneRead(q, "orderservice-.*", w)
	if !got.Degraded {
		t.Fatalf("a restart delta must yield DEGRADED: %+v", got)
	}
}

// Best-effort (VR-C6): a Prometheus that cannot be reached answers Available:false and never panics
// or blocks.
func TestSurvivalPlaneRead_UnavailableIsBestEffort(t *testing.T) {
	q := &fakeProm{err: fmt.Errorf("connection refused")}
	got := SurvivalPlaneRead(q, "orderservice-.*", window())
	if got.Available {
		t.Fatalf("Available = true on a query error, want false: %+v", got)
	}
	if got.Degraded {
		t.Fatalf("an unavailable read must never claim DEGRADED: %+v", got)
	}
}

// A nil query or an empty pod selector is inert — never a nil-pointer panic.
func TestSurvivalPlaneRead_NilQueryOrEmptySelectorIsInert(t *testing.T) {
	if got := SurvivalPlaneRead(nil, "x", window()); got.Available || got.Degraded {
		t.Errorf("nil query must be a no-op: %+v", got)
	}
	if got := SurvivalPlaneRead(&fakeProm{}, "", window()); got.Available || got.Degraded {
		t.Errorf("empty selector must be a no-op: %+v", got)
	}
}

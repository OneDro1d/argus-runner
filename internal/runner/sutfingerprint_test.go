package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Migration 050, EXECUTOR HALF — reading the SUT's revision as a LEVEL without disturbing the EDGE.
//
// The run path records which revision each run exercised. The DeploymentWatcher reports when that
// revision CHANGES. They read the same probe and must stay independent, because the watcher's answer
// is computed against a baseline it owns.

func TestProbeSutFingerprint_NoProbeIsNotMeasured(t *testing.T) {
	fp, at := probeSutFingerprint(context.Background(), nil)
	if fp != "" || at != nil {
		t.Fatalf("got %q/%v, want not-measured — a SUT that declares no deployment_probe has no revision "+
			"to report, and inventing one is the whole failure this column exists to avoid", fp, at)
	}
}

// ⛔ A FAILED PROBE IS NOT A RUN FAILURE, AND NOT A GUESS. Returning an error here would fail runs on
// a SUT that answers tests but not its health endpoint; returning a remembered value would report a
// revision nobody measured.
func TestProbeSutFingerprint_ErrorIsNotMeasuredAndNotFatal(t *testing.T) {
	boom := func(context.Context) (string, error) { return "", errors.New("probe refused") }
	fp, at := probeSutFingerprint(context.Background(), boom)
	if fp != "" || at != nil {
		t.Fatalf("got %q/%v, want not-measured after a probe error", fp, at)
	}
}

func TestProbeSutFingerprint_BlankIsNotMeasured(t *testing.T) {
	for _, s := range []string{"", "   ", "\n\t "} {
		fp, at := probeSutFingerprint(context.Background(), func(context.Context) (string, error) { return s, nil })
		if fp != "" || at != nil {
			t.Fatalf("probe returning %q gave %q/%v, want not-measured", s, fp, at)
		}
	}
}

// The two values move together by construction: there is no input for which one is set and the other
// is not. That pairing is what lets the panel say "the SUT was X shortly beforehand" instead of
// asserting "this run hit X" — without the timestamp it could only assert the stronger claim.
func TestProbeSutFingerprint_MeasuredCarriesBothOrNeither(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	fp, at := probeSutFingerprint(context.Background(), func(context.Context) (string, error) {
		return "  build-9ac31  ", nil // trimmed, like the watcher trims
	})
	if fp != "build-9ac31" {
		t.Fatalf("fingerprint = %q, want the trimmed build-9ac31", fp)
	}
	if at == nil {
		t.Fatal("a measured fingerprint arrived with no measurement time")
	}
	if at.Before(before) || at.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("measurement time %v is not around now", at)
	}
}

// ⛔⛔ THE LOAD-BEARING ONE. The run path reads the SUT's fingerprint on EVERY run; the watcher must
// still see the next redeploy. If a level read ever advanced the watcher's baseline — lastSeen, have,
// or lastProbe — a redeploy the run had already observed would be SILENTLY SWALLOWED: no marker, no
// Timeline entry, no error, and instances.latest_deploy_marker left pointing at the old revision.
//
// This is a REGRESSION FENCE, not a demonstration. Today it passes trivially, because ExecConfig holds
// the raw FingerprintFunc and cannot reach the watcher at all. It exists to go red the day someone
// "tidies up" by routing the run path through the watcher — which is the natural-looking refactor,
// since the watcher is the thing that owns the probe everywhere else.
func TestSutFingerprint_LevelReadDoesNotDisturbTheMarkerEdge(t *testing.T) {
	cur := "build-one"
	probe := func(context.Context) (string, error) { return cur, nil }

	// ONE probe, both readers — exactly how runner.go wires them.
	w := &DeploymentWatcher{Fingerprint: probe, MinInterval: 0, Now: time.Now}
	ctx := context.Background()

	// Baseline established by the watcher's own first Check, which never emits a marker.
	if m, changed, err := w.Check(ctx); err != nil || changed || m != nil {
		t.Fatalf("first Check = %v/%v/%v, want the baseline-only (nil,false,nil)", m, changed, err)
	}

	// Several runs happen against the unchanged SUT, each taking a level reading.
	for i := 0; i < 5; i++ {
		if fp, at := probeSutFingerprint(ctx, probe); fp != "build-one" || at == nil {
			t.Fatalf("run %d measured %q/%v, want build-one", i, fp, at)
		}
	}

	// Now the SUT is genuinely redeployed, and a run observes the new revision BEFORE the watcher does.
	cur = "build-two"
	if fp, _ := probeSutFingerprint(ctx, probe); fp != "build-two" {
		t.Fatalf("post-redeploy run measured %q, want build-two", fp)
	}

	// The watcher must STILL report the change. This is the assertion that matters.
	m, changed, err := w.Check(ctx)
	if err != nil {
		t.Fatalf("Check after redeploy: %v", err)
	}
	if !changed || m == nil {
		t.Fatal("THE REDEPLOY MARKER WAS SWALLOWED. A per-run level read consumed the edge the watcher " +
			"exists to detect: instances.latest_deploy_marker would keep pointing at the old revision " +
			"and the Timeline would show no deploy. The run path must never touch the watcher's baseline.")
	}

	// And it must not double-report: the edge is consumed exactly once, by the watcher.
	if _, changed2, _ := w.Check(ctx); changed2 {
		t.Fatal("Check reported the same redeploy twice")
	}
}

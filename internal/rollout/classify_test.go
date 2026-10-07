package rollout

import "testing"

// AC-D51 / issue #315 — the shared classifier both the executor's autoscaler (internal/runner) and the
// control plane (internal/control) must use, so an ordinary scale-down, a real stalled rollout, and a
// pure readiness shortfall each get exactly one verdict instead of three independently-computed ones
// that can disagree.

// The park tuple, measured 2026-09-28 on 0.3.43: a run scales the executor 3 -> 1, and for about a
// second status.replicas has not caught up (spec=1 total=3 ready=3 updated=3 — every pod is on the
// CURRENT template, there is simply a teardown in flight). The old `total > desired` rule read this as
// half-landed and would have printed "0 pod(s) still run the PREVIOUS image", a headline contradicted
// by its own count.
func TestClassify_ParkIsNotHalfLanded(t *testing.T) {
	v := Classify(1, 3, 3, 3)
	if v.State != Complete {
		t.Errorf("desired=1 total=3 ready=3 updated=3 (an ordinary scale-down) => %q, want %q", v.State, Complete)
	}
	if v.StalePods != 0 {
		t.Errorf("StalePods = %d on a pure scale-down, want 0 — every existing pod is on the current template", v.StalePods)
	}
	if v.ReadinessShortfall {
		t.Errorf("ReadinessShortfall = true on a scale-down where ready(3) >= desired(1)")
	}
}

// The 2026-08-10 case the check exists for: a Pending new-template pod behind a Running old-template
// one. This one MUST stay half-landed.
func TestClassify_TheStalledRolloutStaysHalfLanded(t *testing.T) {
	v := Classify(1, 2, 1, 1)
	if v.State != HalfLanded {
		t.Fatalf("desired=1 total=2 ready=1 updated=1 (2026-08-10 stall) => %q, want %q", v.State, HalfLanded)
	}
	if v.StalePods != 1 {
		t.Errorf("StalePods = %d, want 1 — one old-template pod is still there (total-updated)", v.StalePods)
	}
}

// A pure readiness shortfall (every pod is on the current template, but a quota or a slow node holds
// one back) must NOT become half-landed — that is what sends an operator to check whether the image is
// resolvable, which is the wrong remedy for a resource or scheduling problem.
func TestClassify_ReadinessShortfallIsNotHalfLanded(t *testing.T) {
	v := Classify(3, 3, 2, 3)
	if v.State != Complete {
		t.Errorf("desired=3 total=3 ready=2 updated=3 (readiness-only) => %q, want %q", v.State, Complete)
	}
	if !v.ReadinessShortfall {
		t.Error("ReadinessShortfall = false on desired=3 ready=2 — the shortfall must still be reported, just not as half-landed")
	}
	if v.ReadyGap != 1 {
		t.Errorf("ReadyGap = %d, want 1", v.ReadyGap)
	}
	if v.StalePods != 0 {
		t.Errorf("StalePods = %d, want 0", v.StalePods)
	}
}

// The no-surge strategy the executor Deployment renders with (k8srender.go: maxSurge 0, maxUnavailable
// 1) can leave a stuck NEW pod with the old one already gone: desired=1 total=1 ready=0 updated=1. That
// is not half-landed (there is no old pod left to be stuck), and it must still be visible as a readiness
// shortfall.
func TestClassify_ANoSurgeStuckNewPodIsAReadinessShortfallNotHalfLanded(t *testing.T) {
	v := Classify(1, 1, 0, 1)
	if v.State != Complete {
		t.Errorf("desired=1 total=1 ready=0 updated=1 => %q, want %q", v.State, Complete)
	}
	if !v.ReadinessShortfall || v.ReadyGap != 1 {
		t.Errorf("ReadinessShortfall/ReadyGap = %v/%d, want true/1", v.ReadinessShortfall, v.ReadyGap)
	}
}

// A rollout can be genuinely half-landed AND short of ready at once — both true, neither implies the
// other (mirrors TestAutoscaler_ObservesWhatItActuallyGot in internal/runner).
func TestClassify_HalfLandedAndShortfallCanBothBeTrue(t *testing.T) {
	v := Classify(3, 3, 2, 2)
	if v.State != HalfLanded {
		t.Errorf("desired=3 total=3 ready=2 updated=2 => %q, want %q", v.State, HalfLanded)
	}
	if v.StalePods != 1 {
		t.Errorf("StalePods = %d, want 1", v.StalePods)
	}
	if !v.ReadinessShortfall || v.ReadyGap != 1 {
		t.Errorf("ReadinessShortfall/ReadyGap = %v/%d, want true/1", v.ReadinessShortfall, v.ReadyGap)
	}
}

// A deliberately scaled-to-zero instance is never mid-rollout, whatever ready/total/updated say.
func TestClassify_ScaledToZeroIsComplete(t *testing.T) {
	if v := Classify(0, 0, 0, 0); v.State != Complete {
		t.Errorf("desired=0 => %q, want %q", v.State, Complete)
	}
}

// The steady state.
func TestClassify_FullyRolledIsComplete(t *testing.T) {
	for _, v := range []Verdict{Classify(3, 3, 3, 3), Classify(1, 1, 1, 1)} {
		if v.State != Complete || v.StalePods != 0 || v.ReadinessShortfall {
			t.Errorf("steady state misclassified: %+v", v)
		}
	}
}

// ClassifyObserved is the nil-tolerant form the control plane needs: a compose executor's counts are
// all nil (never measured), which is not the same as a real 0.
func TestClassifyObserved_MissingCountsAreUnknown(t *testing.T) {
	one := 1
	cases := []struct {
		name                           string
		desired, total, ready, updated *int
	}{
		{"nothing reported", nil, nil, nil, nil},
		{"compose: no counts at all", &one, nil, &one, nil},
		{"desired missing", nil, &one, &one, &one},
	}
	for _, c := range cases {
		if v := ClassifyObserved(c.desired, c.total, c.ready, c.updated); v.State != Unknown {
			t.Errorf("%s => %q, want %q", c.name, v.State, Unknown)
		}
	}
}

func TestClassifyObserved_DelegatesToClassifyWhenComplete(t *testing.T) {
	one, two := 1, 2
	v := ClassifyObserved(&one, &two, &one, &one)
	if v.State != HalfLanded || v.StalePods != 1 {
		t.Errorf("ClassifyObserved(1,2,1,1) = %+v, want half-landed/1", v)
	}
}

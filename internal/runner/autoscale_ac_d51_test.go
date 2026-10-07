package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// AC-D51 / issue #315 — a park (3 -> 1) leaves Kubernetes still tearing down the surplus pods for about
// a second: spec=1 total=3 ready=3 updated=3, every pod already on the current template. The old
// `changed && (u < d || tot > d)` condition read `tot(3) > d(1)` as half-landed and would have printed
// "0 pod(s) still run the PREVIOUS image" — a headline contradicted by its own count. The HALF-LANDED
// line must not print on this tuple, ever.
func TestAutoscaler_AC_D51_ParkTransientIsNeverReportedHalfLanded(t *testing.T) {
	var lines []string
	a := &Autoscaler{
		Namespace: "ns", Deployment: "executor",
		Scale:  func(context.Context, string, string, int) error { return nil },
		Status: func(context.Context, string, string) (int, int, int, int, error) { return 1, 3, 3, 3, nil },
		Log:    func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) },
	}
	a.observe(context.Background())
	for _, l := range lines {
		if strings.Contains(l, "HALF-LANDED") {
			t.Fatalf("a pure scale-down (desired=1 total=3 ready=3 updated=3) logged a HALF-LANDED line: %q", l)
		}
	}
}

// The same park must not print the readiness line either: 3 ready of 1 desired is a SURPLUS being torn
// down, not a shortfall. That line used to fire on `d != r`. A real shortfall still prints it.
func TestAutoscaler_AC_D51_ParkSurplusIsNotAShortfall(t *testing.T) {
	logs := func(d, tot, r, u int) []string {
		var lines []string
		a := &Autoscaler{
			Namespace: "ns", Deployment: "executor",
			Scale:  func(context.Context, string, string, int) error { return nil },
			Status: func(context.Context, string, string) (int, int, int, int, error) { return d, tot, r, u, nil },
			Log:    func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) },
		}
		a.observe(context.Background())
		return lines
	}
	for _, l := range logs(1, 3, 3, 3) {
		if strings.Contains(l, "shortfall") {
			t.Fatalf("a park (desired=1 total=3 ready=3 updated=3) logged a shortfall: %q", l)
		}
	}
	found := false
	for _, l := range logs(3, 3, 2, 3) {
		found = found || strings.Contains(l, "2/3 replicas READY")
	}
	if !found {
		t.Fatal("a real shortfall (desired=3 ready=2) no longer logs its READY line")
	}
}

// When the classifier DOES say half-landed, the printed count (total-updated) must always be > 0 — the
// old code could print "0 pod(s) still run the PREVIOUS image" in the u<d branch when every existing
// pod was already on the current template (a scale-up in progress, not a stall).
func TestAutoscaler_AC_D51_HalfLandedCountIsAlwaysPositive(t *testing.T) {
	var lines []string
	a := &Autoscaler{
		Namespace: "ns", Deployment: "executor",
		Scale: func(context.Context, string, string, int) error { return nil },
		// desired=3, total=1, ready=1, updated=1: a scale-up in progress, only one pod created so far,
		// and it is on the CURRENT template. Not half-landed (no old pod anywhere) — just short of ready.
		Status: func(context.Context, string, string) (int, int, int, int, error) { return 3, 1, 1, 1, nil },
		Log:    func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) },
	}
	a.observe(context.Background())
	for _, l := range lines {
		if strings.Contains(l, "HALF-LANDED") {
			t.Fatalf("desired=3 total=1 ready=1 updated=1 (scale-up in progress, no old pod) logged HALF-LANDED: %q", l)
		}
	}
}

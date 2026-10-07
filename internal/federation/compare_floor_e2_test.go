package federation

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// ARGUS-CMP-11: the executor release E2 -- the first one that records tolerance values and load
// numbers and honours a member's target -- has a release behind it, so a control plane can refuse a member below it.

// newestReleasedWithE1 is the newest executor release when ARGUS-CMP-11 was written: 0.3.57, the release that carries E1.
// The E2 floor must name a version ABOVE it: a floor naming a shipped version would idle healthy executors for features
// none of them has. When E2 ships, the release PR raises this number with the tag.
var newestReleasedWithE1 = [3]int{0, 3, 57}

func TestCMP11_E2NamesTheNextReleaseAndNotOneThatIsAlreadyOut(t *testing.T) {
	v, ok := CompareFloorVersion(compare.FloorE2)
	if !ok {
		t.Fatalf("CompareFloorVersion(FloorE2) has no release: the control plane cannot refuse a member below E2")
	}
	floor, pok := ParseSemverCore(v)
	if !pok {
		t.Fatalf("the E2 floor %q is not a MAJOR.MINOR.PATCH version", v)
	}
	if cmpCore(floor, newestReleasedWithE1) <= 0 {
		t.Fatalf("E2 = %s, which is not above the newest release 0.3.57", v)
	}
	if e1, _ := ParseSemverCore(MinExecutorCompare); cmpCore(floor, e1) <= 0 {
		t.Fatalf("E2 = %s is not above E1 = %s", v, MinExecutorCompare)
	}
	if v != "0.3.58" {
		t.Errorf("E2 = %s, the repo names 0.3.58 as the next release", v)
	}
}

func TestCMP11_EveryKeyFiledUnderE2MapsToTheE2ReleaseAndAnUnknownLabelToNothing(t *testing.T) {
	want, _ := CompareFloorVersion(compare.FloorE2)
	n := 0
	for key, label := range compare.KeyFloors {
		if label != compare.FloorE2 {
			continue
		}
		n++
		if v, ok := CompareFloorVersion(label); !ok || v != want {
			t.Errorf("key %q is filed under E2 but maps to (%q, %v)", key, v, ok)
		}
	}
	if n != 3 {
		t.Errorf("%d keys are filed under E2, want Tolerance, Not Worse Than and CompareTarget", n)
	}
	for _, label := range []string{"", "E3", "e2"} {
		if v, ok := CompareFloorVersion(label); ok || v != "" {
			t.Errorf("CompareFloorVersion(%q) = (%q, %v); a label with no release behind it must not give a floor", label, v, ok)
		}
	}
}

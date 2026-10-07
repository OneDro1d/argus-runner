package federation

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// newestReleased is the newest executor release at the time ARGUS-CMP-4 was written. The floor must name a
// version ABOVE it: a floor naming a shipped version would idle healthy executors for a feature none of them
// has. When E1 ships, the release PR raises this number with the tag; until then this test is the reminder.
var newestReleased = [3]int{0, 3, 56}

func TestMinExecutorCompare_NamesAReleaseThatIsNotOutYet(t *testing.T) {
	floor, ok := ParseSemverCore(MinExecutorCompare)
	if !ok {
		t.Fatalf("MinExecutorCompare %q is not a MAJOR.MINOR.PATCH version", MinExecutorCompare)
	}
	if cmpCore(floor, newestReleased) <= 0 {
		t.Fatalf("MinExecutorCompare = %s, which is not above the newest release 0.3.56: an executor that already ships cannot have mode compare", MinExecutorCompare)
	}
	// and the AMQP Load floor is a different, lower number: the two are independent
	if amqp, _ := ParseSemverCore(MinExecutorAMQPLoad); cmpCore(floor, amqp) <= 0 {
		t.Fatalf("MinExecutorCompare %s is not above MinExecutorAMQPLoad %s", MinExecutorCompare, MinExecutorAMQPLoad)
	}
}

// The floor VALUE comes from compare.FloorE1: every key the pure core files under E1 maps to this release,
// and a label this control plane has no release for (E2 has none yet) maps to nothing -- never to a
// floor that would idle every executor.
func TestCompareFloorVersion_IsTheReleaseCompareFloorE1Names(t *testing.T) {
	if v, ok := CompareFloorVersion(compare.FloorE1); !ok || v != MinExecutorCompare {
		t.Fatalf("CompareFloorVersion(FloorE1) = %q, %v; want %q", v, ok, MinExecutorCompare)
	}
	n := 0
	for key, label := range compare.KeyFloors {
		if label != compare.FloorE1 {
			continue
		}
		n++
		if v, ok := CompareFloorVersion(label); !ok || v != MinExecutorCompare {
			t.Errorf("key %q is filed under E1 but maps to (%q, %v)", key, v, ok)
		}
	}
	if n == 0 {
		t.Fatal("compare.KeyFloors holds no E1 key: the table moved")
	}
	// ARGUS-CMP-11: E2 has a release now (compare_floor_e2_test.go); only a label nobody defined maps to nothing
	for _, label := range []string{"", "E3", "e1"} {
		if v, ok := CompareFloorVersion(label); ok || v != "" {
			t.Errorf("CompareFloorVersion(%q) = (%q, %v); a label with no release behind it must not give a floor", label, v, ok)
		}
	}
}

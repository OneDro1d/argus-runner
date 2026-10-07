package federation

// versionfloors_test.go — the THREE states an executor can be in, plus the one it must never be
//                         forced into.  (VR-V2 / V17-010)
//
// The states, and what each MEANS to an operator:
//
//   current              at or above the RECOMMENDED floor. Nothing about updating is shown at all.
//   update recommended   between the floors. Works normally; the operator is told a better one exists.
//   update required      below the ABSOLUTE floor. Refused work, told why and what to do.
//   unknown              cannot be ranked. NEVER refused.
//
// THE LAST ONE IS THE RULE THAT MATTERS. A version we cannot parse is a MISSING SIGNAL, and this
// codebase does not render a missing signal as a verdict — in either direction. Refusing work to a
// developer's locally-built executor (typically NEWER than anything in the fleet) because its version
// string is "0.0.0-src+abc" would be the same defect as calling an ancient one current.
//
// WHY THIS LIVES IN federation AND NOT IN control OR runner: both sides must reach the SAME verdict.
// The control plane decides what to publish and what the web shows; the executor decides whether to
// accept work, including when the control plane is unreachable. Two implementations of one rule is
// how the two run paths diverged in V17-012.

import "testing"

func TestFloorState(t *testing.T) {
	f := VersionFloors{Absolute: "0.3.10", Recommended: "0.3.20"}
	cases := []struct {
		name    string
		version string
		floors  VersionFloors
		want    string
	}{
		{"at the recommended floor", "0.3.20", f, FloorCurrent},
		{"above everything", "0.4.0", f, FloorCurrent},
		{"between the floors", "0.3.15", f, FloorUpdateRecommended},
		{"exactly at the absolute floor", "0.3.10", f, FloorUpdateRecommended},
		{"below the absolute floor", "0.3.9", f, FloorUpdateRequired},
		{"far below", "0.1.0", f, FloorUpdateRequired},

		// Never a verdict.
		{"unparseable version", "m3-iii27", f, FloorUnknown},
		{"empty version", "", f, FloorUnknown},
		{"a source build is NOT old", "0.0.0-src+deadbeef", f, FloorUnknown},
		{"an unstamped dev build is NOT old", "0.0.0-dev", f, FloorUnknown},

		// The control plane has published nothing (an executor that has never reached it, or one
		// talking to a pre-V17 control plane). Unknown, and specifically NOT refused: an executor
		// must not stop working because the other end is old.
		{"no floors published at all", "0.3.15", VersionFloors{}, FloorUnknown},
		{"only the absolute floor", "0.3.15", VersionFloors{Absolute: "0.3.10"}, FloorUnknown},

		// Build metadata is not a version difference: 0.3.20+m3 is 0.3.20.
		{"build metadata is ignored", "0.3.20+m3-iii27", f, FloorCurrent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FloorState(c.version, c.floors); got != c.want {
				t.Errorf("FloorState(%q, %+v) = %q, want %q", c.version, c.floors, got, c.want)
			}
		})
	}
}

// Only ONE state may stop an executor working, and it must be the positively-established one. This
// is the same principle VersionStateBlocks already encodes on the control-plane side, asserted here
// so the two cannot drift.
func TestFloorBlocks_OnlyARequiredUpdateStopsWork(t *testing.T) {
	for _, s := range []string{FloorCurrent, FloorUpdateRecommended, FloorUnknown} {
		if FloorBlocks(s) {
			t.Errorf("state %q blocks work; only %q may", s, FloorUpdateRequired)
		}
	}
	if !FloorBlocks(FloorUpdateRequired) {
		t.Errorf("state %q must block work — it is the whole point of the absolute floor", FloorUpdateRequired)
	}
}

package federation

import (
	"strconv"
	"strings"
)

// VersionFloors is what the control plane publishes about executor versions (VR-V1/VR-V5).
//
// It lives HERE, in the shared wire package, because both sides must reach the SAME verdict from it:
// the control plane decides what the web shows, and the executor decides whether to accept work —
// including when the control plane is unreachable. Two implementations of one rule is exactly how the
// direct and federated run paths diverged in V17-012.
type VersionFloors struct {
	// Absolute is the hard floor: below it an executor is refused work.
	Absolute string `json:"absolute"`
	// Recommended is the soft floor: below it an executor works, and the operator is told a better
	// one exists. At or above it, nothing about updating is shown at all.
	Recommended string `json:"recommended"`
}

// The four states. Three are reportable positions; the fourth is the honest absence of one.
const (
	FloorCurrent           = "current"
	FloorUpdateRecommended = "update_recommended"
	FloorUpdateRequired    = "update_required"
	FloorUnknown           = "unknown"
)

// FloorsFrom lifts the floors out of a poll response's VersionInfo.
func FloorsFrom(v VersionInfo) VersionFloors {
	return VersionFloors{
		Absolute:    strings.TrimSpace(v.MinSupportedRunnerVersion),
		Recommended: strings.TrimSpace(v.MinRecommendedExecutorVersion),
	}
}

// Complete reports whether both floors are published. An incomplete pair cannot rank anything, and
// saying so is better than ranking against half a policy.
func (f VersionFloors) Complete() bool { return f.Absolute != "" && f.Recommended != "" }

// FloorState ranks a running version against the published floors.
//
// UNKNOWN IS NEVER A VERDICT. A version that cannot be parsed, or floors that were never published,
// yield FloorUnknown — and FloorBlocks says that does not stop work. Refusing a developer's
// locally-built executor (typically NEWER than anything in the fleet) because its version reads
// "0.0.0-src+abc" would be the same defect as calling an ancient one current, pointed the other way.
func FloorState(version string, f VersionFloors) string {
	v := strings.TrimSpace(version)
	if v == "" || !f.Complete() {
		return FloorUnknown
	}
	// A no-release-identity sentinel is UNKNOWN, not old. buildinfo emits "0.0.0-dev" for an
	// unstamped build and "0.0.0-src+<rev>" for one built from a checkout; both parse as 0.0.0, which
	// ranks below every floor. The 0.0.0 is an accident of the sentinel's spelling, not a claim about
	// age. (The control plane has carried this same carve-out since UC071.)
	if strings.HasPrefix(v, "0.0.0-dev") || strings.HasPrefix(v, "0.0.0-src") {
		return FloorUnknown
	}
	vp, vok := ParseSemverCore(v)
	ap, aok := ParseSemverCore(f.Absolute)
	rp, rok := ParseSemverCore(f.Recommended)
	if !vok || !aok || !rok {
		return FloorUnknown
	}
	if cmpCore(vp, ap) < 0 {
		return FloorUpdateRequired
	}
	if cmpCore(vp, rp) < 0 {
		return FloorUpdateRecommended
	}
	return FloorCurrent
}

// FloorBlocks reports whether a state should stop the executor accepting work. ONLY a
// positively-established "below the absolute floor" does — every flavour of "I cannot tell" lets work
// proceed, because wrongly idling a healthy executor costs more than running one we could not rank.
func FloorBlocks(state string) bool { return state == FloorUpdateRequired }

// ParseSemverCore reads the MAJOR.MINOR.PATCH core of a version, ignoring any pre-release or build
// metadata after it. "0.3.20+m3-iii27" and "0.3.20" are the same version.
func ParseSemverCore(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func cmpCore(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

package federation

import "github.com/OneDro1d/argus-runner/internal/compare"

// MinExecutorCompare is the executor release the compare mission calls E1: the first one that records
// the outputs of a `compare` run, runs mode `compare` and answers the comparison's relay verb (ARGUS-CMP-3,
// ). The control plane refuses to queue a comparison member for an instance whose
// executor POSITIVELY ranks below it (control.compareEnqueueRefusal) -- the same friendliness-plus-
// defence-in-depth mechanism as MinExecutorAMQPLoad; the guarantee for an executor the floor cannot
// rank is that the set rides a field an old executor does not read (design 11.2).
//
// ⛔ It names a release that did NOT exist when it was written (the newest was 0.3.56): the release PR
// that ships E1 is the one that cuts this tag, and a floor naming an already-shipped version would
// wrongly idle a healthy executor. If E1 ships under another number, change this constant in that PR.
const MinExecutorCompare = "0.3.57"

// MinExecutorCompareE2 is the release the compare mission calls E2 (ARGUS-CMP-11 and -17): the first one
// that records the numbers of a tolerant check (`**Tolerance**`) and of a load check (`**Not Worse Than**`) and that selects
// the target a comparison member names (RunAssignment.CompareTarget). The control plane refuses to queue a member that
// would need one of the three on an executor that POSITIVELY ranks below it.
//
// ⛔ Same rule as MinExecutorCompare: it names a release that did NOT exist when it was written (the newest was 0.3.57, E1),
// so it must stay above the newest tag until the release PR that ships E2 cuts it. If E2 ships under another number, change
// this constant in that PR; TestCMP11_E2NamesTheNextReleaseAndNotOneThatIsAlreadyOut is the reminder.
const MinExecutorCompareE2 = "0.3.58"

// RankedAtOrAbove reports whether runnerVersion is POSITIVELY known to be at or above floorVersion: it parses as a release,
// is not a 0.0.0-* source build, and does not rank below the floor. It is the opposite default of "ranked below": an executor
// that cannot be ranked is NOT shown to be at or above. ARGUS-CMP-11 fix F2 uses it at pickup for a member's target, the one
// thing an old executor ignores silently (a released 0.3.57 carries RunAssignment.CompareTarget on the wire and reads it nowhere).
func RankedAtOrAbove(runnerVersion, floorVersion string) bool {
	have, ok := ParseSemverCore(runnerVersion)
	floor, fok := ParseSemverCore(floorVersion)
	if !ok || !fok || have == [3]int{} {
		return false
	}
	for i := 0; i < 3; i++ {
		if have[i] != floor[i] {
			return have[i] > floor[i]
		}
	}
	return true
}

// CompareFloorVersion maps a compare.KeyFloors label to the executor release it means. ok is false for
// a label this control plane does not know a release for: the caller must then not refuse on it, because a
// floor with no release behind it would idle every executor.
func CompareFloorVersion(label string) (version string, ok bool) {
	switch label {
	case compare.FloorE1:
		return MinExecutorCompare, true
	case compare.FloorE2:
		return MinExecutorCompareE2, true
	}
	return "", false
}

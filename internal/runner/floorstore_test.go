package runner

import (
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// floorstore_test.go — the executor keeps the floors ACROSS A RESTART.  (VR-V5 / V17-010)
//
// The owner's instruction was "assume the version numbers are the same as last time (so keep it in
// memory)". The intent is right and memory alone does not achieve it: a container restart clears
// memory, so an executor that restarts while the control plane is unreachable comes back knowing
// nothing — and must then either refuse everything or allow everything, both of which are verdicts
// invented from an absence.

func TestFloorStore_SurvivesARestart(t *testing.T) {
	id := filepath.Join(t.TempDir(), "identity.key")
	want := federation.VersionFloors{Absolute: "0.3.10", Recommended: "0.3.20"}

	NewFloorStore(id).Observe(want)

	// A NEW store over the same directory is what a restarted container has.
	if got := NewFloorStore(id).Get(); got != want {
		t.Fatalf("after a restart the executor knows %+v, want %+v — it would have to invent a "+
			"verdict from an absence", got, want)
	}
}

// An executor that has NEVER been told anything must not be judged. This is the other half of the
// rule: fail-closed once known, but never closed on something never known.
func TestFloorStore_NothingKnownIsNotAVerdict(t *testing.T) {
	fs := NewFloorStore(filepath.Join(t.TempDir(), "identity.key"))
	if got := fs.State("0.1.0"); got != federation.FloorUnknown {
		t.Errorf("a never-told executor reports %q for an ancient version; want %q — being told "+
			"nothing is not evidence of being old", got, federation.FloorUnknown)
	}
	if federation.FloorBlocks(fs.State("0.1.0")) {
		t.Error("an executor was refused work on the strength of floors it was never given")
	}
}

// A control plane that publishes only half a policy — or none — must NOT erase what we already knew.
// That is the difference between "the other end got older" and "we forgot".
func TestFloorStore_IncompleteFloorsDoNotErase(t *testing.T) {
	id := filepath.Join(t.TempDir(), "identity.key")
	known := federation.VersionFloors{Absolute: "0.3.10", Recommended: "0.3.20"}
	fs := NewFloorStore(id)
	fs.Observe(known)

	fs.Observe(federation.VersionFloors{Absolute: "0.9.9"}) // recommended missing → incomplete
	fs.Observe(federation.VersionFloors{})                  // an old control plane publishing neither

	if got := fs.Get(); got != known {
		t.Errorf("last-known floors were overwritten by an incomplete publication: %+v, want %+v", got, known)
	}
	if got := NewFloorStore(id).Get(); got != known {
		t.Errorf("the erasure reached DISK: %+v, want %+v", got, known)
	}
}

// Once known, they stay known — an executor already judged below the absolute floor does not become
// usable again because the control plane went away.
func TestFloorStore_StateHoldsWhenTheControlPlaneGoesQuiet(t *testing.T) {
	id := filepath.Join(t.TempDir(), "identity.key")
	fs := NewFloorStore(id)
	fs.Observe(federation.VersionFloors{Absolute: "0.3.10", Recommended: "0.3.20"})

	if got := fs.State("0.3.9"); got != federation.FloorUpdateRequired {
		t.Fatalf("State = %q, want %q", got, federation.FloorUpdateRequired)
	}
	// The control plane is unreachable: no Observe calls at all. The verdict must not soften.
	if got := NewFloorStore(id).State("0.3.9"); got != federation.FloorUpdateRequired {
		t.Errorf("after a restart with the control plane unreachable, State = %q, want %q — an "+
			"executor must not become usable again because the other end went quiet", got, federation.FloorUpdateRequired)
	}
}

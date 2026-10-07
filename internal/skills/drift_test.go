package skills_test

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/skills"
)

// TestUnknownHatInstallsNothing — a removal driven by a guess is the class of bug internal/skills
// exists to prevent, so an unrecognised hat must yield an empty list rather than a default.
func TestUnknownHatInstallsNothing(t *testing.T) {
	if got := skills.Installed(role.Role("banana")); len(got) != 0 {
		t.Fatalf("an unknown hat returned %v — teardown would delete those from a folder whose hat it "+
			"could not identify", got)
	}
	if got := skills.Installed(""); len(got) != 0 {
		t.Fatalf("the zero-value hat returned %v; RemoveResult.Hat is the zero value when no folder "+
			"matched, and that path must remove nothing", got)
	}
}

// TestInstalledReturnsAFreshCopy — a shared backing array would let one caller change what every
// later caller sees. (It used to say "what teardown deletes"; teardown deletes no skills now.)
func TestInstalledReturnsAFreshCopy(t *testing.T) {
	a := skills.Installed(role.Test)
	if len(a) == 0 {
		t.Fatal("the test hat installs nothing")
	}
	a[0] = "clobbered"
	if b := skills.Installed(role.Test); b[0] == "clobbered" {
		t.Fatal("Installed handed out a shared backing array — mutating one caller's slice changed " +
			"what every later teardown would remove")
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

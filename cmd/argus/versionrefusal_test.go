package main

// versionrefusal_test.go — an executor below the ABSOLUTE floor refuses to run, AT CALL TIME.
//                          (VR-V4 / V17-010)
//
// WHY AT CALL TIME. runner__run is ASYNC: without a synchronous refusal it returns a run_id, and the
// agent has to infer the problem from a later report showing total:0 — which reads exactly like a run
// that happened and found nothing wrong. The pre-accept hook's own comment already states the
// requirement: "the agent SAY the run cannot be run, so the answer must be available at call time".
//
// WHY THIS PATH AND NOT A CONTROL-PLANE CALL. The refusal must hold on the LOCAL fast loop and with
// the control plane unreachable — the two situations where "we could not check, so carry on" used to
// be the answer. It reads the executor's DURABLE copy of the floors (VR-V5), so it needs nobody.
//
// The message names THREE things on purpose: what is running, what is required, and what to do. A
// refusal that names fewer is a puzzle, and this round exists because operators were handed puzzles.

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func TestVersionRefusal_Message(t *testing.T) {
	floors := federation.VersionFloors{Absolute: "0.3.10", Recommended: "0.3.20"}

	cases := []struct {
		name    string
		running string
		floors  federation.VersionFloors
		refuse  bool
	}{
		{"below the absolute floor is refused", "0.3.9", floors, true},
		{"between the floors still runs", "0.3.15", floors, false},
		{"at or above the recommended floor runs", "0.3.20", floors, false},

		// The rule that must not be got wrong: an absence is not a verdict.
		{"an unrankable version is NOT refused", "m3-iii27", floors, false},
		{"a source build is NOT refused", "0.0.0-src+abc", floors, false},
		{"floors never published means NOT refused", "0.1.0", federation.VersionFloors{}, false},
		{"half a policy means NOT refused", "0.1.0", federation.VersionFloors{Absolute: "0.3.10"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocked := federation.FloorBlocks(federation.FloorState(c.running, c.floors))
			if blocked != c.refuse {
				t.Fatalf("running %q against %+v: blocked=%v, want %v", c.running, c.floors, blocked, c.refuse)
			}
		})
	}
}

// The refusal text is part of the contract, not decoration: an operator reading it must learn what to
// do without going anywhere else, and must NOT be sent to inspect their scenarios or their token —
// which is what every other version-shaped failure in this codebase has historically done.
func TestVersionRefusal_TextNamesRunningRequiredAndRemedy(t *testing.T) {
	msg := versionRefusalFor("0.3.9", federation.VersionFloors{Absolute: "0.3.10", Recommended: "0.3.20"})
	if msg == "" {
		t.Fatal("an executor below the absolute floor produced no refusal")
	}
	for _, want := range []string{"0.3.9", "0.3.10", "Environments"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %q — a refusal that names less than "+
				"running/required/remedy is a puzzle:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "Nothing is wrong with your scenarios") {
		t.Errorf("the refusal does not rule out the WRONG causes; an operator will go and check them:\n%s", msg)
	}
	if got := versionRefusalFor("0.3.20", federation.VersionFloors{Absolute: "0.3.10", Recommended: "0.3.20"}); got != "" {
		t.Errorf("a current executor was refused: %s", got)
	}
}

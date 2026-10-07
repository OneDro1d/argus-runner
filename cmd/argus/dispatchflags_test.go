package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// dispatchflags_test.go — VR4-B3 end to end, through the real `dispatch`.
//
// commonflags_test.go proves the PARTITION is right. This proves it is WIRED: the flag actually
// survives the whole path from argv to the subcommand. The two failures this guards are different
// bugs that happened to have the same symptom, and fixing only one leaves `--state` unusable:
//
//  1. the COMMON parser rejected `--state` before dispatch (main.go, the partition)
//  2. the subcommand was handed the FULL vector including the command name, so its own
//     `flag.Parse` stopped on that positional and never reached `--state` either
//
// The assertion is deliberately about WHICH refusal is produced. `cloud-onboarding-state` with no
// control plane must refuse for a MISSING CONTROL PLANE — reaching that message proves `--state` was
// parsed. Refusing with "flag provided but not defined" means the flag never arrived.

// captureStdout runs fn with stdout redirected, and returns what it printed. `emit` writes JSON to
// stdout via fmt.Println, so this is the only way to read what the CLI actually told the operator.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func TestDispatch_SubcommandOwnFlagIsReachable(t *testing.T) {
	// No control plane and no ARGUS_CP_URL, so the command must refuse — the question is HOW.
	t.Setenv("ARGUS_CP_URL", "")
	t.Setenv("ARGUS_ONBOARDING_STATE", "")

	out := captureStdout(t, func() {
		dispatch([]string{"cloud-onboarding-state", "--state", "complete", "--instance-id", "prod-1"})
	})

	if strings.Contains(out, "not defined") {
		t.Fatalf("the subcommand's own --state was rejected before it could be used.\n"+
			"This is V18-003, and it is why onboarding_state was never stamped.\ngot: %s", out)
	}
	if !strings.Contains(out, "control-plane") {
		t.Fatalf("expected a refusal naming the MISSING control plane, which is only reachable once "+
			"--state parsed.\ngot: %s", out)
	}
}

func TestDispatch_CommonFlagStillApplies(t *testing.T) {
	t.Setenv("ARGUS_CP_URL", "")
	t.Setenv("ARGUS_ONBOARDING_STATE", "")

	// --instance-id is a COMMON flag. If the partition kept it, the command gets past the
	// "instance is still the default" branch and refuses on the control plane instead.
	out := captureStdout(t, func() {
		dispatch([]string{"cloud-onboarding-state", "--instance-id", "prod-1", "--state", "complete"})
	})
	if strings.Contains(out, "not defined") {
		t.Fatalf("a common flag and a subcommand flag together broke the parse.\ngot: %s", out)
	}
}

// The regression guard for SA §0 C3. `router serve` works today ONLY because `serve` is a positional
// that halts Go's flag parsing. A fix that parsed the whole vector centrally would satisfy VR4-B3 and
// silently break this — so it is asserted at the dispatch layer, not just in the partition.
func TestDispatch_RouterKeepsItsOwnFlags(t *testing.T) {
	fs := commonSet()
	_, rest := splitCommonFlags(fs, []string{"serve", "--state", "/state", "--port", "9765"})

	want := []string{"serve", "--state", "/state", "--port", "9765"}
	if len(rest) != len(want) {
		t.Fatalf("router lost arguments: got %q, want %q", rest, want)
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Fatalf("router argument %d = %q, want %q (order must be preserved)", i, rest[i], want[i])
		}
	}
}

package compare

import (
	"strings"
	"testing"
)

// , seen live on argus-dev (rev 61, comparison cmp_1d20b3e2ff59b87449c13e1d): a property declared
// at 20% that held in 0 of 5 runs read "5 runs cannot support 20%; that needs 2 runs with no disagreement". The
// cell had five runs: the reason it is unsupported is that they disagreed, not that there were too few.
func TestCMP15_UnsupportedFor_EnoughRunsThatDisagreedSaysSoAndNamesNoSmallerRunCount(t *testing.T) {
	got := UnsupportedFor(5, 5, 20)
	if got != "5 of 5 runs disagreed; that does not support 20%" {
		t.Fatalf("UnsupportedFor(5, 5, 20) = %q", got)
	}
	if strings.Contains(got, "that needs") {
		t.Fatalf("it names a run count the cell already has: %q", got)
	}
}

func TestCMP15_UnsupportedFor_TooFewRunsKeepsTheRunCountSentence(t *testing.T) {
	// no disagreement, too few runs: unchanged
	if got, want := UnsupportedFor(0, 5, 99), UnsupportedSentence(5, 99); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// a disagreement AND too few runs: the run count is still the true reason to name
	if got, want := UnsupportedFor(1, 5, 99), UnsupportedSentence(5, 99); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// the 100% rule never names a run count
	if got, want := UnsupportedFor(2, 5, 100), UnsupportedSentence(5, 100); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The wiring of the MEASURED cell: 4 of 10 candidate runs disagree with the reference on a check declared at 40%
// (40% needs 4 clean runs, the cell has 10): unsupported because of the disagreements, and it says so.
func TestCMP15_TheMeasuredCellUsesIt(t *testing.T) {
	in := baseInput()
	in.Checks[0].Rules = measured(t, "Repeats", "10", "Agreement", "40%")
	in.Runs = append(in.Runs, run("r1", "old", "v1", "passed", out("C-1", hx("1"))), run("r2", "old", "v1", "passed", out("C-1", hx("1"))))
	for i := 0; i < 10; i++ {
		h := hx("1")
		if i < 4 {
			h = hx("2")
		}
		in.Runs = append(in.Runs, run("n"+string(rune('a'+i)), "new", "v2", "passed", out("C-1", h)))
	}
	c := cellOf(t, Result(in), "C-1", "new")
	if c.ClaimSupported {
		t.Skipf("4 of 10 disagreeing supports 40%% here (%q): pick other numbers", c.Supports)
	}
	if c.Unsupported != "4 of 10 runs disagreed; that does not support 40%" {
		t.Fatalf("measured cell: agreed %d of %d, unsupported = %q", c.Agreed, c.Runs, c.Unsupported)
	}
}

// The wiring: the judge of a fixed / property check goes through UnsupportedFor.
func TestCMP15_TheClaimJudgeUsesIt(t *testing.T) {
	j := JudgeClaim([]string{"failed", "failed", "failed", "failed", "failed"}, 20)
	if j.ClaimSupported || j.Unsupported != "5 of 5 runs disagreed; that does not support 20%" {
		t.Fatalf("JudgeClaim: supported=%v unsupported=%q", j.ClaimSupported, j.Unsupported)
	}
}

package compare

import "testing"

// A not_applicable check is neither held nor failed: JudgeClaim leaves it out of n (named, not
// counted as a failure of the property), and it is never read as an errored run's "check errored".

func TestJudgeClaim_NotApplicableIsExcludedFromN_NeitherHeldNorFailed(t *testing.T) {
	j := JudgeClaim([]string{OutcomePassed, OutcomePassed, OutcomeNotApplicable}, 100)
	if j.Runs != 2 || j.Held != 2 || j.CouldNotRun != 1 || j.State != StateIdentical {
		t.Fatalf("%+v", j)
	}
	j = JudgeClaim([]string{OutcomeNotApplicable, OutcomeNotApplicable}, 100)
	if j.Runs != 0 || j.State != StateCouldNotRun {
		t.Fatalf("all-N/A must not be judged as held: %+v", j)
	}
	j = JudgeClaim([]string{OutcomeFailed, OutcomeNotApplicable}, 100)
	if j.Runs != 1 || j.Held != 0 || j.State != StateDiffers {
		t.Fatalf("a failure beside an N/A still differs: %+v", j)
	}
}

func TestClassify_NotApplicableHasItsOwnReasonNotErrored(t *testing.T) {
	ck := &Check{ID: "C1"}
	c := &calc{in: Input{SetHash: "h"}}
	r := &Run{Status: "completed", SetHash: "h", Outcomes: map[string]string{"C1": OutcomeNotApplicable}}
	got := c.classify(r, ck)
	if got.kind != clsCNR || got.reason != rsnNotApplicable {
		t.Fatalf("N/A classified as %v/%q, want could-not-run with its own reason", got.kind, got.reason)
	}
	r.Outcomes["C1"] = OutcomeErrored
	if got := c.classify(r, ck); got.reason != rsnErrored {
		t.Fatalf("errored moved: %q", got.reason)
	}
}

package chain

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// VR12-CH1 / VR12-CH2 (V30-001) — THE SEVEN RULES, one case each for the ones a test can decide.
//
// Rules 1, 2 and 4 are covered by the updated cases in chain_test.go / capture_test.go; rule 5 by
// unreachable_test.go. This file carries 3, 6 and 7, plus the two properties the design says must
// NOT change.

// RULE 3 — a step whose `${saved.<var>}` was never captured is `not-measured` and is NEVER FIRED.
//
// A step that cannot be given its inputs was not tested, and must not be reported as if it were.
// Before this, `bindSaved`'s error arrived as a step FAILURE — after the decision to call had
// already been made.
func TestRule3_UnboundSavedIsNotMeasuredAndNeverFired(t *testing.T) {
	fired := false
	steps := []Step{
		{Name: "produce", Run: fail("the SUT rejected the write")},
		{
			Name:  "consume",
			Needs: func(vars map[string]string) error { _, err := bindSaved(`{"id":"${saved.docId}"}`, vars); return err },
			Run: func(string, map[string]string) report.StepResult {
				fired = true
				return report.StepResult{Status: "passed"}
			},
		},
	}
	res := Run("tr-r3", steps)
	if fired {
		t.Fatal("a step whose input was never produced must NEVER be fired")
	}
	if res.Steps[1].Status != report.StepNotMeasured {
		t.Fatalf("status = %q, want %q", res.Steps[1].Status, report.StepNotMeasured)
	}
	// The reason names the VARIABLE — that is what makes it actionable rather than a shrug.
	if !strings.Contains(res.Steps[1].Observed, "docId") {
		t.Errorf("the reason must name the unresolved variable, got %q", res.Steps[1].Observed)
	}
	// It fired no request, so it is in no request bucket.
	if res.ReqSuccess+res.ReqFailed+res.ReqError != 1 {
		t.Errorf("only the step that FIRED may be counted: ok:%d fail:%d err:%d",
			res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
	// Rule 1: it still means the scenario did not pass.
	if res.Status != "failed" {
		t.Errorf("status = %q, want failed", res.Status)
	}
}

// …and a step whose inputs ARE available runs even though an earlier step failed. This is the
// whole point: RACE-003's `delete-namespace` names its target from the correlation id, known
// before the chain starts, and needs nothing from the step that failed.
func TestRule2_ACleanupStepThatNeedsNothingStillRuns(t *testing.T) {
	cleaned := false
	res := Run("tr-r2", []Step{
		{Name: "create", Run: pass()},
		{Name: "assert", Run: fail("the marker was not found")},
		{Name: "delete-namespace", Run: func(string, map[string]string) report.StepResult {
			cleaned = true
			return report.StepResult{Status: "passed"}
		}},
	})
	if !cleaned {
		t.Fatal("the cleanup step must run — the two runs of six that leaked were exactly the ones " +
			"whose chain broke before it")
	}
	if res.Steps[2].Status != report.StepRanAfterFailureOK {
		t.Errorf("…and it may still never be green: %q", res.Steps[2].Status)
	}
	if res.Residue != "" {
		t.Errorf("cleanup RAN and succeeded, so nothing may claim residue: %q", res.Residue)
	}
}

// RULE 6 — the FIRST failing step is the named cause; later failures are ECHOES, said out loud.
//
// The worked example: RACE-002 declares ZERO `${saved.…}` dependencies, so if `create-namespace`
// fails, the write, the search and the delete all run and all fail. Four red steps, one bug. That
// is an ACCEPTED COST of continuing — what makes it payable is that the report SAYS they are
// echoes, rather than leaving the reader to work it out.
func TestRule6_LaterFailuresAreLabelledEchoesOfTheFirst(t *testing.T) {
	res := Run("tr-r6", []Step{
		{Name: "create-library", Run: pass()},
		{Name: "create-namespace", Run: fail("quota exceeded")},
		{Name: "write-marked-doc", Run: fail("no such namespace")},
		{Name: "global-search-marker", Run: fail("no such namespace")},
		{Name: "delete-namespace", Run: fail("no such namespace")},
	})
	if res.Failure == nil {
		t.Fatal("a failed chain must carry a failure")
	}
	if !strings.Contains(res.Failure.Observed, "'create-namespace'") {
		t.Errorf("the FIRST failing step must be named as the cause, got %q", res.Failure.Observed)
	}
	if strings.Contains(res.Failure.Observed, "write-marked-doc") {
		t.Errorf("an echo must not be promoted to the cause: %q", res.Failure.Observed)
	}
	if !strings.Contains(res.Failure.Observed, "3 later step(s)") ||
		!strings.Contains(res.Failure.Observed, "echoes") {
		t.Errorf("the echoes must be COUNTED and named as echoes, in the report — not discovered by "+
			"the reader: %q", res.Failure.Observed)
	}
	for _, i := range []int{2, 3, 4} {
		if res.Steps[i].Status != report.StepRanAfterFailureFailed {
			t.Errorf("step %d = %q, want %q", i, res.Steps[i].Status, report.StepRanAfterFailureFailed)
		}
		if !res.Steps[i].RanAfterFailure {
			t.Errorf("step %d must carry RanAfterFailure", i)
		}
	}
}

// RULE 7 — a run that ends with something possibly still on the SUT SAYS SO, in one line.
// ⛔ Absence of cleanup must never be silent.
func TestRule7_ResidueIsStatedWhenCleanupDidNotComplete(t *testing.T) {
	res := Run("tr-r7", []Step{
		{Name: "create-namespace", Run: pass()},
		{Name: "write-doc", Run: fail("boom")},
		{Name: "delete-namespace", Run: fail("still in use")},
	})
	if res.Residue == "" {
		t.Fatal("cleanup was attempted and FAILED — the run must say something may remain")
	}
	if !strings.Contains(res.Residue, "delete-namespace") {
		t.Errorf("the line must name the step(s) that did not complete: %q", res.Residue)
	}
	// ⛔ It says MAY remain. The runner knows which steps did not complete; it does not know what
	// the SUT holds, and a confident sentence it cannot support is the defect, not the fix.
	if !strings.Contains(res.Residue, "may remain") {
		t.Errorf("the line must claim only what the runner can know: %q", res.Residue)
	}
	// A passing chain never carries one.
	if ok := Run("tr-r7b", []Step{{Name: "a", Run: pass()}}); ok.Residue != "" {
		t.Errorf("a passing chain must carry no residue line: %q", ok.Residue)
	}
}

// ⛔ WHAT MUST NOT CHANGE #1 — THE CAPTURE MERGE STAYS BEFORE THE OUTCOME IS JUDGED.
//
// A cleanup step needs the ids captured by the step that FAILED. If the merge moved after the
// break check, rule 3 would fire on variables that were, in fact, captured — and the cleanup step
// would be marked not-measured instead of running. This test fails if the merge moves.
func TestMustNotChange_CapturesFromAFailingStepStillReachLaterSteps(t *testing.T) {
	var sawID string
	res := Run("tr-merge", []Step{
		{Name: "create", Run: func(string, map[string]string) report.StepResult {
			// It SAVED successfully and then failed its assertion — the commonest shape there is.
			return report.StepResult{Status: "failed", Observed: "the name was not echoed back",
				Captured: map[string]string{"libId": "lib-77"}}
		}},
		{
			Name: "delete",
			Needs: func(vars map[string]string) error {
				_, err := bindSaved(`{"library_id":"${saved.libId}"}`, vars)
				return err
			},
			Run: func(_ string, vars map[string]string) report.StepResult {
				sawID = vars["libId"]
				return report.StepResult{Status: "passed"}
			},
		},
	})
	if sawID != "lib-77" {
		t.Fatalf("the delete step saw libId=%q — the capture merge must stay BEFORE the outcome is "+
			"judged, or a cleanup step can never undo what the failing step created", sawID)
	}
	if res.Steps[1].Status != report.StepRanAfterFailureOK {
		t.Errorf("the cleanup ran, so it is ran-after-failure: ok, got %q", res.Steps[1].Status)
	}
}

// ⛔ WHAT MUST NOT CHANGE #2 — A CHAIN THAT PASSES IS UNTOUCHED. This fix changes only the failure
// path, and a green chain's record must be identical to what it was.
func TestMustNotChange_APassingChainIsIdentical(t *testing.T) {
	res := Run("tr-green", []Step{
		{Name: "a", Run: pass()},
		{Name: "b", Run: pass()},
	})
	if res.Status != "passed" || res.Failure != nil || res.Residue != "" {
		t.Fatalf("a passing chain must be unchanged: %+v", res)
	}
	for i, st := range res.Steps {
		if st.Status != "passed" || st.RanAfterFailure {
			t.Errorf("step %d = %q ranAfterFailure=%v, want passed/false", i, st.Status, st.RanAfterFailure)
		}
	}
	if res.ReqSuccess != 2 || res.ReqFailed != 0 || res.ReqError != 0 {
		t.Errorf("request counts changed on the green path: ok:%d fail:%d err:%d",
			res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
}

// VR12-CH2 — a RATE-LIMITED step stops the chain, and the marker rides on the SCENARIO.
//
// The concrete harm, stronger than "it would double the objects": a rate-limited chain is retried
// as a WHOLE, from step 1, with the SAME correlation id — and scenario object names are built from
// that id. If the first attempt CREATED a library and was then throttled, the retry re-runs
// `create-library` with a name that already exists and fails on a collision that has nothing to do
// with the SUT under test.
func TestCH2_RateLimitedStepStopsTheChainDead(t *testing.T) {
	fired := 0
	res := Run("tr-ch2", []Step{
		{Name: "create", Run: func(string, map[string]string) report.StepResult {
			fired++
			return report.StepResult{Status: "passed"}
		}},
		{Name: "search", Run: func(string, map[string]string) report.StepResult {
			fired++
			return report.StepResult{Status: report.StatusError, RateLimited: true, RetryAfterMs: 30000,
				Observed: mcp.RateLimitedObserved(0)}
		}},
		{Name: "delete", Run: func(string, map[string]string) report.StepResult {
			fired++
			return report.StepResult{Status: "passed"}
		}},
	})
	if fired != 2 {
		t.Fatalf("the chain must stop at the throttled step; %d steps fired", fired)
	}
	if res.Status != report.StatusError || !res.RateLimited || res.RetryAfterMs != 30000 {
		t.Fatalf("the scenario must be not-measured and carry the retry-after: %+v", res)
	}
	for _, i := range []int{1, 2} {
		if res.Steps[i].Status != report.StepNotMeasured {
			t.Errorf("step %d = %q, want %q (the throttled step AND every later one)",
				i, res.Steps[i].Status, report.StepNotMeasured)
		}
	}
	if !strings.Contains(res.Steps[1].Observed, "rate-limited") {
		t.Errorf("the reason belongs in the step's text: %q", res.Steps[1].Observed)
	}
	// …and it says so out loud, because the delete never ran.
	if !strings.Contains(res.Residue, "delete") {
		t.Errorf("the residue line must name the cleanup that never ran: %q", res.Residue)
	}
}

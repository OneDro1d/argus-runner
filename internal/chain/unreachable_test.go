package chain

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

func errStep(msg string) func(string, map[string]string) report.StepResult {
	return func(string, map[string]string) report.StepResult {
		return report.StepResult{Status: report.StatusError, Observed: msg}
	}
}

// A chain that broke on its FIRST step because nothing answered is an UNRUN chain, not a failed one.
// The rollup used to flatten a StatusError step into a plain "failed" scenario, so an absent SUT
// arrived looking like a product defect.
func TestRun_UnreachableFirstStepIsErrored(t *testing.T) {
	res := Run("tr-1", []Step{
		{Name: "mcp", Run: errStep("unreachable — connection refused")},
		{Name: "db", Run: pass()},
	})
	if res.Status != report.StatusError {
		t.Fatalf("status = %q, want %q — nothing was reached, so nothing failed", res.Status, report.StatusError)
	}
	if res.ReqError != 1 || res.ReqSuccess != 0 || res.ReqFailed != 0 {
		t.Fatalf("request counts = ok:%d fail:%d err:%d, want 0/0/1", res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
}

// ⛔ SUPERSEDED BY VR12-CH2 (V30-001). This case used to assert `failed` on the reasoning that
// "once ANY step got a real response, the chain genuinely failed". That reasoning does not hold:
// a chain whose LAST step could not be answered has not been measured, and the owner's rule is
// explicit — *"'server is unreachable' is not a failure, it means we can't run tests."*
//
// The request accounting is untouched: one success, one error.
func TestRun_ReachedThenUnreachableIsNotMeasured(t *testing.T) {
	res := Run("tr-2", []Step{
		{Name: "mcp", Run: pass()},
		{Name: "db", Run: errStep("connection refused")},
	})
	if res.Status != report.StatusError {
		t.Fatalf("status = %q, want %q — the SUT would not answer, so the scenario was not measured",
			res.Status, report.StatusError)
	}
	if res.ReqSuccess != 1 || res.ReqError != 1 {
		t.Fatalf("request counts = ok:%d fail:%d err:%d, want 1 success and 1 error",
			res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
}

// ⛔ …BUT "NOT MEASURED" NEVER OVERRIDES "MEASURED AND WRONG". A real failure followed by an
// unreachable step stays `failed`: re-labelling it `error` would bury a genuine finding behind an
// infrastructure excuse, which is the same mis-attribution this round exists to remove.
func TestRun_FailedThenUnreachableStaysFailed(t *testing.T) {
	res := Run("tr-3", []Step{
		{Name: "mcp", Run: fail("0 rows for the correlation id")},
		{Name: "db", Run: errStep("connection refused")},
	})
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed — a measured defect is not erased by a later outage", res.Status)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "'mcp'") {
		t.Fatalf("the FIRST failing step stays the named cause: %+v", res.Failure)
	}
}

// A plain assertion failure is untouched: the SUT answered, the answer was wrong.
func TestRun_OrdinaryFailureIsStillFailed(t *testing.T) {
	res := Run("tr-3", []Step{{Name: "db", Run: fail("0 rows for the correlation id")}})
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed", res.Status)
	}
}

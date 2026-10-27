package argus

import (
	"sync"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// countScenario adds ONE finished check to a Summary: the status-to-count mapping, in the one place it
// lives. summarize (the final counts, after the loop) and Tally (the counts so far, during it) both call
// it, so a mid-run count and the end-of-run count can never disagree about what a status means.
//
// It counts Total, the status bucket and RateLimitedScenarios. It does not touch RateLimitPauses (the
// pacer stamps that once, at the end), Faults or anything per layer: those stay summarize's own.
func countScenario(sum *report.Summary, s report.ScenarioResult) {
	sum.Total++
	switch s.Status {
	case "passed":
		sum.Passed++
	case "failed":
		sum.Failed++
	case report.StatusError:
		sum.Errored++ // execution/harness failure — non-green, DISTINCT from failed (UC-62)
		if s.RateLimited {
			// VR10-R1: "rate-limited" is a REASON INSIDE errored (owner D4: no fourth
			// status), so it is counted here and never in Passed/Failed — a pass rate is
			// over MEASURED scenarios only.
			sum.RateLimitedScenarios++
		}
	case report.StatusNotApplicable:
		// NOT EXECUTED because a precondition it depends on is absent. Counted on its own -
		// never in Passed (nothing was verified), Failed (the system was not shown wrong) or Errored (the
		// instrument is fine: it measured that the mechanism is absent).
		sum.NotApplicable++
	case report.StatusDegraded:
		// AC-11: DEGRADED is counted on its own — never in Passed (its assertions did not
		// simply pass unremarked) and never in Failed/Errored (nothing was wrong or
		// unmeasured). A pass rate stays a pass rate; a DEGRADED run is visible separately.
		sum.Degraded++
	default:
		sum.Skipped++
	}
}

// Tally is the run's counts so far, for the executor's mid-run heartbeat (a tester watching a run of many
// checks could not tell a run going well from one failing every check until it ended).
//
// COUNTS ONLY. It keeps no id, no outcome per check and no summary, so nothing that reads it can leak
// which check did what: the control plane stamps scenarios.last_run_outcome for every entry of a push's
// scenario list, and a heartbeat must never carry one.
//
// One writer (the run loop) and one reader (the heartbeat goroutine), so it is mutex-guarded. A nil
// *Tally is valid and does nothing, which is what keeps every caller that passes none exactly as it was.
// It is a value the caller creates and hands in, never package state: two runs in one process (a test,
// or a future executor with two slots) cannot see each other's counts.
type Tally struct {
	mu      sync.Mutex
	planned int
	sum     report.Summary
}

// TallySnapshot is one consistent reading of a Tally. Passed + Failed + Errored + Degraded + NotApplicable +
// (skipped, which the wire has no field for) always equals Total, mid-run as at the end.
type TallySnapshot struct {
	Planned       int
	Total         int
	Passed        int
	Failed        int
	Errored       int
	Degraded      int
	NotApplicable int
}

// NewTally returns an empty Tally.
func NewTally() *Tally { return &Tally{} }

// SetPlanned records how many checks the run selected. Called once, before the first check fires.
func (t *Tally) SetPlanned(n int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.planned = n
	t.mu.Unlock()
}

// Finished counts one check whose row is final for the run loop. Called exactly once per check, with the
// row that is reported (a check re-fired after a rate-limit pause is counted once, with the re-fire's row).
func (t *Tally) Finished(s report.ScenarioResult) {
	if t == nil {
		return
	}
	t.mu.Lock()
	countScenario(&t.sum, s)
	t.mu.Unlock()
}

// Snapshot reads the counts so far. A nil Tally reads as all zeros.
func (t *Tally) Snapshot() TallySnapshot {
	if t == nil {
		return TallySnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return TallySnapshot{
		Planned: t.planned, Total: t.sum.Total, Passed: t.sum.Passed, Failed: t.sum.Failed,
		Errored: t.sum.Errored, Degraded: t.sum.Degraded, NotApplicable: t.sum.NotApplicable,
	}
}

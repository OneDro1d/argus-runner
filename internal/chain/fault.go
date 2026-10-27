package chain

import (
	"context"
	"time"

	"github.com/OneDro1d/argus-runner/internal/fault"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// FaultApplier is what a fault step needs of internal/fault's Manager (an interface so the chain package
// can be tested without a cluster).
type FaultApplier interface {
	Apply(ctx context.Context, sp fault.Spec) error
}

// FaultStep builds the `fault` chain step. It passes when the fault was applied and
// fails, by name, when it was refused or could not be applied; it makes no claim of its own.
//
// ⛔ It is NOT where faults are undone. The end-of-chain restore belongs to the caller that owns the
// Manager (internal/argus), because it has to run on paths this step never sees: a failed chain, a
// timed-out one, an abandoned step, a panic. `always` is honoured like any step's (a `restore` step that
// must still fire after the SUT stopped answering).
func FaultStep(name string, ap FaultApplier, sp fault.Spec, always bool) Step {
	run := func(deadline time.Time) report.StepResult {
		ctx, cancel := context.Background(), func() {}
		if !deadline.IsZero() {
			ctx, cancel = context.WithDeadline(ctx, deadline.Add(stepGrace))
		}
		defer cancel()
		if err := ap.Apply(ctx, sp); err != nil {
			return report.StepResult{Status: "failed", Observed: err.Error()}
		}
		return report.StepResult{Status: "passed", Observed: faultObserved(sp)}
	}
	return Step{Name: name, Always: always,
		Run: func(string, map[string]string) report.StepResult { return run(time.Time{}) },
		RunUntil: func(_ string, _ map[string]string, deadline time.Time) report.StepResult {
			return run(deadline)
		}}
}

// faultObserved says what was done, in allowlist names only (never an address).
func faultObserved(sp fault.Spec) string {
	switch sp.Op {
	case fault.OpRestore:
		return "every fault this chain applied was undone"
	case fault.OpWaitReady:
		return "deployment " + sp.Deployment + " is ready"
	case fault.OpStop, fault.OpStart, fault.OpScale:
		return "applied " + sp.Op + " to deployment " + sp.Deployment
	}
	return "applied " + sp.Op + " to link " + sp.Link
}

package runner

import (
	"context"

	"github.com/OneDro1d/argus-runner/internal/fault"
)

// replayFaults runs every pending fault undo left by a dead executor. It is called by Loop
// before the first poll, so the system under test is whole before this executor is handed new work.
//
// It never stops the executor: a failed undo is logged LOUDLY (fault.Replay) and kept in the journal for
// the next start, because an executor that refuses to start over a stuck undo would leave the SUT broken
// AND unmonitored. The outcome of every entry is also appended to <journal>/replay.log.
func (e *Executor) replayFaults(ctx context.Context) {
	if e.FaultJournalDir == "" {
		return
	}
	kube, toxi := e.FaultKube, e.FaultToxi
	if kube == nil {
		kube = fault.DefaultKube
	}
	if toxi == nil {
		toxi = fault.DefaultToxi
	}
	fault.Replay(ctx, fault.ReplayConfig{
		Journal: &fault.Journal{Dir: e.FaultJournalDir}, Owner: fault.Hostname(),
		Kube: kube, Toxi: toxi, Logf: e.log,
	})
}

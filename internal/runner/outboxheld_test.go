package runner

import (
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// VR-F28 / INT-004 — the executor can NAME what its outbox is holding, and says so on the POLL.
//
// Pending() answered "how many", which was enough for a log line and not enough for the ledger: the
// control plane cannot mark runs it cannot name. Held() is the same queue, described.

func TestOutbox_HeldNamesEveryQueuedRunWithItsTallies(t *testing.T) {
	o := &Outbox{Dir: t.TempDir(), Log: func(string, ...any) {}}

	if got := o.Held(); got != nil {
		t.Errorf("an empty outbox reported %+v, want nil — most polls carry nothing", got)
	}

	for _, p := range []federation.ResultsPush{
		{RunID: "run-a", Scope: "full", Status: "completed", Tallies: federation.Tallies{Total: 33, Passed: 30, Failed: 3}},
		{RunID: "run-b", Scope: "layer", Status: "failed", Tallies: federation.Tallies{Total: 4, Failed: 4}},
	} {
		if err := o.Put(p); err != nil {
			t.Fatalf("put %s: %v", p.RunID, err)
		}
	}

	held := o.Held()
	if len(held) != 2 {
		t.Fatalf("Held() = %d entries, want 2 (Pending() says %d)", len(held), o.Pending())
	}
	byID := map[string]federation.PendingRun{}
	for _, h := range held {
		byID[h.RunID] = h
	}
	a, ok := byID["run-a"]
	if !ok {
		t.Fatalf("run-a is missing from %+v", held)
	}
	// The TALLIES are the point. "pending, 30/33" is actionable; a bare "pending" is not, and
	// re-deriving the counts would cost the payload that could not be delivered in the first place.
	if a.Tallies.Total != 33 || a.Tallies.Passed != 30 || a.Tallies.Failed != 3 {
		t.Errorf("run-a tallies = %+v, want 33/30/3", a.Tallies)
	}
	if a.Scope != "full" {
		t.Errorf("run-a scope = %q, want full", a.Scope)
	}
	if a.QueuedAt.IsZero() {
		t.Error("run-a has no QueuedAt — without it a surface cannot tell a restart blip from an " +
			"executor that has not reached the control plane since Tuesday")
	}

	// After an Ack the entry is gone from the report, or the CP would be told a delivered run is
	// still pending.
	if err := o.Ack("run-a"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	held = o.Held()
	if len(held) != 1 || held[0].RunID != "run-b" {
		t.Errorf("after Ack(run-a), Held() = %+v, want only run-b", held)
	}
}

// A disabled outbox reports nothing rather than panicking — compose instances without a durable
// results volume take this path on every poll.
func TestOutbox_HeldIsSafeWhenDisabled(t *testing.T) {
	var o *Outbox
	if got := o.Held(); got != nil {
		t.Errorf("nil outbox reported %+v", got)
	}
	if got := (&Outbox{}).Held(); got != nil {
		t.Errorf("unconfigured outbox reported %+v", got)
	}
}

// THE CLASS GUARD (INT-029). The lesson was: a fact only the executor knows, carried on REGISTER,
// reaches the control plane exactly once and never again — and nothing ENFORCED the lesson, so the
// next such field walked straight into it. This is the next such field.
//
// It is worse than the register case, and that is the reason this assertion exists: "which runs are
// finished but undelivered" is true precisely WHEN the push path is failing. Carried on the push, the
// one condition it reports would be the one condition under which it can never be reported.
func TestPollRequest_CarriesTheHeldRuns(t *testing.T) {
	queued := time.Unix(1_700_000_000, 0).UTC()
	pending := []federation.PendingRun{{
		RunID: "run-x", Scope: "full", Attempts: 2, QueuedAt: queued,
		Tallies: federation.Tallies{Total: 10, Passed: 9, Failed: 1},
	}}
	req := pollRequestFor("0.3.0+m3-fx", nil, nil, nil, nil, pending, federation.SUTObservation{}, nil, nil)
	if len(req.ResultsPending) != 1 {
		t.Fatalf("the poll payload carries %d pending run(s), want 1.\n"+
			"  This is the INT-029 class: an executor-only fact that is not on the poll cannot reach\n"+
			"  the control plane at all.", len(req.ResultsPending))
	}
	got := req.ResultsPending[0]
	if got.RunID != "run-x" || got.Tallies.Passed != 9 || got.Attempts != 2 || !got.QueuedAt.Equal(queued) {
		t.Errorf("pending run reached the payload as %+v, want the fields intact", got)
	}
	// Nothing when there is nothing — omitempty keeps the common poll body unchanged.
	if got := pollRequestFor("0.3.0+m3-fx", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil).ResultsPending; got != nil {
		t.Errorf("an empty queue put %+v on the wire", got)
	}
}

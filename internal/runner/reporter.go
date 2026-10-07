package runner

// reporter.go — the "ALL RUNS REPORT UP" seam for a DIRECT in-env run (M3 fix plan D3 / R10 / UC062).
// The MCP runner__run in the serve process runs the on-disk scenarios (toolcore.Run) and, when the serve
// process is CP-wired, calls Client.ReportUp to push the SAME evidence-free ResultsPush the executor's
// pickup loop pushes — under the run's OWN run_id with NO run_request_id (a direct run is not enqueued).
// The evidence (Expected/Observed, sagas, logs) is DROPPED at the mapReport seam; only tallies + generic
// per-scenario outcomes cross.
//
// F1 (CP-M3-III-62): the sentence that used to end this comment — "a failed push (e.g. the CP
// momentarily down) loses nothing and lands on the next reachable CP" — was FALSE, in the same way
// executor.go's and direct.go's were. Nothing landed it. The Stage III live gate caught it: the
// cloud-requested path recovered its results after an outage and the MCP runner__run path did NOT,
// because this is a THIRD terminal-push seam and only the first two had been given the outbox.
// It now queues durably before pushing, so the sentence is finally true.

import (
	"context"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// ReportUp maps an in-env run report to the evidence-free ResultsPush and pushes it under runID with an
// EMPTY run_request_id (direct run). scope is single|tag|layer|full; setHash/deepLink/ann ride the ledger
// row as metadata (never evidence). Returns the push error so the caller can log it (best-effort).
// ⛔ VR9-T1 — startedAt/finishedAt ARE PARAMETERS, and they have to be. ReportUp receives an ALREADY
// FINISHED report: the run happened in its CALLER, so there is no moment here at which "the run began"
// could be observed. An earlier draft of the requirement said each site "must capture startedAt before
// the run begins", which is impossible at this one — hence the signature change and both callers.
func (c *Client) ReportUp(ctx context.Context, rep *report.Report, runID, scope, setHash, deepLink string, ann federation.Annotations, startedAt, finishedAt time.Time) error {
	// Direct/local runs never carry an artifact digest (they are build-mode by construction; a
	// `final` certification always goes through the federated assignment path in execute.go).
	push := mapReport(rep, runID, "", scope, setHash, deepLink, ann, startedAt, finishedAt, "")
	// Durable BEFORE the network call — the same ordering the executor's terminal push uses, and for
	// the same reason. With Outbox nil this is a no-op and the behaviour is exactly as it was.
	_ = c.Outbox.Put(push)
	if err := c.Push(ctx, push); err != nil {
		return err // stays queued; the executor's poll loop drains it from the shared results volume
	}
	return c.Outbox.Ack(push.RunID)
}

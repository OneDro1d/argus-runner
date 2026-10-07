package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// mapReport projects an in-env run report onto the federation ResultsPush — the ONLY data that crosses
// to the cloud (D-FED.4 / D-CP-WEB.1 MAY column). It carries tallies + per-scenario OUTCOME + a
// GENERIC summary line, and NOTHING else: the report's Failure detail (Expected/Observed), steps, MCP
// envelopes, sagas and logs are DROPPED here — the locality rule enforced at the seam, reinforcing the
// schema (which has no field for them anyway). runID + runRequestID + scope + setHash come from the
// run's own context (the executor generates runID; runRequestID/scope/setHash ride the assignment).
// The run annotations (commit/PR/label — metadata, not evidence) are ECHOED so they land on the cloud
// ledger row and read back via get_run_status + the web run detail (UC040 / D-CP-WEB.1 MAY column).
// VR9-T1 — startedAt/finishedAt are the EXECUTOR'S OWN WALL CLOCK, passed in rather than derived.
//
// ⛔ NOT Σ sc.DurationMs. The per-scenario durations are right here (see the loop below), which makes
// the sum a one-line change producing plausible, differing, non-zero values — and it omits
// materialisation, setup and teardown, so it under-reports a run that spent its time waiting.
//
// ⛔ AND NOT THE CONTROL PLANE'S RECEIPT TIME. A CP-side timestamp is indistinguishable from an honest
// one on a fast run and 30s wrong the moment a push is delayed.
//
// Measured before this existed: 16 runs, duration_ms = 0 on all 16, on a schema that had carried the
// column all along.
func mapReport(rep *report.Report, runID, runRequestID, scope, setHash, deepLink string, ann federation.Annotations, startedAt, finishedAt time.Time, artifactDigest string) federation.ResultsPush {
	push := federation.ResultsPush{
		RunID:          runID,
		RunRequestID:   runRequestID,
		Scope:          scope,
		SetHash:        setHash,
		DeepLink:       deepLink,
		Annotations:    ann,
		ArtifactDigest: artifactDigest,
		Tallies: federation.Tallies{
			Passed:  rep.Summary.Passed,
			Failed:  rep.Summary.Failed,
			Errored: rep.Summary.Errored,
			Total:   rep.Summary.Total,
			// VR10-R1: carried up so the Runs page can say what was NOT MEASURED. Without this the
			// operator reads "N errored" about a run where the SUT only asked us to slow down.
			RateLimited:     rep.Summary.RateLimitedScenarios,
			RateLimitPauses: rep.Summary.RateLimitPauses,
			// AC-11: carried up so the Runs page can say how many scenarios passed UNDER DISTRESS —
			// never folded into Passed or Failed, same reasoning as RateLimited.
			Degraded: rep.Summary.Degraded,
		},
		Status: "completed",
	}
	// VR9-T1. Pointers on the wire so "not reported" stays representable — an older executor omits
	// them rather than claiming a zero.
	if !startedAt.IsZero() {
		st := startedAt
		push.StartedAt = &st
	}
	if !finishedAt.IsZero() {
		fin := finishedAt
		push.FinishedAt = &fin
	}
	// ⚠ NEVER NEGATIVE. A clock that steps backwards mid-run is rare and real; a negative elapsed time
	// would be stored and rendered as one.
	if ms := finishedAt.Sub(startedAt).Milliseconds(); ms > 0 {
		push.DurationMs = ms
	}
	if rep.Failed() {
		push.Status = "failed"
	}
	// A run that executed ZERO scenarios is a vacuous non-run and must never be a green "completed"
	// ledger row (the ORDE-006 false-green the owner reproduced). Use "failed" (NOT "errored": the
	// run_ledger CHECK (012) is ('running','completed','failed','results_pending','abandoned') — it
	// does not list "errored" at all, so pushing it would violate the constraint and DROP the whole
	// push, not just this one row) — a valid, non-green, terminal status the web renders red.
	//
	// OWNER RULING 2026-07-22: this now applies at EVERY scope, including "full". The earlier
	// carve-out let a full-scope run against an empty instance stay green; the ruling is that if
	// there is nothing to run, the run cannot happen at all — so a 0-total row reaching here means
	// something went wrong upstream, and it must be visible. Runs are refused before execution
	// (toolcore.resolveScenarioDir) and before enqueue (control.requestRun); this is the last
	// line of defense for the direct path and for an archive-after-enqueue race.
	if rep.Summary.Total == 0 {
		push.Status = "failed"
	}
	// AC-11 (F7 — the same failure mode this whole block guards against): a load-mode scenario can
	// pass every assertion while the survival plane shows the SUT in distress. That is its OWN
	// status, "degraded" — migration 038 EXTENDS the run_ledger CHECK above to admit it, precisely
	// so setting it here does not repeat the "unrecognized status drops the whole push" bug. It
	// never overrides "failed": a real failure is always the worse word, decided above and first.
	if push.Status != "failed" && rep.Summary.Degraded > 0 {
		push.Status = "degraded"
	}
	// AC-6/VR-4: the run's VERDICT (passed|failed|degraded), distinct from Status above (the push's
	// own delivery/lifecycle state — completed|failed|degraded|running|results_pending). Empty until
	// the run is terminal: there is nothing to bind a digest to before then. A degraded run's verdict
	// is "degraded", never "passed": the assertions held, but a verdict that a third party later
	// checks must carry the distress the run saw (migration 038 admits it on run_ledger.outcome).
	switch push.Status {
	case "completed":
		push.Outcome = "passed"
	case "failed":
		push.Outcome = "failed"
	case "degraded":
		push.Outcome = "degraded"
	}
	for _, layer := range rep.Layers {
		for _, sc := range layer.Scenarios {
			push.Scenarios = append(push.Scenarios, federation.ScenarioResult{
				ID:      sc.ID,
				Outcome: outcome(sc.Status),
				// F2/UC029: the enclosing group already IS the layer. Dropping it here is what made a
				// run row depend on the live catalog to stay readable.
				Layer:      layer.Layer,
				DurationMs: int64(sc.DurationMs),
				Summary:    genericSummary(sc.Status), // NO Expected/Observed — evidence stays in-env
				// AC-6/VR-4: sha256 of this scenario's own in-env evidence record — see
				// evidenceHash's doc for exactly what is hashed and why.
				EvidenceHash: evidenceHash(sc),
			})
		}
	}
	push.EvidenceBundleHash = evidenceBundleHash(push.Scenarios)
	// the AMQP Load per-step measurements ride along as their own field (nil, and so
	// absent from the wire, on every run without one).
	push.LoadRamp = loadRampWire(rep)
	return push
}

// evidenceHash is AC-6 item 1's per-scenario digest: sha256, hex-encoded, over a canonical JSON
// encoding of sc — the runner's OWN in-env evidence record for this scenario (Failure/Expected-
// Observed, Steps, Cleanup, Residue, MCPEnvelope, Unexecuted, the request counters — everything
// mapReport itself deliberately drops above). encoding/json is used AS the canonicalization: Go
// marshals a struct's fields in their declared order, never alphabetized or randomized, so the same
// report.ScenarioResult value always produces the same bytes — which is what the idempotent-retry
// guarantee (AC-6 item 3) rests on. sc.Requests is excluded because report.ScenarioResult itself
// excludes it from the persisted record (`json:"-"`, runtime-only, not part of the report contract).
//
// The evidence bytes themselves are never returned and never cross the wire — only this digest does
// (D-FED.4 locality rule; see federation.ScenarioResult.EvidenceHash).
func evidenceHash(sc report.ScenarioResult) string {
	b, err := json.Marshal(sc)
	if err != nil {
		// json.Marshal on this struct cannot fail (no channels/funcs/cyclic maps); a paranoia guard
		// that still hashes something deterministic rather than silently emitting an empty evidence
		// hash if that ever changes.
		b = []byte(sc.ID)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// evidenceBundleHash is AC-6 item 2's run-level binding: sha256, hex-encoded, over the per-scenario
// EvidenceHash values in ASCENDING SCENARIO-ID order — never in report/layer order, which depends on
// catalog grouping and would make the same evidence set hash differently across two runs that
// executed it in a different order.
func evidenceBundleHash(scenarios []federation.ScenarioResult) string {
	ids := make([]string, 0, len(scenarios))
	byID := make(map[string]string, len(scenarios))
	for _, sc := range scenarios {
		ids = append(ids, sc.ID)
		byID[sc.ID] = sc.EvidenceHash
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(byID[id]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// outcome maps a report status (passed|failed|skipped|error|degraded) to a push outcome
// (passed|failed|errored|degraded). AC-11: "degraded" is EXTENDED here, never folded into
// "errored" — that is the whole point of the fourth status: a survival-plane signal is not the
// same fact as "nothing could be judged".
func outcome(status string) string {
	switch status {
	case "passed":
		return "passed"
	case "failed":
		return "failed"
	case report.StatusDegraded:
		return report.StatusDegraded
	default: // error, skipped, or anything unexpected → errored (never silently "passed")
		return "errored"
	}
}

// genericSummary is a redaction-safe, scenario-agnostic line (never the expected/observed values).
func genericSummary(status string) string {
	switch status {
	case "passed":
		return ""
	case "failed":
		return "one or more assertions failed (see in-env report/sagas)"
	case "skipped":
		return "not executed"
	case report.StatusDegraded:
		return "assertions passed; the SUT showed distress during the run (see in-env report)"
	default:
		return "execution error (see in-env report)"
	}
}

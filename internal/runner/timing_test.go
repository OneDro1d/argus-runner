package runner

import (
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// VR9-T1 — EVERY RUN HAS AN ELAPSED TIME, AND THE NUMBER REFLECTS THE RUN.
//
// ── WHAT WAS MEASURED ─────────────────────────────────────────────────────────────────────────────
//
// mapReport set tallies, status and scenarios and NO run-level timing at all. 16 runs, duration_ms = 0
// on all 16; the 7 re-measured runs unchanged. The columns already existed in run_ledger and in the
// wire type — nothing was ever put in them.
//
// ⛔ WALL CLOCK, NOT Σ sc.DurationMs. mapReport already carries per-scenario DurationMs, which makes
// the sum a ONE-LINE change producing plausible, differing, non-zero values — and it is wrong: it omits
// materialisation, setup and teardown, and under-reports a run that spent its time waiting. That is
// why the executor's own t0/t1 are passed IN rather than derived here.
//
// ⛔ AND IT IS THE EXECUTOR'S CLOCK, NOT THE CONTROL PLANE'S RECEIPT TIME. A CP-side timestamp is
// indistinguishable from an honest one on a fast run and 30 s wrong when a push is delayed — which is
// exactly what the PO's acceptance 4 exists to separate.
func TestMapReport_CarriesTheRunsWallClock(t *testing.T) {
	t0 := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(207500 * time.Millisecond) // 207.5s — the orderservice end of the measured spread

	rep := &report.Report{}
	rep.Summary.Passed, rep.Summary.Total = 3, 3

	got := mapReport(rep, "run-1", "rr-1", "full", "hash", "https://g", federation.Annotations{}, t0, t1, "")

	t.Run("it carries a duration, a start and a finish", func(t *testing.T) {
		if got.DurationMs == 0 {
			t.Fatal("duration_ms is 0 — which is what 16 measured runs reported, on a schema that has " +
				"had the column all along")
		}
		if got.DurationMs != 207500 {
			t.Errorf("duration_ms = %d, want 207500 (the wall clock between t0 and t1)", got.DurationMs)
		}
		// ⚠ POINTERS on the wire, so "not reported" stays representable — the same
		// absence-is-not-zero discipline this round applies to the holder counts.
		if got.FinishedAt == nil || !got.FinishedAt.Equal(t1) {
			t.Errorf("finished_at = %v, want %v", got.FinishedAt, t1)
		}
		if got.StartedAt == nil || !got.StartedAt.Equal(t0) {
			t.Errorf("started_at = %v, want %v — the EXECUTOR's start, which is the only clock that "+
				"knows when the work began", got.StartedAt, t0)
		}
	})

	// ⛔ THE ONE-LINE CHEAT, MADE TO FAIL. A materialisation-heavy run: the scenarios sum to well under
	// 30% of the wall clock, so Σ sc.DurationMs and the truth are far apart. The PO added this case
	// precisely to remove the builder's choice of a SUT where the two would look alike.
	t.Run("a materialisation-heavy run reports the WALL CLOCK, not the scenario sum", func(t *testing.T) {
		heavy := &report.Report{}
		heavy.Summary.Passed, heavy.Summary.Total = 2, 2
		heavy.Layers = []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{
			{ID: "A", Status: "passed", DurationMs: 900},
			{ID: "B", Status: "passed", DurationMs: 1100},
		}}}
		start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
		end := start.Add(60 * time.Second) // 60s wall clock, 2s of scenarios

		push := mapReport(heavy, "run-2", "", "full", "h", "", federation.Annotations{}, start, end, "")
		var sum int64
		for _, sc := range push.Scenarios {
			sum += sc.DurationMs
		}
		if push.DurationMs == sum {
			t.Fatalf("duration_ms (%d) equals the SUM of scenario durations (%d). That omits "+
				"materialisation, setup and teardown, and under-reports a run that spent its time "+
				"waiting — here by 30x.", push.DurationMs, sum)
		}
		if push.DurationMs != 60000 {
			t.Fatalf("duration_ms = %d, want 60000 (the wall clock)", push.DurationMs)
		}
	})

	// A zero-length window is a real possibility on a fast machine and must not become a negative or a
	// nonsense number.
	t.Run("a zero or reversed window never yields a negative duration", func(t *testing.T) {
		same := mapReport(rep, "r", "", "full", "", "", federation.Annotations{}, t0, t0, "")
		if same.DurationMs < 0 {
			t.Errorf("duration_ms = %d for a zero window", same.DurationMs)
		}
		rev := mapReport(rep, "r", "", "full", "", "", federation.Annotations{}, t1, t0, "")
		if rev.DurationMs < 0 {
			t.Errorf("duration_ms = %d for a reversed window — a clock step backwards must not produce "+
				"a negative elapsed time", rev.DurationMs)
		}
	})
}

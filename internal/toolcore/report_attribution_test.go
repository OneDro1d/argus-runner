package toolcore

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// RO-06: get_report({run_id}) must be RUN-ATTRIBUTED — it returns YOUR run's report
// from its per-run file (results/<instance>/runs/<run_id>.json), even when the shared
// "latest" report.json belongs to a DIFFERENT (concurrent) run. Decision: optional-but-
// stamped — per-run files + a latest copy; FR-3's {instance_id}-only form is unchanged.
func TestGetReport_PerRunFileWinsOverLatestRace(t *testing.T) {
	dir := t.TempDir()
	e := Env{Instance: "local", ResultsRoot: dir}

	// The race: the shared latest report.json is some OTHER run that finished last.
	if err := writeReport(e.reportPath(), &report.Report{RunID: "run-OTHER", Summary: report.Summary{Total: 1, Passed: 1}}); err != nil {
		t.Fatal(err)
	}
	// MY run wrote its own per-run file.
	if err := writeReport(runReportPath(e.resultsDir(), "run-MINE"), &report.Report{RunID: "run-MINE", Summary: report.Summary{Total: 1, Failed: 1}}); err != nil {
		t.Fatal(err)
	}

	got, _, err := GetReport(e, role.Test, "run-MINE")
	if err != nil {
		t.Fatal(err)
	}
	rep, ok := got.(*report.Report)
	if !ok {
		t.Fatalf("get_report({run-MINE}) returned %T, not the per-run report (the latest-race shadowed it)", got)
	}
	if rep.RunID != "run-MINE" {
		t.Fatalf("got run %q, want run-MINE (per-run file must win over the report.json race)", rep.RunID)
	}
	if rep.Summary.Failed != 1 {
		t.Errorf("returned the wrong run's summary: %+v", rep.Summary)
	}
}

// FR-3 preserved: get_report({instance_id}) with no run_id returns the latest report,
// always carrying its own run_id so a caller can detect a mismatch (never silently cross-attributed).
func TestGetReport_NoRunID_ReturnsLatestStamped(t *testing.T) {
	dir := t.TempDir()
	e := Env{Instance: "local", ResultsRoot: dir}
	if err := writeReport(e.reportPath(), &report.Report{RunID: "run-LATEST", Summary: report.Summary{Total: 1, Passed: 1}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := GetReport(e, role.Test, "")
	if err != nil {
		t.Fatal(err)
	}
	rep, ok := got.(*report.Report)
	if !ok || rep.RunID != "run-LATEST" {
		t.Fatalf("no-run_id form must return the latest report stamped with its run_id, got %T %v", got, got)
	}
}

// An unknown run_id (no per-run file, latest is a different run) is an honest
// pending/unknown signal, never the stale latest masquerading as yours.
func TestGetReport_UnknownRunID_PendingNotStale(t *testing.T) {
	dir := t.TempDir()
	e := Env{Instance: "local", ResultsRoot: dir}
	if err := writeReport(e.reportPath(), &report.Report{RunID: "run-OTHER", Summary: report.Summary{Total: 1, Passed: 1}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := GetReport(e, role.Test, "run-NOPE")
	if err != nil {
		t.Fatal(err)
	}
	if _, isReport := got.(*report.Report); isReport {
		t.Fatal("unknown run_id must NOT return the stale latest report as yours")
	}
	m, ok := got.(map[string]any)
	if !ok || m["status"] != "pending_or_unknown" {
		t.Fatalf("unknown run_id should signal pending_or_unknown, got %v", got)
	}
}

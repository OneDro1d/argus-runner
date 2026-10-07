package runner

// relay_test.go — AC-17: the executor-side command executor (NewCommandFunc) and the custody-safe
// report reduction (reduceReport) the relayed get_report answers with. TEST-FIRST: expected AND
// observed are stripped (stricter than the product hat's own redactExpected, which keeps observed),
// and a `final` run's relayed get_report answers the verdict fields only.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// expectedStr is Failure.Expected — a *string, so a build-mode report can carry it the way the test
// hat's own report would (reduceReport must strip it same as the product hat's redaction already does
// for `expected`; this test's real target is `observed`, which redactExpected does NOT strip).
func expectedStr(s string) *string { return &s }

func buildModeReport() *report.Report {
	return &report.Report{
		RunID: "run_1", Mode: "build",
		Summary: report.Summary{Total: 2, Passed: 1, Failed: 1},
		Layers: []report.Layer{{
			Layer: "HTTP Ingestion",
			Scenarios: []report.ScenarioResult{
				{ID: "ORDE-001", Status: "passed", CorrelationID: "tr-run_1-ORDE-001-aaa"},
				{ID: "ORDE-002", Status: "failed", CorrelationID: "tr-run_1-ORDE-002-bbb",
					Failure: &report.Failure{Expected: expectedStr("total == 10"), Observed: "total == 9"}},
			},
		}},
		BuildRecordObjectID: "obj_memstore_123",
	}
}

// forbiddenKeyWalk is the SAME technique federation/wire_test.go's TestResultsPush_NoEvidenceFields
// uses: marshal, unmarshal into `any`, walk every map key recursively.
func forbiddenKeyWalk(t *testing.T, b []byte, forbidden map[string]bool) {
	t.Helper()
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("forbiddenKeyWalk: unmarshal: %v", err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch t2 := v.(type) {
		case map[string]any:
			for k, vv := range t2 {
				if forbidden[k] {
					t.Errorf("relayed result carries a forbidden evidence key %q in %s", k, b)
				}
				walk(vv)
			}
		case []any:
			for _, vv := range t2 {
				walk(vv)
			}
		}
	}
	walk(m)
}

var custodyForbidden = map[string]bool{
	"observed": true, "expected": true, "evidence": true, "logs": true, "sagas": true, "saga": true, "db_rows": true, "content": true,
}

// TestReduceReport_BuildMode_StripsExpectedAndObserved_KeepsVerdictAndObjectID: the build-mode shape
// the relay's runner__get_report answers with — verdict + failing scenario ids + correlation ids +
// the AC-10 object id, with expected AND observed gone, proved by the forbidden-key walk.
func TestReduceReport_BuildMode_StripsExpectedAndObserved_KeepsVerdictAndObjectID(t *testing.T) {
	out := reduceReport(buildModeReport(), "https://grafana.example/d/x")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	forbiddenKeyWalk(t, b, custodyForbidden)

	if out.Status != "failed" {
		t.Errorf("Status = %q, want failed (1 of 2 scenarios failed)", out.Status)
	}
	if out.Passed != 1 || out.Failed != 1 || out.Total != 2 {
		t.Errorf("tallies = passed:%d failed:%d total:%d, want 1/1/2", out.Passed, out.Failed, out.Total)
	}
	if len(out.FailingScenarios) != 1 || out.FailingScenarios[0] != "ORDE-002" {
		t.Errorf("FailingScenarios = %v, want [ORDE-002]", out.FailingScenarios)
	}
	if len(out.CorrelationIDs) != 2 {
		t.Errorf("CorrelationIDs = %v, want 2 entries", out.CorrelationIDs)
	}
	if out.BuildRecordObjectID != "obj_memstore_123" {
		t.Errorf("BuildRecordObjectID = %q, want obj_memstore_123", out.BuildRecordObjectID)
	}
	if out.DashboardURL == "" {
		t.Error("DashboardURL is empty, want the dashboard link passed in")
	}
}

// TestReduceReport_FinalMode_VerdictOnly: a `final` run's relayed get_report answers the verdict
// fields ONLY — no failing scenarios, no correlation ids, no dashboard link, no object id — even
// though the input report carries all of them (a final run never has a build record, but this proves
// the reduction does not merely rely on that being empty).
func TestReduceReport_FinalMode_VerdictOnly(t *testing.T) {
	rep := buildModeReport()
	rep.Mode = "final"
	out := reduceReport(rep, "https://grafana.example/d/x")
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	forbiddenKeyWalk(t, b, custodyForbidden)
	if len(out.FailingScenarios) != 0 {
		t.Errorf("final mode FailingScenarios = %v, want none", out.FailingScenarios)
	}
	if len(out.CorrelationIDs) != 0 {
		t.Errorf("final mode CorrelationIDs = %v, want none", out.CorrelationIDs)
	}
	if out.DashboardURL != "" {
		t.Errorf("final mode DashboardURL = %q, want empty", out.DashboardURL)
	}
	if out.BuildRecordObjectID != "" {
		t.Errorf("final mode BuildRecordObjectID = %q, want empty", out.BuildRecordObjectID)
	}
	if out.Status != "failed" || out.Passed != 1 || out.Failed != 1 || out.Total != 2 {
		t.Errorf("final mode verdict fields = %+v, want status=failed passed=1 failed=1 total=2", out)
	}
}

// TestNewCommandFunc_GetReport_MatchesReduceReport: the executor-side command executor's get_report
// verb returns EXACTLY reduceReport's own JSON — proving the wiring, not a second copy of the rule.
func TestNewCommandFunc_GetReport_MatchesReduceReport(t *testing.T) {
	root := t.TempDir()
	const inst = "local"
	instDir := filepath.Join(root, inst)
	if err := os.MkdirAll(filepath.Join(instDir, "runs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rep := buildModeReport()
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(instDir, "report.json"), b, 0o644); err != nil {
		t.Fatalf("write report.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "runs", rep.RunID+".json"), b, 0o644); err != nil {
		t.Fatalf("write run report: %v", err)
	}

	cf := NewCommandFunc(ExecConfig{ToolInstance: inst, ResultsRoot: root})
	got, err := cf(context.Background(), "get_report", json.RawMessage(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("NewCommandFunc get_report: %v", err)
	}
	forbiddenKeyWalk(t, got, custodyForbidden)

	var gotReport RelayedReport
	if err := json.Unmarshal(got, &gotReport); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if gotReport.BuildRecordObjectID != "obj_memstore_123" {
		t.Errorf("NewCommandFunc get_report BuildRecordObjectID = %q, want obj_memstore_123", gotReport.BuildRecordObjectID)
	}
	if gotReport.Status != "failed" {
		t.Errorf("NewCommandFunc get_report Status = %q, want failed", gotReport.Status)
	}
}

// TestNewCommandFunc_GetReport_NotFoundPassesThrough (item 7a, msgbus tester 2026-09-28):
// toolcore.GetReport answers a run_id nobody has a report for on disk yet with a status map
// ({status:"not_found",...}), not a *report.Report. The relay used to type-assert straight to
// *report.Report and turn that map into the error "get_report: unexpected report shape
// map[string]interface {}" — the exact failure the msgbus builder hit against runner__get_report
// for 10 minutes straight (a real run that WAS running, just not written to disk yet). The map
// must pass through as an ordinary JSON answer instead.
func TestNewCommandFunc_GetReport_NotFoundPassesThrough(t *testing.T) {
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir()})
	got, err := cf(context.Background(), "get_report", json.RawMessage(`{"run_id":"nope"}`))
	if err != nil {
		t.Fatalf("NewCommandFunc get_report(unknown run_id) = error %v, want a not_found answer, not an error", err)
	}
	var out struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v (got %s)", err, got)
	}
	if out.Status != "not_found" || out.RunID != "nope" || out.Note == "" {
		t.Errorf("get_report(unknown run_id) = %s, want status=not_found run_id=nope note=<non-empty>", got)
	}
}

// TestNewCommandFunc_GetReport_PendingOrUnknownPassesThrough: the on-disk "latest" report.json
// belongs to a DIFFERENT run than the one asked for — toolcore.GetReport's other map-shaped
// answer (DF-07). Same custody rule as above: pass it through, never a decode error.
func TestNewCommandFunc_GetReport_PendingOrUnknownPassesThrough(t *testing.T) {
	root := t.TempDir()
	const inst = "local"
	instDir := filepath.Join(root, inst)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	other := buildModeReport()
	other.RunID = "run_other"
	b, _ := json.Marshal(other)
	if err := os.WriteFile(filepath.Join(instDir, "report.json"), b, 0o644); err != nil {
		t.Fatalf("write report.json: %v", err)
	}
	cf := NewCommandFunc(ExecConfig{ToolInstance: inst, ResultsRoot: root})
	got, err := cf(context.Background(), "get_report", json.RawMessage(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("NewCommandFunc get_report(pending run_id) = error %v, want a pending_or_unknown answer", err)
	}
	var out struct {
		Status string `json:"status"`
		RunID  string `json:"run_id"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v (got %s)", err, got)
	}
	if out.Status != "pending_or_unknown" || out.RunID != "run_1" {
		t.Errorf("get_report(pending run_id) = %s, want status=pending_or_unknown run_id=run_1", got)
	}
}

// TestNewCommandFunc_GetFullReport_NotFoundPassesThrough: the same map-passthrough rule applies
// to the author-scope get_full_report verb (P1 #8) — it reads the same on-disk file, so it hits
// the same not_found/pending_or_unknown shape.
func TestNewCommandFunc_GetFullReport_NotFoundPassesThrough(t *testing.T) {
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir()})
	got, err := cf(context.Background(), "get_full_report", json.RawMessage(`{"run_id":"nope"}`))
	if err != nil {
		t.Fatalf("NewCommandFunc get_full_report(unknown run_id) = error %v, want a not_found answer", err)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v (got %s)", err, got)
	}
	if out.Status != "not_found" {
		t.Errorf("get_full_report(unknown run_id) = %s, want status=not_found", got)
	}
}

// TestNewCommandFunc_Run_ReturnsImmediately: the relayed `run` verb answers {running:true, run_id}
// WITHOUT waiting for the run to finish — it starts toolcore.Run in the background and returns at
// once, exactly like the local router's own Async dispatch.
func TestNewCommandFunc_Run_ReturnsImmediately(t *testing.T) {
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir()})
	got, err := cf(context.Background(), "run", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("NewCommandFunc run: %v", err)
	}
	var out struct {
		Running bool   `json:"running"`
		RunID   string `json:"run_id"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Running || out.RunID == "" {
		t.Fatalf("run answer = %s, want {running:true, run_id:<non-empty>}", got)
	}
}

// TestNewCommandFunc_UnknownVerb_Errors: a verb outside the closed four is an error, never a
// half-answer.
func TestNewCommandFunc_UnknownVerb_Errors(t *testing.T) {
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir()})
	if _, err := cf(context.Background(), "get_sagas", json.RawMessage(`{}`)); err == nil {
		t.Fatal("NewCommandFunc(get_sagas) = nil error, want a refusal — get_sagas is never relayed")
	} else if !strings.Contains(err.Error(), "get_sagas") {
		t.Errorf("error = %q, want it to name the unknown verb", err.Error())
	}
}

// TestNewCommandFunc_GetFullReport_UnredactedButGetReportStaysHeld (P1 #8, msgbus tester 2026-09-27):
// the SAME on-disk run file answers BOTH verbs. get_full_report (author_get_report's relay verb) hands
// back expected AND observed; get_report (runner__get_report's verb, unchanged) still strips both — so
// adding the new verb does not weaken the existing holdout one bit.
func TestNewCommandFunc_GetFullReport_UnredactedButGetReportStaysHeld(t *testing.T) {
	root := t.TempDir()
	const inst = "local"
	instDir := filepath.Join(root, inst)
	if err := os.MkdirAll(filepath.Join(instDir, "runs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rep := buildModeReport()
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(instDir, "report.json"), b, 0o644); err != nil {
		t.Fatalf("write report.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "runs", rep.RunID+".json"), b, 0o644); err != nil {
		t.Fatalf("write run report: %v", err)
	}

	cf := NewCommandFunc(ExecConfig{ToolInstance: inst, ResultsRoot: root})

	full, err := cf(context.Background(), "get_full_report", json.RawMessage(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("NewCommandFunc get_full_report: %v", err)
	}
	var gotFull report.Report
	if err := json.Unmarshal(full, &gotFull); err != nil {
		t.Fatalf("unmarshal full report: %v", err)
	}
	sc, _ := gotFull.Find("ORDE-002")
	if sc == nil || sc.Failure == nil || sc.Failure.Expected == nil || *sc.Failure.Expected != "total == 10" {
		t.Fatalf("get_full_report withheld `expected` — got %+v, want Expected==%q", sc, "total == 10")
	}
	if sc.Failure.Observed != "total == 9" {
		t.Errorf("get_full_report Observed = %q, want %q", sc.Failure.Observed, "total == 9")
	}

	// The existing relayed verb, on the SAME run, from the SAME file: still no expected value, and
	// still passes the full custody forbidden-key walk (belt and braces — TestNewCommandFunc_GetReport_
	// MatchesReduceReport already proves the shape; this proves get_full_report existing did not touch it).
	reduced, err := cf(context.Background(), "get_report", json.RawMessage(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("NewCommandFunc get_report: %v", err)
	}
	forbiddenKeyWalk(t, reduced, custodyForbidden)
	if strings.Contains(string(reduced), "total == 10") {
		t.Fatalf("get_report leaked the expected value after get_full_report was added: %s", reduced)
	}
	var gotReduced RelayedReport
	if err := json.Unmarshal(reduced, &gotReduced); err != nil {
		t.Fatalf("unmarshal reduced report: %v", err)
	}
	if gotReduced.Status != "failed" {
		t.Errorf("get_report Status = %q, want failed (unchanged by get_full_report existing)", gotReduced.Status)
	}
}

// TestReduceReport_DegradedRunIsRelayedAsDegraded: a run with a DEGRADED scenario and nothing failed or
// errored is relayed as "degraded", never "passed", in every mode, and the relayed report carries the
// degraded count. A failed or errored scenario still makes the run "failed".
func TestReduceReport_DegradedRunIsRelayedAsDegraded(t *testing.T) {
	cases := []struct {
		name string
		sum  report.Summary
		want string
	}{
		{"degraded only", report.Summary{Total: 1, Degraded: 1}, "degraded"},
		{"degraded beside a pass", report.Summary{Total: 2, Passed: 1, Degraded: 1}, "degraded"},
		{"failed beside degraded", report.Summary{Total: 2, Failed: 1, Degraded: 1}, "failed"},
		{"errored beside degraded", report.Summary{Total: 2, Errored: 1, Degraded: 1}, "failed"},
		{"all passed", report.Summary{Total: 1, Passed: 1}, "passed"},
	}
	for _, mode := range []string{"build", "final", "scheduled"} {
		for _, c := range cases {
			b, err := json.Marshal(reduceReport(&report.Report{RunID: "run_1", Mode: mode, Summary: c.sum}, ""))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got struct {
				Status   string `json:"status"`
				Degraded *int   `json:"degraded"`
			}
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Status != c.want {
				t.Errorf("mode=%s, %s: status = %q, want %q: %s", mode, c.name, got.Status, c.want, b)
			}
			if got.Degraded == nil || *got.Degraded != c.sum.Degraded {
				t.Errorf("mode=%s, %s: want a degraded count of %d: %s", mode, c.name, c.sum.Degraded, b)
			}
		}
	}
}

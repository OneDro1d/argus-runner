package toolcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

func writeRunReport(t *testing.T, dir, runID, project string) Env {
	t.Helper()
	e := Env{Instance: "local", ResultsRoot: dir}
	rep := &report.Report{Project: project, RunID: runID, Timestamp: "2026-06-22T10:00:00Z",
		Summary: report.Summary{Total: 1, Passed: 1},
		Layers:  []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{{ID: "ORD-001", Status: "passed", CorrelationID: "tr-1"}}}}}
	if err := os.MkdirAll(filepath.Join(dir, "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "local", "report.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

// DF-07: get_report scoped to a run_id returns THAT run; a different/unknown run_id must
// NOT return the stale last report as "yours" — it returns a pending/not-found signal.
func TestGetReport_RunScoped(t *testing.T) {
	e := writeRunReport(t, t.TempDir(), "run-AAA", "order-service")

	// matching run_id → the run's report
	p, _, err := GetReport(e, role.Test, "run-AAA")
	if err != nil {
		t.Fatalf("get_report(matching run_id): %v", err)
	}
	if rep, ok := p.(*report.Report); !ok || rep.RunID != "run-AAA" {
		t.Fatalf("matching run_id must return that run's report, got %#v", p)
	}

	// different run_id (e.g. a stale cross-project report on disk) → NOT returned as the run
	p2, _, _ := GetReport(e, role.Test, "run-ZZZ")
	b, _ := json.Marshal(p2)
	if rep, ok := p2.(*report.Report); ok && rep.RunID == "run-AAA" {
		t.Fatalf("a non-matching run_id must NOT return the stale report as the run: %s", b)
	}
	if !strings.Contains(string(b), "run-ZZZ") {
		t.Fatalf("the response should reference the requested run_id + a pending/not-found status: %s", b)
	}

	// no run_id → back-compat: returns the last report
	p3, _, err := GetReport(e, role.Test, "")
	if err != nil || p3.(*report.Report).RunID != "run-AAA" {
		t.Fatalf("no run_id → last report (back-compat): %v %#v", err, p3)
	}
}

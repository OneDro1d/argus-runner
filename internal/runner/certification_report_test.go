package runner

// (#414), part b: the executor records the run's REAL mode, and reduceReport keeps
// per-scenario fields only for a build run (or the builder's own local "ci" run).

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

func TestReduceReport_VerdictOnlyForEveryModeButBuildAndLocal(t *testing.T) {
	for _, mode := range []string{"final", "scheduled", "rehearsal", "", "mystery"} {
		rep := &report.Report{Mode: mode, RunID: "r1", BuildRecordObjectID: "obj-1"}
		rep.Summary.Failed, rep.Summary.Total = 1, 2
		rep.Layers = []report.Layer{{Scenarios: []report.ScenarioResult{
			{ID: "CERT-001", Status: "failed", CorrelationID: "tr-r1-CERT-001-x"},
			{ID: "CERT-002", Status: "passed", CorrelationID: "tr-r1-CERT-002-x"},
		}}}
		out := reduceReport(rep, "https://grafana.example/d/x")
		b, _ := json.Marshal(out)
		for _, leak := range []string{"failing_scenarios", "correlation_ids", "dashboard_url", "build_record_object_id", "CERT-00"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("mode %q: the relayed report leaks %q: %s", mode, leak, b)
			}
		}
		if out.Status != "failed" || out.Failed != 1 || out.Total != 2 {
			t.Errorf("mode %q: the verdict and tallies must survive: %s", mode, b)
		}
	}
	for _, mode := range []string{"build", "ci"} {
		rep := &report.Report{Mode: mode, RunID: "r1"}
		rep.Summary.Failed, rep.Summary.Total = 1, 1
		rep.Layers = []report.Layer{{Scenarios: []report.ScenarioResult{{ID: "S-1", Status: "failed", CorrelationID: "tr-1"}}}}
		out := reduceReport(rep, "d")
		if len(out.FailingScenarios) != 1 || len(out.CorrelationIDs) != 1 || out.DashboardURL != "d" {
			t.Errorf("mode %q is the builder's own run: it keeps its scenarios; got %+v", mode, out)
		}
	}
}

// NewRunFunc hands the assignment's mode to the run core; a direct run (no mode, no run request) is
// left as the local "ci" run it is, and a federated assignment that arrived with NO mode is marked
// "unknown" so that it fails closed everywhere downstream.
func TestNewRunFunc_RecordsTheAssignmentsRealMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    *federation.RunAssignment
		want string
	}{
		{"final", &federation.RunAssignment{Mode: "final", ArtifactDigest: mdA, RunID: "r-f", RunRequestID: "rr-1"}, "final"},
		{"build", &federation.RunAssignment{Mode: "build", RunID: "r-b", RunRequestID: "rr-2"}, "build"},
		{"direct run, no mode", &federation.RunAssignment{RunID: "r-d"}, ""},
		{"federated, no mode", &federation.RunAssignment{RunID: "r-u", RunRequestID: "rr-3"}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			stubRunCore(t) // installs the stub core and restores the real one on cleanup
			inner := toolcoreRun
			toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
				seen = e.RunMode
				return inner(e, runID, layer, tag, scenarioID)
			}
			cfg := ExecConfig{ResultsRoot: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "argus-config.yaml")}
			if _, err := NewRunFunc(cfg)(context.Background(), tc.a); err != nil {
				t.Fatalf("run: %v", err)
			}
			if seen != tc.want {
				t.Errorf("the run core was given RunMode %q, want %q", seen, tc.want)
			}
		})
	}
}

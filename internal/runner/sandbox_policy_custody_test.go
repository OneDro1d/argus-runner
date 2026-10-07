package runner

// Spec 26 P1 the sandbox_policy block is in-env evidence. On a certification run a
// builder gets the verdict only (the #417 rule); on no run does the block leave the executor, so the
// relayed get_report, the results push and everything the control plane builds from them (the
// ledger, the web UI, certificationVerdictOnly) can never carry it.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

const runnerSandboxCanary = "sandbox-target-canary.example"

func sandboxBlockReport(mode, runID string) report.Report {
	one := 1
	return report.Report{
		Project: "p", RunID: runID, Mode: mode,
		Summary: report.Summary{Total: 2, Passed: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "http", Scenarios: []report.ScenarioResult{
			{ID: "SB-001", Status: "failed", DurationMs: 5, CorrelationID: "tr-" + runID + "-SB-001-ab",
				SandboxPolicy: &report.SandboxPolicy{Source: "loki", Sandbox: "sb-canary-1", Coverage: report.CoverageComplete,
					CoverageReason: "read 2 lines from the sandbox", DeniedCount: &one,
					Events: []report.SandboxPolicyEvent{{Class: "NET:OPEN", Action: "Denied", Target: runnerSandboxCanary + ":443", Reason: "no matching policy"}}}},
			{ID: "SB-002", Status: "passed", DurationMs: 5, CorrelationID: "tr-" + runID + "-SB-002-ab",
				SandboxPolicy: &report.SandboxPolicy{Source: "loki", Sandbox: "sb-canary-1", Coverage: report.CoverageUnavailable,
					CoverageReason: "Loki answered HTTP 401", Events: []report.SandboxPolicyEvent{}}},
		}}},
	}
}

// sandboxTraces names every trace of the block in b. The bare coverage words are not listed: a
// results push carries other words of its own; the block's key, fields, ids and reasons are.
func sandboxTraces(b []byte) []string {
	var out []string
	for _, w := range []string{"sandbox_policy", "denied_count", "coverage", "events_omitted", "shared_with",
		runnerSandboxCanary, "sb-canary-1", "no matching policy", "HTTP 401", "read 2 lines"} {
		if strings.Contains(string(b), w) {
			out = append(out, w)
		}
	}
	return out
}

// The executor-side get_report verb (what runner__get_report relays to a builder): no block on any
// run. On a certification run the answer is the verdict alone (#417); on a build or local run the
// relayed answer is the custody-reduced projection, which has no field for the block.
func TestNewCommandFunc_GetReport_NeverRelaysSandboxPolicy(t *testing.T) {
	for _, mode := range []string{"final", "scheduled", "rehearsal", "unknown", "", "build", "ci"} {
		t.Run("mode="+mode, func(t *testing.T) {
			root := t.TempDir()
			const inst = "local"
			instDir := filepath.Join(root, inst)
			if err := os.MkdirAll(filepath.Join(instDir, "runs"), 0o755); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(sandboxBlockReport(mode, "run_sb"))
			for _, p := range []string{filepath.Join(instDir, "report.json"), filepath.Join(instDir, "runs", "run_sb.json")} {
				if err := os.WriteFile(p, b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cf := NewCommandFunc(ExecConfig{ToolInstance: inst, ResultsRoot: root})
			got, err := cf(context.Background(), "get_report", json.RawMessage(`{"run_id":"run_sb"}`))
			if err != nil {
				t.Fatal(err)
			}
			if leaks := sandboxTraces(got); len(leaks) > 0 {
				t.Errorf("the relayed get_report carries %q: %s", leaks, got)
			}
			var rr RelayedReport
			if err := json.Unmarshal(got, &rr); err != nil || rr.Status != "failed" || rr.Total != 2 {
				t.Errorf("the verdict and tallies must survive: %s (%v)", got, err)
			}
		})
	}
}

// The results push of a run (what the control plane's ledger, web UI and certificationVerdictOnly are
// built from) carries none of the block, on a certification run and a build run alike, while the
// rows themselves are pushed.
func TestNewRunFunc_PushCarriesNoSandboxPolicy(t *testing.T) {
	for _, mode := range []string{"final", "scheduled", "rehearsal", "build"} {
		t.Run("mode="+mode, func(t *testing.T) {
			prev := toolcoreRun
			toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
				b, _ := json.Marshal(sandboxBlockReport(e.RunMode, runID))
				dir := filepath.Join(e.ResultsRoot, e.Instance)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, false, err
				}
				return nil, true, os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
			}
			t.Cleanup(func() { toolcoreRun = prev })
			a := &federation.RunAssignment{Mode: mode, RunID: "run-sb-" + mode, RunRequestID: "rr-sb"}
			if mode == "final" || mode == "scheduled" {
				a.ArtifactDigest = mdA
			}
			push, err := runFinal(t, ExecConfig{MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}, nil)}, a)
			if err != nil {
				t.Fatal(err)
			}
			pb, _ := json.Marshal(push)
			if leaks := sandboxTraces(pb); len(leaks) > 0 {
				t.Errorf("the results push carries %q: %s", leaks, pb)
			}
			if len(push.Scenarios) != 2 {
				t.Errorf("both rows must still be pushed (id, outcome, hash): %s", pb)
			}
		})
	}
}

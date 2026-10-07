package runner

// cmp11_test.go -- ARGUS-CMP-11: what the executor does with the numbers of a comparison and with the target of a member,
// from the assignment to the push. The run core is stubbed where the test is about the plumbing (NewRunFunc) and is the
// REAL toolcore.Run, over a real chain scenario against an httptest SUT, where the test is about what the push carries.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

func TestCMP11_TheMembersTargetReachesTheRunCoreInModeCompareAndOnlyThere(t *testing.T) {
	var seen []string
	prev := toolcoreRun
	toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
		seen = append(seen, e.RunMode+"="+e.CompareTarget)
		rep := report.Report{RunID: runID, Mode: e.RunMode, Summary: report.Summary{Passed: 1, Total: 1},
			Layers: []report.Layer{{Layer: "Permissions", Scenarios: []report.ScenarioResult{{ID: "CHN-A", Status: "passed"}}}}}
		b, _ := json.Marshal(rep)
		dir := filepath.Join(e.ResultsRoot, e.Instance)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, err
		}
		return nil, true, os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
	}
	t.Cleanup(func() { toolcoreRun = prev })
	k := newKey(t)

	a := compareAssignment(t, k)
	a.CompareTarget = "graph"
	if _, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k})(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	// a build assignment that somehow carried a target does not use it
	b := &federation.RunAssignment{Mode: "build", RunID: "run-b1", RunRequestID: "rq-b", Scope: "full", SetHash: "h", CompareTarget: "graph",
		Scenarios: []federation.ScenarioPayload{{Path: "permissions/CHN-A.md", Body: "# Scenario: a"}}}
	if _, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k})(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "compare=graph,build=" {
		t.Errorf("the run core saw %v, want compare=graph and build= (nothing)", seen)
	}
}

func TestCMP11_ARealExecutorPushCarriesTheTolerantNumbersAsNumbers(t *testing.T) {
	const canary = 987654321.125
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ws-1","total":987654321.125,"note":"keep"}`))
	}))
	defer srv.Close()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: c", "", "## Metadata", "- **ID**: CHN-TOL", "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json",
		`{"steps":[{"type":"http","name":"create","method":"POST","url":"` + srv.URL + `/things"}]}`, "```", "",
		"## EXPECT", "### Runnable", "- step create: status=200", "", "## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		"## COMPARE", "- **Reference**: measured", "- **Output**: status, body", "- **Mask**: $.id", "- **Tolerance**: $.total abs 0.01", "",
	}, "\n")
	k := newKey(t)
	a := &federation.RunAssignment{Mode: "compare", RunID: "run-tol", RunRequestID: "rq-tol", Scope: "full", SetHash: "sh",
		CompareScenarios: []federation.ScenarioPayload{sealed(t, k, "run-tol", "permissions/CHN-TOL.md", md)}}
	t.Setenv("ARGUS_SUT_NAMESPACE", "")
	push, err := NewRunFunc(ExecConfig{ConfigPath: cfgPath, ResultsRoot: root, X25519Priv: k})(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if push.Tallies.Passed != 1 {
		t.Fatalf("tallies %+v: a tolerant check on an E2 executor must run", push.Tallies)
	}
	rows, norm, derr := compare.DecodeOutputs(push.Outputs)
	if derr != nil || len(rows) != 1 || rows[0].State != compare.StateRecorded {
		t.Fatalf("outputs %s (%v)", push.Outputs, derr)
	}
	if len(rows[0].Values) != 1 || rows[0].Values[0].Path != "$.total" || rows[0].Values[0].Value != canary {
		t.Fatalf("values = %+v, want [{$.total 0 987654321.125}] as a NUMBER", rows[0].Values)
	}
	if !strings.Contains(string(norm), `"values":[{"path":"$.total","rule":0,"value":987654321.125}]`) {
		t.Errorf("the wire form of the values = %s", norm)
	}
	if strings.Contains(string(norm), "keep") || strings.Contains(string(norm), "ws-1") {
		t.Errorf("a body value crossed: %s", norm)
	}
	if push.OutputsRoot != compare.OutputsRoot(rows) {
		t.Errorf("outputs_root %q", push.OutputsRoot)
	}
}

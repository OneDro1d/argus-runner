package toolcore

// / — StepResult.FailedClaims is AUTHOR-ONLY. toolcore.GetReport is
// the one read every local builder surface goes through (the runner__get_report tool, the CLI's
// `argus get-report` under a runner credential, and the relay's get_report verb before it is reduced),
// so redactExpected must drop the field on BOTH its read paths (the per-run file and the latest
// report.json) and in the exported RedactExpected the build record uses.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

const (
	fcObserved = "SENTINEL-OBSERVED-4K8"
	fcClaim    = "SENTINEL-CLAIM-7Q1"
)

func fcReport() *report.Report {
	return &report.Report{
		RunID: "run_1", Project: "p", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "Chain", Scenarios: []report.ScenarioResult{{
			ID: "CHAI-001", Status: "failed",
			Steps: []report.StepResult{{
				Name: "check", Status: "failed", Observed: "the claims did not hold",
				AssertionsEnforced: []string{"field " + fcClaim + " > 5"}, AssertionsEnforcedCount: 1,
				FailedClaims: []report.FailedClaim{{Claim: "field " + fcClaim + " > 5", Observed: fcObserved}},
			}},
		}}}},
	}
}

func fcEnv(t *testing.T, rep *report.Report) Env {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "local")
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rep)
	for _, p := range []string{filepath.Join(dir, "report.json"), filepath.Join(dir, "runs", rep.RunID+".json")} {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Env{Instance: "local", ResultsRoot: root, ScenariosDir: root}
}

func fcGet(t *testing.T, e Env, hat role.Role, runID string) string {
	t.Helper()
	p, _, err := GetReport(e, hat, runID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func TestFailedClaims_Builder_GetReport_BothReadPaths(t *testing.T) {
	e := fcEnv(t, fcReport())
	for name, runID := range map[string]string{"per-run file": "run_1", "latest report.json": ""} {
		out := fcGet(t, e, role.Product, runID)
		for _, needle := range []string{fcObserved, fcClaim, `"failed_claims":`} {
			if strings.Contains(out, needle) {
				t.Errorf("builder GetReport (%s) carries %q: %s", name, needle, out)
			}
		}
		// the author's read of the same file carries it: the positive control
		author := fcGet(t, e, role.Test, runID)
		if !strings.Contains(author, fcObserved) || !strings.Contains(author, `"failed_claims"`) {
			t.Errorf("author GetReport (%s) must carry failed_claims and the observed value: %s", name, author)
		}
	}
}

func TestFailedClaims_Builder_RedactExpected(t *testing.T) {
	rep := fcReport()
	RedactExpected(rep, role.Product)
	b, _ := json.Marshal(rep)
	for _, needle := range []string{fcObserved, `"failed_claims":`} {
		if strings.Contains(string(b), needle) {
			t.Errorf("RedactExpected(product) left %q: %s", needle, b)
		}
	}
	if rep.Layers[0].Scenarios[0].Steps[0].AssertionsEnforcedCount != 1 {
		t.Errorf("the count stays for the product hat: %+v", rep.Layers[0].Scenarios[0].Steps[0])
	}
	author := fcReport()
	RedactExpected(author, role.Test)
	if len(author.Layers[0].Scenarios[0].Steps[0].FailedClaims) != 1 {
		t.Errorf("RedactExpected(test) must not touch the author's record")
	}
}

// End to end: a REAL chain step fails on its claims against a live (httptest) SUT, the result is
// written the way a run writes it, and the two hats read it back. The author sees the claim and the
// observed value; the builder sees neither.
func TestFailedClaims_EndToEnd_AuthorSeesItBuilderDoesNot(t *testing.T) {
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"count": 4242}`)) // the SUT's answer: distinctive, and a miss for `count > 99999`
	}))
	defer sut.Close()
	step := chain.HTTPStep("check", "GET", sut.URL+"/b", nil, "", 200,
		[]mcp.BodyAssert{{Field: "count", Op: mcp.BodyGTOp, Value: "99999"}}, nil, nil, false, nil, &scenario.MoneySpendLedger{})
	res := chain.Run("tr-x", []chain.Step{step})
	if res.Status != "failed" {
		t.Fatalf("want a failed chain, got %+v", res)
	}
	rep := &report.Report{RunID: "run_1", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "Chain", Scenarios: []report.ScenarioResult{res}}}}
	e := fcEnv(t, rep)

	author := fcGet(t, e, role.Test, "run_1")
	if !strings.Contains(author, `"failed_claims"`) || !strings.Contains(author, "field count \\u003e 99999") && !strings.Contains(author, "field count > 99999") || !strings.Contains(author, "4242") {
		t.Fatalf("the author's report must name the claim and the observed 4242: %s", author)
	}
	builder := fcGet(t, e, role.Product, "run_1")
	// (the step's TEXT names the field, so the key is matched with its quotes and colon)
	if strings.Contains(builder, "4242") || strings.Contains(builder, `"failed_claims":`) {
		t.Fatalf("the builder's report carries the observed value or the field: %s", builder)
	}
}

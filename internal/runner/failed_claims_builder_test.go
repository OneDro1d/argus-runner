package runner

// StepResult.FailedClaims is AUTHOR-ONLY: it carries the claim and
// the SUT's observed value. Every path on the executor side that hands a report to the builder hat is
// asserted here to carry neither the field nor a planted observed value; the author path (get_full_
// report) is the positive control, so a test that cannot fail is caught.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

const (
	fcSentinelObserved = "SENTINEL-OBSERVED-4K8"
	fcSentinelClaim    = "SENTINEL-CLAIM-7Q1"
)

func reportWithFailedClaims() *report.Report {
	return &report.Report{
		RunID: "run_1", Mode: "build",
		Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{
			Layer: "Chain",
			Scenarios: []report.ScenarioResult{{
				ID: "CHAI-001", Status: "failed", CorrelationID: "tr-run_1-CHAI-001-aaa",
				Failure: &report.Failure{Observed: "step 'check' failed: the claims did not hold"},
				Steps: []report.StepResult{{
					Name: "check", Status: "failed", Observed: "http status 200 matched but the body did not satisfy the claims",
					AssertionsEnforced: []string{"field " + fcSentinelClaim + " > 5"}, AssertionsEnforcedCount: 1,
					FailedClaims:        []report.FailedClaim{{Claim: "field " + fcSentinelClaim + " > 5", Observed: fcSentinelObserved}},
					FailedClaimsOmitted: 37, // author-only, planted non-zero
				}},
			}},
		}},
	}
}

func assertNoFailedClaims(t *testing.T, path string, b []byte) {
	t.Helper()
	// the JSON key with its quotes and colon: a step's TEXT may name the field (the pointer K3 added)
	for _, needle := range []string{fcSentinelObserved, fcSentinelClaim, `"failed_claims":`, "failed_claims_omitted"} {
		if strings.Contains(string(b), needle) {
			t.Errorf("builder path %s carries %q: %s", path, needle, b)
		}
	}
}

func writeRunFixture(t *testing.T, rep *report.Report) (root string) {
	t.Helper()
	root = t.TempDir()
	instDir := filepath.Join(root, "local")
	if err := os.MkdirAll(filepath.Join(instDir, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(instDir, "report.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "runs", rep.RunID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// builder path: runner.reduceReport (relay.go) — the ONE place a relayed get_report is reduced; it
// copies named fields only, so the field has nowhere to ride.
func TestFailedClaims_Builder_ReduceReport(t *testing.T) {
	b, _ := json.Marshal(reduceReport(reportWithFailedClaims(), "http://dash"))
	assertNoFailedClaims(t, "runner.reduceReport", b)
}

// builder path: the relayed get_report verb, end to end from the on-disk run file (toolcore.GetReport
// with role.Product, then reduceReport). The same file, read by get_full_report (the author's verb),
// DOES carry it — the positive control.
func TestFailedClaims_Builder_RelayedGetReportVsAuthorGetFullReport(t *testing.T) {
	root := writeRunFixture(t, reportWithFailedClaims())
	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: root})

	for _, runArgs := range []string{`{"run_id":"run_1"}`, `{}`} { // per-run file, then the "latest" report.json
		got, err := cf(context.Background(), "get_report", json.RawMessage(runArgs))
		if err != nil {
			t.Fatalf("get_report %s: %v", runArgs, err)
		}
		assertNoFailedClaims(t, "relay get_report "+runArgs, got)
	}

	full, err := cf(context.Background(), "get_full_report", json.RawMessage(`{"run_id":"run_1"}`))
	if err != nil {
		t.Fatalf("get_full_report: %v", err)
	}
	if !strings.Contains(string(full), fcSentinelObserved) || !strings.Contains(string(full), `"failed_claims"`) ||
		!strings.Contains(string(full), `"failed_claims_omitted":37`) {
		t.Fatalf("positive control: the AUTHOR's get_full_report must carry failed_claims and the observed value: %s", full)
	}
}

// builder path: the federation results push (mapper.go) — counts, ids and a generic summary only.
func TestFailedClaims_Builder_FederationPush(t *testing.T) {
	push := mapReport(reportWithFailedClaims(), "run_1", "", "full", "sethash", "", federation.Annotations{},
		time.Unix(0, 0), time.Unix(1, 0), "")
	b, _ := json.Marshal(push)
	assertNoFailedClaims(t, "runner.mapReport (federation push)", b)
}

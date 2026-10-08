package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// / — the local builder surface: runner__get_report with the runner
// (product) token must carry neither StepResult.FailedClaims nor the observed value planted in it,
// through the MCP envelope; the author token reads the same file WITH it (the positive control).
const (
	fcSentinelObserved = "SENTINEL-OBSERVED-4K8"
	fcSentinelClaim    = "SENTINEL-CLAIM-7Q1"
)

func writeFailedClaimsFixture(t *testing.T) toolcore.Env {
	t.Helper()
	tmp := t.TempDir()
	// Mode "ci": a builder only sees per-scenario results on a build/ci run (ScenariosVisibleToBuilder).
	// With Mode empty every scenario was withheld BEFORE the failed-claims redaction ran, so the absence
	// asserts below passed vacuously — deleting toolcore's `FailedClaims = nil` left this test green.
	rep := report.Report{
		Project: "fixture", Mode: "ci", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "chain", Scenarios: []report.ScenarioResult{{
			ID: "CHAI-001", Status: "failed", CorrelationID: "tr-x",
			Steps: []report.StepResult{{
				Name: "check", Status: "failed", Observed: "the claims did not hold",
				AssertionsEnforced: []string{"field " + fcSentinelClaim + " > 5"}, AssertionsEnforcedCount: 1,
				FailedClaims:        []report.FailedClaim{{Claim: "field " + fcSentinelClaim + " > 5", Observed: fcSentinelObserved}},
				FailedClaimsOmitted: 37, // author-only, planted non-zero
			}},
		}}}},
	}
	b, _ := json.MarshalIndent(&rep, "", "  ")
	dir := filepath.Join(tmp, "local")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return toolcore.Env{Instance: "local", ResultsRoot: tmp, ScenariosDir: tmp,
		Grafana: "http://grafana", Loki: "http://127.0.0.1:9"}
}

func TestFailedClaims_Builder_RunnerGetReportEnvelope(t *testing.T) {
	env := writeFailedClaimsFixture(t)
	builder := string(callGetReport(t, env, rtok))
	// Sanity: the builder really was shown the scenario, so the absence below is the redaction's work
	// and not the scenarios being withheld wholesale.
	if !strings.Contains(builder, "CHAI-001") {
		t.Fatalf("sanity: the builder envelope must carry the scenario (else the absence checks prove nothing):\n%s", builder)
	}
	for _, needle := range []string{fcSentinelObserved, fcSentinelClaim} {
		if strings.Contains(builder, needle) {
			t.Fatalf("SECURITY: the builder's runner__get_report envelope carries %q:\n%s", needle, builder)
		}
	}
	// the JSON key may sit in the envelope as an escaped string, so match both spellings
	if strings.Contains(builder, `\"failed_claims\":`) || strings.Contains(builder, `"failed_claims":`) {
		t.Fatalf("SECURITY: the builder's runner__get_report envelope carries the failed_claims field:\n%s", builder)
	}
	if strings.Contains(builder, "failed_claims_omitted") {
		t.Fatalf("SECURITY: the builder's runner__get_report envelope carries failed_claims_omitted:\n%s", builder)
	}
	author := string(callGetReport(t, env, atok))
	if !strings.Contains(author, "failed_claims_omitted") {
		t.Fatalf("positive control: the author's get_report must keep failed_claims_omitted:\n%s", author)
	}
	if !strings.Contains(author, fcSentinelObserved) {
		t.Fatalf("positive control: the author's get_report must carry the planted observed value:\n%s", author)
	}
}

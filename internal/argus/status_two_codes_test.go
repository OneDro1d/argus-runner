package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// THE EXECUTOR'S DOORS REFUSE A STATUS CLAIM THAT LISTS TWO CODES.
//
// `status=200/201` used to be read as `status=200`: DeriveProps wired expect.status=200, the run fired, and
// the second code was never compared. The grammar and its refusal live in internal/scenario
// (CheckDeclaredStatuses); these tests pin that the runtime reads through it, so a scenario that reaches a
// run without passing the validator (a catalogue written before the rule) is REPORTED, not judged on half
// of what its author wrote. The validator half is pinned in internal/scenario and internal/toolcore.

// DOOR 1 — DeriveProps (the JMeter path's property set).
func TestDeriveProps_RefusesAStatusClaimListingTwoCodes(t *testing.T) {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	for _, claim := range []string{"status=200/201", "status=200,201", "status=200|201", "status=200 or 201"} {
		s := scenario.Parse(scen("HTTP Ingestion", "### Runnable\n- "+claim+"\n"))
		p, err := DeriveProps(c, s, "tr-test-0001")
		if err == nil {
			t.Errorf("%q: DeriveProps wired expect.status=%q and returned no error — the second code was dropped silently", claim, p["expect.status"])
			continue
		}
		if !strings.Contains(err.Error(), "lists two codes") {
			t.Errorf("%q: the refusal must name the problem; got %v", claim, err)
		}
	}
}

// DOOR 2 — the run loop's preflight. The scenario is REPORTED `error` (nothing about the SUT was measured,
// so it is never `failed`) and its request is NEVER fired. A scenario that is silently judged on 200 alone
// is the defect.
func TestRunAll_AStatusClaimListingTwoCodesIsErroredAndNeverFired(t *testing.T) {
	for _, claim := range []string{"status=200/201", "status=200,201", "status=200|201", "status=200 or 201"} {
		t.Run(claim, func(t *testing.T) {
			dir := t.TempDir()
			sc := filepath.Join(dir, "scenarios")
			if err := os.MkdirAll(filepath.Join(sc, "http-ingestion"), 0o755); err != nil {
				t.Fatal(err)
			}
			md := strings.Join([]string{
				"# Scenario: two codes", "",
				"## Metadata",
				"- **ID**: TWO-001",
				"- **Layer**: HTTP Ingestion",
				"- **Tags**: http, order", "",
				"## TRIGGER",
				"GET `${INGESTION_URL}/health`", "",
				"## EXPECT",
				"### Runnable",
				"- " + claim, "",
				"## TIMEOUT", "30s", "",
				"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
			}, "\n")
			if err := os.WriteFile(filepath.Join(sc, "http-ingestion", "TWO-001.md"), []byte(md), 0o644); err != nil {
				t.Fatal(err)
			}

			rec := &recordingRunner{}
			rr, err := RunAll(httpConfig(), sc, filepath.Join(dir, "results"), "p", "", "", "", "", rec)
			if err != nil {
				t.Fatalf("RunAll: %v", err)
			}
			res, _ := rr.Report.Find("TWO-001")
			if res == nil {
				t.Fatal("TWO-001 is missing from the report — a scenario that cannot be judged must still be REPORTED")
			}
			if res.Status != report.StatusError {
				t.Errorf("status = %q, want %q — the claim is unreadable, nothing about the SUT was measured", res.Status, report.StatusError)
			}
			if res.Failure == nil || !strings.Contains(res.Failure.Observed, "lists two codes") {
				t.Errorf("the observed must name the problem, got %+v", res.Failure)
			}
			if len(rec.props) != 0 {
				t.Errorf("the runner fired %d request(s) for a scenario whose status claim is ambiguous", len(rec.props))
			}
		})
	}
}

// DOOR 3 — a chain http step. httpStepExpect delegates to scenario.HTTPStepClaims.
func TestHTTPStepExpect_RefusesAStatusClaimListingTwoCodes(t *testing.T) {
	for _, claim := range []string{"status=200/201", "status=200,201", "status=200|201", "status=200 or 201"} {
		st, _, err := httpStepExpect([]string{claim})
		if err == nil {
			t.Errorf("%q: httpStepExpect returned status %d and no error — the second code was dropped silently", claim, st)
		} else if !strings.Contains(err.Error(), "lists two codes") {
			t.Errorf("%q: the refusal must name the problem; got %v", claim, err)
		}
	}
	if st, _, err := httpStepExpect([]string{"status: 404"}); err != nil || st != 404 {
		t.Errorf("an ordinary claim must keep working: status %d err %v", st, err)
	}
}

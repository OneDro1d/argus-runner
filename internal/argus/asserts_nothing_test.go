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

// V31-002 R10 — A SCENARIO THAT DECLARES NOTHING IS REPORTED, NOT RUN.
//
// Measured on 0.3.31: an mcp or chain scenario with no runnable bullet RAN and PASSED — one request,
// no failure, nothing compared. Only the JMeter path caught it, through NoDeclaredStatusObserved.
// R1 makes this reachable for every old flat file too, so without R10 the removal of the old-format
// leniency would turn a half-executed file into a silently green one, which is worse than before.
//
// ⛔ IT IS NOT A FORMAT RULE. It states what was MEASURED — nothing — from one condition on the
// parsed file, and holds no copy of the validator's rules (the owner's DEC-1). The proof that it is
// not a refusal is the request count: the SUT is never called, so there is nothing to refuse.
func TestRunAll_AScenarioWithNoRunnableCheckIsErroredAndNeverFired(t *testing.T) {
	cases := []struct{ name, tags, layer, expect string }{
		{"mcp, only prose", "mcp", "HTTP Ingestion", "### Non-runnable\n- the tool answers"},
		{"chain, only prose", "chain", "Permissions", "### Non-runnable\n- the steps run in order"},
		// ⭐ the old flat shape: R1 no longer treats its bullets as runnable, so it lands here.
		{"an OLD flat file", "mcp", "HTTP Ingestion", "- result.isError == false"},
		{"an EXPECT with nothing under either heading", "mcp", "HTTP Ingestion", "### Runnable\n\n### Non-runnable\n- prose"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			scDir := filepath.Join(dir, "scenarios")
			d := filepath.Join(scDir, "http-ingestion")
			_ = os.MkdirAll(d, 0o755)
			md := strings.Join([]string{
				"# Scenario: N", "",
				"## Metadata", "- **ID**: NONE-001", "- **Layer**: " + c.layer, "- **Tags**: " + c.tags, "",
				"## TRIGGER", "POST \x60${MCP_URL}\x60", "",
				"```json",
				`{"transport":"streamable-http","tool":"t","args":{}}`,
				"```", "",
				"## EXPECT", c.expect, "",
				"## TIMEOUT", "30s", "",
				"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
			}, "\n")
			if err := os.WriteFile(filepath.Join(d, "NONE-001.md"), []byte(md), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg := &config.Config{}
			cfg.Project.Name = "p"
			cfg.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
			fr := &fakeRunner{pass: map[string]bool{"NONE-001": true}} // would PASS if it ran
			rr, err := RunAll(cfg, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
			if err != nil {
				t.Fatalf("RunAll: %v", err)
			}
			res, _ := rr.Report.Find("NONE-001")
			if res == nil {
				t.Fatal("NONE-001 not in the report")
			}
			if res.Status != report.StatusError {
				t.Fatalf("status = %q, want %q — it declared nothing, so nothing was measured", res.Status, report.StatusError)
			}
			if res.Failure == nil || res.Failure.Observed != AssertsNothingObserved {
				t.Errorf("Observed = %+v, want AssertsNothingObserved", res.Failure)
			}
			// ⛔ the load-bearing half: it returns BEFORE the dispatch, so the SUT is never touched.
			if res.ReqSuccess+res.ReqFailed+res.ReqError != 0 {
				t.Errorf("requests = %d/%d/%d, want 0/0/0 — nothing may be fired for a scenario that asks nothing",
					res.ReqSuccess, res.ReqFailed, res.ReqError)
			}
		})
	}
}

// GUARD — a `ui` scenario is checked through the SAME predicate the author path uses, so the two
// cannot drift. It passes R10 by naming its own spec, exactly as it passes W2.
func TestRunAll_UIScenarioNamingItsSpecIsNotCaughtByR10(t *testing.T) {
	md := strings.Join([]string{
		"# Scenario: U", "",
		"## Metadata", "- **ID**: UI-R10", "- **Layer**: Web UI", "- **Tags**: ui", "",
		"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
		"## EXPECT", "### Non-runnable", "- the dashboard renders", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	s := scenario.Parse(md)
	if len(s.RunnableExpect()) != 0 {
		t.Fatalf("the fixture must declare no runnable bullet, got %v", s.RunnableExpect())
	}
	if !scenario.UINamesItsOwnSpec(s) {
		t.Fatal("a ui scenario naming its own spec must satisfy the rule — its assertions live there")
	}
}

package argus

import (
	"os"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// An MCP SUT that never answers is UNRUN, not failed. mcpRequestOutcome already classified the call
// as an error one level down; before this the scenario rollup could only say passed or failed, so an
// absent server was reported as a product defect.
func TestRunMCPScenario_UnreachableIsErroredNotFailed(t *testing.T) {
	os.Unsetenv("MCP_URL")
	res := runMCPScenario(mcpCfg(t, "http://127.0.0.1:1/mcp"), scenario.Parse(mcpScenarioMD()), "tr-un")

	if res.Status != report.StatusError {
		t.Fatalf("status = %q, want %q — the SUT never answered, so nothing failed", res.Status, report.StatusError)
	}
	if !res.NeverReachedSUT() {
		t.Fatalf("request counts contradict the status: ok=%d fail=%d err=%d",
			res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
	// No Expected: it is not a SUT mismatch, and an expectation nothing was measured against is noise.
	if res.Failure == nil {
		t.Fatal("an errored scenario must still carry an observed reason")
	}
	if res.Failure.Expected != nil {
		t.Errorf("errored scenario carries Expected=%q — nothing was measured against it", *res.Failure.Expected)
	}
}

// A scenario that never fired a request (no resolvable endpoint) must NOT be promoted: there is no
// evidence of unreachability, and inventing some is the same mistake inverted. It stays a plain
// preflight failure.
func TestRunMCPScenario_PreflightFailureIsNotPromoted(t *testing.T) {
	os.Unsetenv("MCP_URL")
	res := runMCPScenario(mcpCfg(t, ""), scenario.Parse(mcpScenarioMD()), "tr-pf")
	if res.ReqError != 0 {
		t.Fatalf("preflight fired requests? ok=%d fail=%d err=%d", res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
	if res.Status == report.StatusError {
		t.Fatal("a scenario that fired NO requests was promoted to errored on no evidence")
	}
}

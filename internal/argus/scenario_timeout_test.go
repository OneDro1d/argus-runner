package argus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-TO-RUN — a scenario's declared `## TIMEOUT` must be ENFORCED at run time, not merely
// validated at authoring time (P1 #12: "no run-time reader found beyond parse.go/sections.go").
//
// mcpTimeoutScenarioMD is mcpScenarioMD()'s twin with an explicit, SHORT `## TIMEOUT` — the
// fixture that lets this test declare a budget the fake SUT is made to miss.
func mcpTimeoutScenarioMD(timeout string) string {
	tick := "\x60"
	return strings.Join([]string{
		"# Scenario: MCP slow tool", "",
		"## Metadata",
		"- **ID**: MCP-TO-001",
		"- **Layer**: HTTP Ingestion",
		"- **Tags**: mcp, critical", "",
		"## TRIGGER",
		"POST " + tick + "${MCP_URL}" + tick, "",
		tick + tick + tick + "json",
		`{"transport":"streamable-http","tool":"slow_tool","args":{"x":1}}`,
		tick + tick + tick, "",
		"## EXPECT",
		"### Runnable",
		"- result.isError == false", "",
		"## TIMEOUT",
		timeout, "",
	}, "\n")
}

// fakeSlowMCPServer answers `initialize` immediately (so the test proves the TOOLS/CALL leg is
// the one that misses the budget, not the handshake) and then sleeps past the declared TIMEOUT
// before answering `tools/call` — a real SUT that is simply too slow, exactly the case P1 #12
// asks for: "a local httptest server that sleeps".
func fakeSlowMCPServer(sleep time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := ""
		switch {
		case strings.Contains(string(body), `"initialize"`):
			method = "initialize"
		case strings.Contains(string(body), `"notifications/initialized"`):
			method = "notifications/initialized"
		case strings.Contains(string(body), `"tools/call"`):
			method = "tools/call"
		}
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			time.Sleep(sleep) // the SUT is simply too slow — never a connection failure
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}],"isError":false}}`))
		}
	}))
}

// TestRunMCPScenario_EnforcesDeclaredTimeout is the RED/GREEN proof for P1 #12 on the MCP path.
//
// BEFORE the fix (mcp_scenario.go set `cl.Timeout: mcpT.TimeoutOrDefault()`, ignoring the
// scenario's own `## TIMEOUT` entirely): this scenario declares TIMEOUT 300ms against a SUT that
// answers in 2s, well inside the 30s target-level default — the call SUCCEEDS, the scenario
// PASSES, and 2s > the scenario's own declared 300ms budget goes unmeasured. That is "a scenario
// outliving its TIMEOUT": the run takes ~2s and reports `passed`.
//
// AFTER the fix: the call is cut at ~300ms (min of the scenario's TIMEOUT and the target's
// configured timeout, mcp_scenario.go's `callTimeout`), the scenario is `failed`, and Observed
// names both the elapsed time and the declared TIMEOUT.
func TestRunMCPScenario_EnforcesDeclaredTimeout(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := fakeSlowMCPServer(2 * time.Second)
	defer ts.Close()

	c := mcpCfg(t, ts.URL)
	s := scenario.Parse(mcpTimeoutScenarioMD("300ms"))
	if s.Timeout != "300ms" {
		t.Fatalf("parser did not pick up the declared TIMEOUT: got %q", s.Timeout)
	}

	start := time.Now()
	res := runMCPScenario(c, s, "tr-to-1")
	elapsed := time.Since(start)

	if elapsed >= time.Second {
		t.Fatalf("scenario ran for %s — the declared 300ms ## TIMEOUT was NOT enforced (the call rode "+
			"the SUT's real 2s, exactly the pre-fix defect: a scenario outliving its TIMEOUT)", elapsed)
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want %q (a SUT that answers too late is a SUT-blamed failure, not `passed` "+
			"and not `error`) — Observed: %s", res.Status, "failed", failureObserved(res))
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "timed out") ||
		!strings.Contains(res.Failure.Observed, "300ms") {
		t.Fatalf("Observed = %q, want it to name both the timeout and the declared 300ms budget", failureObserved(res))
	}
	t.Logf("GREEN: elapsed=%s status=%s observed=%q", elapsed, res.Status, failureObserved(res))
}

func failureObserved(res report.ScenarioResult) string {
	if res.Failure == nil {
		return "<nil>"
	}
	return res.Failure.Observed
}

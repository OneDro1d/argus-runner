package argus

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// TestRunChainScenario_LegitimatelySlowStepsDoNotSumPastTheCeiling is EVIDENCE for item 3 (the
// chain whole-wall-clock budget): a chain with several steps, EACH legitimately near its own
// per-step budget (a slow-but-healthy SUT — nothing hung, nothing unreachable), must not be killed
// because their SUM exceeds the scenario's declared `## TIMEOUT`. `## TIMEOUT` is capped at a FLAT
// 120s for every `chain`-tagged scenario regardless of step count (sections.go's timeoutCeiling) —
// before this fix, chain.Run's budget WAS that raw value, so a chain of 6+ steps each answering in,
// say, 150ms could still sum past a very short declared TIMEOUT and be cut off mid-chain, exactly
// the JMeter-whole-process regression one layer up.
//
// This fixture uses a short TIMEOUT (200ms) so the test runs fast: each step's own call is well
// inside 200ms (150ms), but 6 steps × 150ms (900ms) would already exceed a 200ms WHOLE-CHAIN
// deadline (the pre-fix behaviour). The fix scales the backstop by step count
// (runChainScenarioWithMoneyWrites' `backstop := s.TimeoutDuration() * len(steps)`), so 6×200ms =
// 1.2s comfortably covers the real ~900ms the chain takes.
func TestRunChainScenario_LegitimatelySlowStepsDoNotSumPastTheCeiling(t *testing.T) {
	os.Unsetenv("MCP_URL")
	const perCallSleep = 100 * time.Millisecond
	// Sleep ONLY on tools/call (the one leg that models "the SUT itself is slow-but-healthy") —
	// initialize/notifications/initialized stay instant, so each step's real cost is a predictable,
	// single perCallSleep rather than 2-3x it (handshake overhead would otherwise make the margin
	// against the backstop unpredictably tight).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if strings.Contains(string(body), `"tools/call"`) {
			time.Sleep(perCallSleep)
		}
		fakeMCPServer()(w, &http.Request{Method: r.Method, Header: r.Header, Body: io.NopCloser(strings.NewReader(string(body)))})
	}))
	defer ts.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  name: t\ntargets:\n  mcp:\n    base_url: "+ts.URL+"\n    timeout_seconds: 5\n"), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	const nSteps = 6
	payload := `{"steps":[`
	for i := 0; i < nSteps; i++ {
		if i > 0 {
			payload += ","
		}
		payload += `{"type":"mcp","name":"s` + string(rune('1'+i)) + `","tool":"ok_tool","args":{},"expect":"result.isError == false"}`
	}
	payload += `]}`

	s := &scenario.Scenario{ID: "CHAIN-SLOW", Tags: []string{"chain"}, Timeout: "300ms"}
	s.Trigger.Payload = payload

	start := time.Now()
	res := runChainScenario(c, s, "tr-chain-slow", "testkit/ui")
	elapsed := time.Since(start)

	if res.Status != "passed" {
		var notMeasured int
		for _, st := range res.Steps {
			if st.Status == "not-measured" {
				notMeasured++
			}
		}
		t.Fatalf("status=%q (want passed) after %s — %d/%d steps not-measured; each step's own call "+
			"(%s) was legitimately within budget, only their SUM (%s) exceeded the flat per-chain "+
			"TIMEOUT ceiling (300ms) the old whole-chain-deadline design used verbatim: %+v",
			res.Status, elapsed, notMeasured, len(res.Steps), perCallSleep, time.Duration(nSteps)*perCallSleep, res.Failure)
	}
	if len(res.Steps) != nSteps {
		t.Fatalf("got %d steps, want %d — some steps never fired", len(res.Steps), nSteps)
	}
	for _, st := range res.Steps {
		if st.Status != "passed" {
			t.Errorf("step %s status=%q, want passed", st.Name, st.Status)
		}
	}
	t.Logf("GREEN: %d steps in %s, each step capped at 300ms (its own per-step TIMEOUT), chain backstop 1.8s", nSteps, elapsed)
}

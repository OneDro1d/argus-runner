package argus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// throttlingMCPServer answers `tools/call` with the SUT's declared throttle for the FIRST
// `throttleFirst` calls and normally afterwards. It counts the tool calls so a test can prove the
// scenario was re-fired EXACTLY once.
type throttlingMCPServer struct {
	mu            sync.Mutex
	calls         int
	throttleFirst int  // -1 = always throttle
	retryAfter    int  // seconds put in the refusal body
	omitRetry     bool // answer without a retry_after field at all
}

func (s *throttlingMCPServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			s.mu.Lock()
			s.calls++
			n := s.calls
			s.mu.Unlock()
			throttle := s.throttleFirst < 0 || n <= s.throttleFirst
			text := `{"ok":true}`
			isErr := false
			if throttle {
				isErr = true
				if s.omitRetry {
					text = `{"error":"rate_limited"}`
				} else {
					text = fmt.Sprintf(`{"error":"rate_limited","retry_after":%d}`, s.retryAfter)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isErr}})
		}
	}
}

func (s *throttlingMCPServer) callCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// rlCfg loads a config with an mcp target and a rate_limit block (pauseYAML may be "").
func rlCfg(t *testing.T, baseURL, pauseYAML string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: t\ntargets:\n  mcp:\n    base_url: " + baseURL + "\n" +
		"rate_limit:\n  requests: 120\n  per: minute\n  signature:\n    body_contains: '\"error\":\"rate_limited\"'\n    retry_after_field: retry_after\n" + pauseYAML
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// writeMCPScenario drops one mcp-tagged scenario into the scenarios dir.
func writeMCPScenario(t *testing.T, dir, id string) {
	t.Helper()
	d := filepath.Join(dir, "http-ingestion")
	_ = os.MkdirAll(d, 0o755)
	md := strings.Replace(mcpScenarioMD(), "- **ID**: MCP-001", "- **ID**: "+id, 1)
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeSleep swaps the run loop's wall-clock pause for a recorder, so a cap test does not have to
// wait 48 real seconds to prove the run waited 48 seconds.
func fakeSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	prev := rateLimitSleep
	rateLimitSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { rateLimitSleep = prev })
	return &slept
}

// VR10-R1-2 / VR10-R1-6: a throttled mcp scenario is `errored` (grey — NOT MEASURED), never
// `failed`, and its observed line names the rate limit and the retry-after value.
func TestRunMCPScenario_ThrottleIsErroredNotFailed(t *testing.T) {
	os.Unsetenv("MCP_URL")
	srv := &throttlingMCPServer{throttleFirst: -1, retryAfter: 48}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	res := runMCPScenario(rlCfg(t, ts.URL, ""), scenario.Parse(mcpScenarioMD()), "tr-rl")
	if res.Status != report.StatusError {
		t.Fatalf("status = %q; want %q — a rate limit is NOT MEASURED, never a product failure", res.Status, report.StatusError)
	}
	if !res.RateLimited {
		t.Fatal("the scenario is not marked rate-limited, so the run loop can never pause for it")
	}
	want := "SUT rate-limited this request (retry_after 48s) — the scenario was not measured"
	if res.Failure == nil || res.Failure.Observed != want {
		t.Fatalf("observed = %+v; want %q", res.Failure, want)
	}
	if res.Failure.Expected != nil {
		t.Errorf("a not-measured scenario carries Expected=%q — nothing was measured against it", *res.Failure.Expected)
	}
	if res.RetryAfterMs != 48000 {
		t.Errorf("RetryAfterMs = %d; want 48000 (the SUT's own retry_after)", res.RetryAfterMs)
	}
}

// VR10-R1-3: with NO rate_limit block the very same refusal is judged exactly as today — `failed`.
func TestRunMCPScenario_UndeclaredThrottleStaysFailed(t *testing.T) {
	os.Unsetenv("MCP_URL")
	srv := &throttlingMCPServer{throttleFirst: -1, retryAfter: 48}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	res := runMCPScenario(mcpCfg(t, ts.URL), scenario.Parse(mcpScenarioMD()), "tr-nd")
	if res.Status != "failed" || res.RateLimited {
		t.Fatalf("an UNDECLARED SUT was reclassified on a heuristic: status=%q rateLimited=%v", res.Status, res.RateLimited)
	}
}

// THE HEADLINE (owner D8, VR10-R1-7): a throttle pauses the run BETWEEN scenarios for the SUT's
// retry_after, retries the scenario ONCE, and the run continues — so what comes after a throttle is
// measured instead of poisoned.
func TestRunAll_RateLimitPausesOnceAndRetriesOnce(t *testing.T) {
	os.Unsetenv("MCP_URL")
	slept := fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: 1, retryAfter: 48} // the first call only
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, scDir, "MCP-001")
	writeMCPScenario(t, scDir, "MCP-002")

	rr, err := RunAll(rlCfg(t, ts.URL, ""), scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 48*time.Second {
		t.Fatalf("pauses = %v; want exactly one 48s pause (the SUT's own retry_after)", *slept)
	}
	if srv.callCount() != 3 {
		t.Fatalf("tool calls = %d; want 3 (MCP-001 throttled, MCP-001 retried once, MCP-002)", srv.callCount())
	}
	s := rr.Report.Summary
	if s.RateLimitPauses != 1 {
		t.Errorf("Summary.RateLimitPauses = %d; want 1", s.RateLimitPauses)
	}
	if s.RateLimitedScenarios != 0 {
		t.Errorf("Summary.RateLimitedScenarios = %d; want 0 — the retry MEASURED it", s.RateLimitedScenarios)
	}
	if s.Passed != 2 || s.Errored != 0 || s.Failed != 0 {
		t.Errorf("summary = %+v; want both scenarios measured and green", s)
	}
	one, _ := rr.Report.Find("MCP-001")
	if one == nil || one.Status != "passed" {
		t.Fatalf("MCP-001 = %+v; the RETRY's outcome is the row's status (SA R1-a)", one)
	}
	if one.RateLimitedRetries != 1 || one.PausedMs != 48000 {
		t.Errorf("MCP-001 must record the retry beside the status: retries=%d paused_ms=%d", one.RateLimitedRetries, one.PausedMs)
	}
	if one.RateLimited {
		t.Error("MCP-001 was measured on its retry; it must not still be flagged rate-limited")
	}
}

// SA 0.14 R1-a: throttled AGAIN on the one retry -> `errored` ("rate-limited twice"), and NO
// further retry.
func TestRunAll_ThrottledTwiceIsErroredAndNotRetriedAgain(t *testing.T) {
	os.Unsetenv("MCP_URL")
	slept := fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: -1, retryAfter: 10}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, scDir, "MCP-001")

	rr, err := RunAll(rlCfg(t, ts.URL, ""), scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if srv.callCount() != 2 {
		t.Fatalf("tool calls = %d; want 2 (the original + exactly ONE retry)", srv.callCount())
	}
	if len(*slept) != 1 {
		t.Fatalf("pauses = %v; want exactly 1", *slept)
	}
	one, _ := rr.Report.Find("MCP-001")
	if one == nil || one.Status != report.StatusError || !one.RateLimited {
		t.Fatalf("MCP-001 = %+v; want errored + rate-limited", one)
	}
	if one.Failure == nil || !strings.Contains(one.Failure.Observed, "twice") {
		t.Errorf("observed must say it was rate-limited twice: %+v", one.Failure)
	}
	if rr.Report.Summary.RateLimitedScenarios != 1 || rr.Report.Summary.Errored != 1 || rr.Report.Summary.Failed != 0 {
		t.Errorf("summary = %+v; want 1 rate-limited, 1 errored, 0 failed", rr.Report.Summary)
	}
}

// VR10-R1-8: once max_pauses is reached the run STOPS pausing, classifies the rest as they come,
// and the summary says the cap was hit. Waiting more is not the answer — splitting the pack is.
func TestRunAll_PauseCapStopsPausingAndSaysSo(t *testing.T) {
	os.Unsetenv("MCP_URL")
	slept := fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: -1, retryAfter: 5}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	for _, id := range []string{"MCP-001", "MCP-002", "MCP-003"} {
		writeMCPScenario(t, scDir, id)
	}
	c := rlCfg(t, ts.URL, "  pause:\n    max_pauses: 1\n    max_total_wait: 60s\n")

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 {
		t.Fatalf("pauses = %v; want exactly 1 — max_pauses: 1 was declared", *slept)
	}
	s := rr.Report.Summary
	if s.RateLimitPauses != 1 || !s.RateLimitCapHit {
		t.Fatalf("summary = %+v; want 1 pause and the cap flagged", s)
	}
	if !strings.Contains(s.Note, "rate-limit pause cap reached (1/60s)") || !strings.Contains(s.Note, "split the pack") {
		t.Errorf("Summary.Note = %q; it must name the cap that was reached and the fix", s.Note)
	}
	if s.RateLimitedScenarios != 3 || s.Errored != 3 || s.Failed != 0 {
		t.Errorf("summary = %+v; the remaining scenarios must be classified as they come (errored), never failed", s)
	}
}

// VR10-R1-8 / owner D3: the pause never exceeds what is left of max_total_wait — when the wait does
// not fit the budget we do not wait for it.
func TestRunAll_PauseIsClampedToTheRemainingBudget(t *testing.T) {
	os.Unsetenv("MCP_URL")
	slept := fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: 1, retryAfter: 48}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, scDir, "MCP-001")
	c := rlCfg(t, ts.URL, "  pause:\n    max_total_wait: 5s\n")

	if _, err := RunAll(c, scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{}); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != 5*time.Second {
		t.Fatalf("pauses = %v; want one pause clamped to the 5s budget", *slept)
	}
}

// SA 0.14 R1-h: no retry_after in the refusal -> pause for the DECLARED window (per: minute = 60s).
func TestRunAll_NoRetryAfterPausesForTheDeclaredWindow(t *testing.T) {
	os.Unsetenv("MCP_URL")
	slept := fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: 1, omitRetry: true}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, scDir, "MCP-001")

	if _, err := RunAll(rlCfg(t, ts.URL, ""), scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{}); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] != time.Minute {
		t.Fatalf("pauses = %v; want one 60s pause (per: minute), the SUT gave no retry_after", *slept)
	}
}

// VR10-R1-3 / VR10-R1-9: an UNDECLARED SUT never pauses, and its summary carries none of the new
// fields — an old report stays byte-identical.
func TestRunAll_UndeclaredSUTNeverPausesAndTheSummaryIsUnchanged(t *testing.T) {
	os.Unsetenv("MCP_URL")
	slept := fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: -1, retryAfter: 48}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, scDir, "MCP-001")

	rr, err := RunAll(mcpCfg(t, ts.URL), scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 0 {
		t.Fatalf("an undeclared SUT paused the run: %v", *slept)
	}
	b, _ := json.Marshal(rr.Report.Summary)
	for _, k := range []string{"rate_limit_pauses", "rate_limited_scenarios", "rate_limit_cap_hit", "note"} {
		if strings.Contains(string(b), k) {
			t.Errorf("summary JSON carries %q on a run that never saw a rate limit: %s", k, b)
		}
	}
}

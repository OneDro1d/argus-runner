package argus

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// CHANGE-1: a chain MCP step with an unresolved ${MCP_URL} (no env) must fall back to the
// argus-config targets.mcp.base_url — the same resolution the single-mcp path uses.
func TestRunChainScenario_MCPStepResolvesFromConfig(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPServer()) // shared with mcp_scenario_test (package argus)
	defer ts.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  name: t\ntargets:\n  mcp:\n    base_url: "+ts.URL+"\n    transport: streamable-http\n"), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s := &scenario.Scenario{ID: "CHAIN-CFG", Tags: []string{"chain"}}
	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"s1","server_url":"${MCP_URL}","tool":"ok_tool","args":{},"expect":"result.isError == false"}]}`
	res := runChainScenario(c, s, "tr-chain-cfg", "testkit/ui")
	if res.Status != "passed" || len(res.Steps) != 1 || res.Steps[0].Status != "passed" {
		t.Fatalf("chain mcp step must resolve endpoint from targets.mcp.base_url and pass: %+v", res)
	}
}

// Patch #3: runChainScenario must record DurationMs (chain.Run doesn't), so a chain feeds
// argus_scenario_duration_seconds (the Slowest-scenarios panel) like any scenario.
func TestRunChainScenario_RecordsDuration(t *testing.T) {
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(8 * time.Millisecond) // measurable end-to-end duration
		fakeMCPServer()(w, r)
	}))
	defer ts.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	os.WriteFile(cfgPath, []byte("project:\n  name: t\ntargets:\n  mcp:\n    base_url: "+ts.URL+"\n"), 0o644)
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	s := &scenario.Scenario{ID: "CHAIN-DUR", Tags: []string{"chain"}}
	s.Trigger.Payload = `{"steps":[{"type":"mcp","name":"s1","tool":"ok_tool","args":{},"expect":"result.isError == false"}]}`
	res := runChainScenario(c, s, "tr-chain-dur", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("expected pass: %+v", res)
	}
	if res.DurationMs < 1 {
		t.Errorf("chain scenario must record a non-zero DurationMs (Slowest panel feed), got %d", res.DurationMs)
	}
}

// A multi-step chain threads ONE cid across steps, records per-step status, and on a
// mid-chain break STOPS — the failing step is named, later steps are "skipped" (never
// falsely green), and the cid is preserved on every step (UC-32/63 / VR-K1..K5). Uses
// ui-only steps via the injectable uiRun so no network is needed.
func TestRunChainScenario_PartialFailure(t *testing.T) {
	orig := uiRun
	t.Cleanup(func() { uiRun = orig })
	// pass unless the spec name contains "fail".
	uiRun = func(vendorDir string, specs []string, appURL, cid string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		if strings.Contains(strings.Join(specs, " "), "fail") {
			return 1, true, ""
		}
		return 0, true, ""
	}

	s := &scenario.Scenario{ID: "CHAIN-001", Tags: []string{"chain"}}
	s.Trigger.Payload = `{"steps":[
	  {"type":"ui","name":"step1-load","spec":"tests/live/ok.spec.ts"},
	  {"type":"ui","name":"step2-fail","spec":"tests/live/fail.spec.ts"},
	  {"type":"ui","name":"step3-after","spec":"tests/live/ok.spec.ts"}
	]}`
	res := runChainScenario(&config.Config{}, s, "tr-chain-1", "testkit/ui")

	if res.Status != "failed" {
		t.Fatalf("a mid-chain break must be failed, got %q", res.Status)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("want 3 step records, got %d", len(res.Steps))
	}
	// ⛔ UPDATED BY V30-001: the third step used to be `skipped` — never run. It runs now, and it
	// may never be recorded green (rule 4): a positive claim after a break is not trustworthy.
	wantStatus := []string{"passed", "failed", report.StepRanAfterFailureOK}
	for i, w := range wantStatus {
		if res.Steps[i].Status != w {
			t.Errorf("step %d status = %q; want %q", i, res.Steps[i].Status, w)
		}
		if res.Steps[i].CorrelationID != "tr-chain-1" {
			t.Errorf("step %d cid not threaded: %q", i, res.Steps[i].CorrelationID)
		}
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "step2-fail") {
		t.Errorf("partial-failure must name the failing step, got %+v", res.Failure)
	}
}

// Test-requests panel: one request per EXECUTED step; a SKIPPED step (after a mid-chain
// break) fired none and is NOT counted (argus_sut_requests_total).
func TestRunChainScenario_RequestCounts(t *testing.T) {
	orig := uiRun
	t.Cleanup(func() { uiRun = orig })
	uiRun = func(vendorDir string, specs []string, appURL, cid string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		if strings.Contains(strings.Join(specs, " "), "fail") {
			return 1, true, ""
		}
		return 0, true, ""
	}
	// all-pass: 2 executed steps → ReqSuccess=2, the rest 0, with 2 timestamped samples.
	pass := &scenario.Scenario{ID: "CHAIN-RC1", Tags: []string{"chain"}}
	pass.Trigger.Payload = `{"steps":[{"type":"ui","name":"a","spec":"tests/live/a.spec.ts"},{"type":"ui","name":"b","spec":"tests/live/b.spec.ts"}]}`
	if r := runChainScenario(&config.Config{}, pass, "tr-rc1", "testkit/ui"); r.ReqSuccess != 2 || r.ReqFailed != 0 || r.ReqError != 0 || len(r.Requests) != 2 {
		t.Errorf("all-pass chain (2 steps) → success=2/failed=0/error=0 + 2 samples, got %d/%d/%d (%d samples)", r.ReqSuccess, r.ReqFailed, r.ReqError, len(r.Requests))
	}
	// partial: step1 pass, step2 FAIL (exit!=0 WITH artifact → a negative response, not a harness
	// error), step3 RUNS AFTER THE BREAK and answers positively.
	//
	// ⛔ UPDATED BY V30-001. Step 3 used to be skipped and fired nothing; it now fires, so the
	// REQUEST panel counts it as the success it was. The panel counts REQUESTS, not verdicts —
	// conflating them would make the request distribution lie about what the SUT answered, while
	// the step's own status (`ran-after-failure: ok`) carries the verdict truth.
	part := &scenario.Scenario{ID: "CHAIN-RC2", Tags: []string{"chain"}}
	part.Trigger.Payload = `{"steps":[{"type":"ui","name":"s1","spec":"tests/live/ok.spec.ts"},{"type":"ui","name":"s2-fail","spec":"tests/live/fail.spec.ts"},{"type":"ui","name":"s3","spec":"tests/live/ok.spec.ts"}]}`
	if r := runChainScenario(&config.Config{}, part, "tr-rc2", "testkit/ui"); r.ReqSuccess != 2 || r.ReqFailed != 1 || r.ReqError != 0 {
		t.Errorf("partial chain (pass,fail,ran-after-failure:ok) → success=2/failed=1/error=0, got success=%d failed=%d error=%d", r.ReqSuccess, r.ReqFailed, r.ReqError)
	}
}

func TestRunChainScenario_AllPass(t *testing.T) {
	orig := uiRun
	t.Cleanup(func() { uiRun = orig })
	uiRun = func(vendorDir string, specs []string, appURL, cid string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		return 0, true, ""
	}

	s := &scenario.Scenario{ID: "CHAIN-002", Tags: []string{"chain"}}
	s.Trigger.Payload = `{"steps":[
	  {"type":"ui","name":"a","spec":"tests/live/a.spec.ts"},
	  {"type":"ui","name":"b","spec":"tests/live/b.spec.ts"}
	]}`
	res := runChainScenario(&config.Config{}, s, "tr-chain-2", "testkit/ui")
	if res.Status != "passed" || len(res.Steps) != 2 {
		t.Fatalf("all-pass chain: status=%q steps=%d", res.Status, len(res.Steps))
	}
	for _, st := range res.Steps {
		if st.Status != "passed" || st.CorrelationID != "tr-chain-2" {
			t.Errorf("step %+v not passed/cid-threaded", st)
		}
	}
}

func TestRunChainScenario_Preflight(t *testing.T) {
	s := &scenario.Scenario{ID: "CHAIN-003", Tags: []string{"chain"}}
	s.Trigger.Payload = `{"steps":[{"type":"bogus","name":"x"}]}`
	if res := runChainScenario(&config.Config{}, s, "tr-x", "testkit/ui"); res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "unknown chain step type") {
		t.Fatalf("unknown step type must preflight-fail, got %q %+v", res.Status, res.Failure)
	}
	s.Trigger.Payload = `{"steps":[]}`
	if res := runChainScenario(&config.Config{}, s, "tr-x", "testkit/ui"); res.Status != "failed" || res.Failure == nil {
		t.Fatalf("empty steps must fail, got %q", res.Status)
	}
}

package chain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
)

func pass() func(string, map[string]string) report.StepResult {
	return func(string, map[string]string) report.StepResult {
		return report.StepResult{Status: "passed", Observed: "ok"}
	}
}
func fail(msg string) func(string, map[string]string) report.StepResult {
	return func(string, map[string]string) report.StepResult {
		return report.StepResult{Status: "failed", Observed: msg}
	}
}

func TestRun_AllPass(t *testing.T) {
	res := Run("tr-1", []Step{{Name: "mcp", Run: pass()}, {Name: "db", Run: pass()}})
	if res.Status != "passed" || len(res.Steps) != 2 {
		t.Fatalf("all-pass chain should pass with 2 steps: %+v", res)
	}
}

// VR-K5 / UC-63: a mid-chain failure stops the chain — the LATER step is marked
// skipped (never falsely green) and is NOT executed; the failing step is named.
func TestRun_MidFailContinuesButNeverGreen(t *testing.T) {
	ran3 := false
	steps := []Step{
		{Name: "mcp", Run: pass()},
		{Name: "db", Run: fail("0 rows for the correlation id")},
		{Name: "ui", Run: func(string, map[string]string) report.StepResult {
			ran3 = true
			return report.StepResult{Status: "passed"}
		}},
	}
	res := Run("tr-2", steps)
	if res.Status != "failed" {
		t.Fatalf("mid-chain failure must be RED: %+v", res)
	}
	// ⛔ UPDATED BY V30-001. This case used to assert `!ran3` and `Steps[2].Status == "skipped"`.
	// The rule it encoded was POSITIONAL, not causal — and the steps it discarded are the ones
	// that undo what the test created. Measured on the live RACE pack: of six runs, the only two
	// that left a namespace behind were the two whose chain broke.
	if !ran3 {
		t.Fatal("the step after a failure MUST now be executed (VR12-CH1 rule 2) — the cleanup " +
			"steps that undo what the test created were exactly the ones being discarded")
	}
	if len(res.Steps) != 3 {
		t.Fatalf("every step must be recorded: %+v", res.Steps)
	}
	// …and it may NEVER be recorded green. Once a step has failed the SUT is in an unknown state,
	// so a later step's positive claim is not trustworthy (rule 4).
	if res.Steps[2].Status != report.StepRanAfterFailureOK {
		t.Fatalf("a step that ran after the break must be %q, never passed: %+v",
			report.StepRanAfterFailureOK, res.Steps[2])
	}
	if !res.Steps[2].RanAfterFailure {
		t.Error("RanAfterFailure must be set, so a machine consumer need not parse the status string")
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "'db'") {
		t.Fatalf("the failing step must be named: %+v", res.Failure)
	}
	// Rule 7: nothing was left un-attempted here — every step ran to a real verdict — so there is
	// no residue line. The line must mean something when it appears.
	if res.Residue != "" {
		t.Errorf("no step was left un-attempted, so nothing may claim residue: %q", res.Residue)
	}
}

// UC-63: step1 PASS / step2 PASS / step3 FAIL → partial-failure, failing step named, all green-before-fail.
func TestRun_LastFailNamesStep(t *testing.T) {
	res := Run("tr-3", []Step{{Name: "mcp", Run: pass()}, {Name: "db", Run: pass()}, {Name: "ui", Run: fail("DOM mismatch")}})
	if res.Status != "failed" || res.Steps[0].Status != "passed" || res.Steps[1].Status != "passed" || res.Steps[2].Status != "failed" {
		t.Fatalf("expected pass/pass/fail per-step: %+v", res.Steps)
	}
	if !strings.Contains(res.Failure.Observed, "'ui'") {
		t.Fatalf("failing step 'ui' must be named: %+v", res.Failure)
	}
}

// VR-K5: the shared correlation id is preserved on EVERY step record (incl. skipped).
func TestRun_CorrelationPreservedAcrossAllSteps(t *testing.T) {
	res := Run("tr-keep", []Step{{Name: "a", Run: pass()}, {Name: "b", Run: fail("x")}, {Name: "c", Run: pass()}})
	for _, s := range res.Steps {
		if s.CorrelationID != "tr-keep" {
			t.Fatalf("step %q lost the correlation id: %+v", s.Name, s)
		}
	}
}

// Test-requests panel (r3): chain.Run records one timestamped RequestSample per EXECUTED step,
// mapped 3-way (passed→success, error→error, failed→failed); a SKIPPED step (after a break)
// records nothing.
func TestRun_RecordsRequestSamples(t *testing.T) {
	errStep := func(string, map[string]string) report.StepResult {
		return report.StepResult{Status: report.StatusError, Observed: "harness died"}
	}
	// passed, error → then a third step is SKIPPED (the error stops the chain).
	res := Run("tr-rs", []Step{{Name: "a", Run: pass()}, {Name: "b", Run: errStep}, {Name: "c", Run: pass()}})
	if res.ReqSuccess != 1 || res.ReqError != 1 || res.ReqFailed != 0 {
		t.Errorf("executed pass+error → success=1/error=1/failed=0, got %d/%d/%d", res.ReqSuccess, res.ReqError, res.ReqFailed)
	}
	if len(res.Requests) != 2 {
		t.Fatalf("only the 2 EXECUTED steps fire requests (skipped fires none), got %d samples", len(res.Requests))
	}
	if res.Requests[0].Outcome != report.OutcomeSuccess || res.Requests[1].Outcome != report.OutcomeError || res.Requests[1].AtMs == 0 {
		t.Errorf("per-step samples mis-mapped/un-stamped: %+v", res.Requests)
	}
	// a failed (negative-response) step records `failed`, not `error`.
	res2 := Run("tr-rs2", []Step{{Name: "x", Run: fail("0 rows")}})
	if res2.ReqFailed != 1 || res2.ReqError != 0 || res2.ReqSuccess != 0 {
		t.Errorf("a failed step → failed=1, got success=%d failed=%d error=%d", res2.ReqSuccess, res2.ReqFailed, res2.ReqError)
	}
}

// r3 gate-fix: an MCP step whose endpoint is UNREACHABLE (no response) must record an EXECUTION
// error, not a negative response — matching the standalone MCP path (mcpRequestOutcome).
//
// ⛔ UPDATED BY V30-001 / VR12-CH2. The REQUEST accounting is unchanged (it asked and got no
// answer: error=1). The STEP is now `not-measured` rather than `error`, because that is what it
// is — the SUT would not answer, so this step was not tested. `error` stays the SCENARIO's status.
func TestMCPStep_UnreachableIsError(t *testing.T) {
	cl := &mcp.Client{ServerURL: "http://127.0.0.1:1/mcp", Transport: mcp.Streamable, Token: "x"}
	res := Run("tr-unreach", []Step{MCPStep("mcp", cl, "do", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil)})
	if res.ReqError != 1 || res.ReqFailed != 0 || res.ReqSuccess != 0 {
		t.Errorf("an unreachable MCP step must record error=1 (no response), got success=%d failed=%d error=%d", res.ReqSuccess, res.ReqFailed, res.ReqError)
	}
	if res.Steps[0].Status != report.StepNotMeasured {
		t.Errorf("the unreachable step status must be %q, got %q", report.StepNotMeasured, res.Steps[0].Status)
	}
	if !strings.Contains(res.Steps[0].Observed, "unreachable") {
		t.Errorf("the reason belongs in the step's text, not in a second status name: %q", res.Steps[0].Observed)
	}
	if res.Status != report.StatusError {
		t.Errorf("the SCENARIO is %q — not measured, as opposed to measured and wrong; got %q",
			report.StatusError, res.Status)
	}
}

func fakeStreamable() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": `{"ok":true}`}}, "isError": false}})
		}
	}
}

// UC-31 (MCP→DB success shape): a native-MCP first step passes, then a downstream
// (here faked) DB step keyed on the SAME correlation id passes → chain GREEN.
func TestMCPStep_ThenDownstream(t *testing.T) {
	ts := httptest.NewServer(fakeStreamable())
	defer ts.Close()
	cl := &mcp.Client{ServerURL: ts.URL + "/mcp", Transport: mcp.Streamable, Token: "x"}

	var seenCID string
	dbStep := Step{Name: "db", Run: func(cid string, _ map[string]string) report.StepResult {
		seenCID = cid // a real DB step would SELECT ... WHERE request_id = cid
		return report.StepResult{Status: "passed", Observed: "1 row"}
	}}
	res := Run("tr-chain", []Step{
		MCPStep("mcp", cl, "do_thing", `{"x":1}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
		dbStep,
	})
	if res.Status != "passed" {
		t.Fatalf("MCP→DB chain should pass: %+v", res)
	}
	if seenCID != "tr-chain" {
		t.Fatalf("the DB step must receive the SAME correlation id as the MCP step, got %q", seenCID)
	}
}

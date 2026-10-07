package chain

// ⚠ VR12-E8 (V29-017): these fixtures moved from the scalar mcp.Expect.BodyContains /
// BodyMatches to Expect.Body, because EVERY declared assertion is now bound, judged and recorded
// -- the scalars kept only the first of each kind. The behaviour each case asserts is unchanged:
// ${saved.x} still binds per execution, an unbound variable still fails by name, and a re-run still
// must not see the previous run id.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR10-S2 (V28-014): captured values bind into a step's CONTENT assertion with the same
// ${saved.<var>} grammar as its args, into a per-execution copy of mcp.Expect — so a chain can
// assert "the answer names THE document I just wrote", and a re-run never carries one run's id
// into the next run's assertion.

// mutableTextServer answers tools/call with whatever text is current — so one set of steps can be
// run twice against different answers (the re-run test).
type mutableTextServer struct {
	mu   sync.Mutex
	text string
}

func (m *mutableTextServer) set(text string) { m.mu.Lock(); m.text = text; m.mu.Unlock() }

func (m *mutableTextServer) handler() http.HandlerFunc {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			m.mu.Lock()
			text := m.text
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}})
		}
	}
}

// Acceptance 11 (the ${saved.…} half): write → read-back, asserting the answer names the id the
// write returned. Right answer passes; a different document's id goes red on the body plane.
func TestMCPStep_SavedValueBindsIntoTheBodyAssertion(t *testing.T) {
	writeSrv := fakeCaptureMCP(t, `{"id":"srv-generated-42"}`)
	defer writeSrv.Close()
	rightSrv := fakeCaptureMCP(t, `{"doc":{"id":"srv-generated-42"},"title":"t"}`)
	defer rightSrv.Close()
	wrongSrv := fakeCaptureMCP(t, `{"doc":{"id":"someone-elses-doc"},"title":"t"}`)
	defer wrongSrv.Close()

	steps := func(read *httptest.Server) []Step {
		return []Step{
			MCPStep("write-doc", &mcp.Client{ServerURL: writeSrv.URL, Transport: mcp.Streamable}, "memstore_write", `{"title":"t"}`,
				mcp.Expect{ErrorPlane: mcp.PlaneNone}, map[string]scenario.SaveSpec{"docId": {Path: "id"}}),
			MCPStep("read-back", &mcp.Client{ServerURL: read.URL, Transport: mcp.Streamable}, "memstore_read", `{"document_id":"${saved.docId}"}`,
				mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "${saved.docId}"}}}, nil),
		}
	}
	res := Run("tr-x", steps(rightSrv))
	if res.Status != "passed" {
		t.Fatalf("the answer names the written id — must pass: %s / %+v", res.Status, res.Failure)
	}
	if got := strings.Join(res.Steps[1].AssertionsEnforced, ";"); !strings.Contains(got, "srv-generated-42") || strings.Contains(got, "${saved") {
		t.Errorf("the enforced record must carry the BOUND value, got %q", got)
	}
	res = Run("tr-y", steps(wrongSrv))
	if res.Status != "failed" || res.Steps[1].Status != "failed" {
		t.Fatalf("an answer naming a different document must go red (failed, not error): %+v", res.Steps)
	}
	if strings.Contains(res.Steps[1].Observed, "srv-generated-42") {
		t.Errorf("observed must never echo the asserted value (VR-C8): %q", res.Steps[1].Observed)
	}
}

// VR10-S2-10 (owner-locked): an unbound variable in an assertion is an AUTHORING error — fail the
// step, name the variable, and NEVER compare the literal placeholder text (here the SUT's own
// answer contains the literal "${saved.missing}", which would make a literal compare pass).
func TestMCPStep_UnboundVariableInAssertionFailsNamingIt(t *testing.T) {
	srv := fakeCaptureMCP(t, `{"note":"${saved.missing}"}`)
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	res := Run("tr-x", []Step{MCPStep("read", cl, "t", `{}`,
		mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "${saved.missing}"}}}, nil)})
	if res.Status == "passed" {
		t.Fatal("the literal placeholder was compared against the answer — it must never be")
	}
	if obs := res.Steps[0].Observed; !strings.Contains(obs, "saved.missing") {
		t.Errorf("the failure must NAME the unbound variable, got: %s", obs)
	}
}

// Acceptance 12 / VR10-S2-11: the bound Expect is a per-execution copy. The same steps run twice
// against a SUT that mints a different id each time; if run 1's id had leaked into the step's
// Expect, run 2 would fail on the body plane.
func TestMCPStep_RerunDoesNotLeakTheFirstRunsIdIntoTheAssertion(t *testing.T) {
	write := &mutableTextServer{text: `{"id":"run1-id"}`}
	read := &mutableTextServer{text: `{"doc":{"id":"run1-id"}}`}
	ws, rs := httptest.NewServer(write.handler()), httptest.NewServer(read.handler())
	defer ws.Close()
	defer rs.Close()
	steps := []Step{
		MCPStep("write-doc", &mcp.Client{ServerURL: ws.URL, Transport: mcp.Streamable}, "w", `{}`,
			mcp.Expect{ErrorPlane: mcp.PlaneNone}, map[string]scenario.SaveSpec{"docId": {Path: "id"}}),
		MCPStep("read-back", &mcp.Client{ServerURL: rs.URL, Transport: mcp.Streamable}, "r", `{"document_id":"${saved.docId}"}`,
			mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "${saved.docId}"}}}, nil),
	}
	if r1 := Run("tr-1", steps); r1.Status != "passed" {
		t.Fatalf("run 1 must pass: %+v", r1.Failure)
	}
	write.set(`{"id":"run2-id"}`)
	read.set(`{"doc":{"id":"run2-id"}}`)
	r2 := Run("tr-2", steps)
	if r2.Status != "passed" {
		t.Fatalf("run 2 must judge against ITS OWN captured id, not run 1's: %+v", r2.Failure)
	}
	if got := strings.Join(r2.Steps[1].AssertionsEnforced, ";"); !strings.Contains(got, "run2-id") || strings.Contains(got, "run1-id") {
		t.Errorf("run 2's enforced record must carry run 2's id only, got %q", got)
	}
}

// VR10-S2-12 / acceptance 7: the report records, per step, which content assertions were ENFORCED
// (the text for the test hat; the count is what the product hat keeps).
func TestMCPStep_RecordsWhichAssertionsWereEnforced(t *testing.T) {
	srv := fakeCaptureMCP(t, `{"id":"abc"}`)
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	res := Run("tr-x", []Step{
		MCPStep("with", cl, "t", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "abc"}, {Op: mcp.BodyMatchesOp, Value: `"id"`}}}, nil),
		MCPStep("without", cl, "t", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
	})
	if res.Status != "passed" {
		t.Fatalf("both steps pass: %+v", res.Failure)
	}
	if len(res.Steps[0].AssertionsEnforced) != 2 || res.Steps[0].AssertionsEnforcedCount != 2 {
		t.Errorf("step 'with' enforced two content assertions, got %+v", res.Steps[0])
	}
	if len(res.Steps[1].AssertionsEnforced) != 0 || res.Steps[1].AssertionsEnforcedCount != 0 {
		t.Errorf("step 'without' enforced none, got %+v", res.Steps[1])
	}
}

// VR10-S2-5 + S2-e: a body miss is `failed` (a negative response, not an execution error), the
// chain stops there, and a failed assertion saves NOTHING.
func TestMCPStep_BodyMissIsFailedNotErrorAndSavesNothing(t *testing.T) {
	srv := fakeCaptureMCP(t, `{"results":[]}`)
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	res := Run("tr-x", []Step{MCPStep("search", cl, "t", `{}`,
		mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "argus-race"}}}, map[string]scenario.SaveSpec{"r": {Path: "results"}})})
	if res.Status != "failed" || res.Steps[0].Status != "failed" {
		t.Fatalf("a body miss is failed, not %q / %q", res.Status, res.Steps[0].Status)
	}
	if res.Steps[0].Status == report.StatusError || res.ReqError != 0 || res.ReqFailed != 1 {
		t.Errorf("a body miss is a negative RESPONSE: want failed=1 error=0, got failed=%d error=%d", res.ReqFailed, res.ReqError)
	}
	if len(res.Steps[0].Captured) != 0 {
		t.Errorf("a failed assertion saves nothing, got %v", res.Steps[0].Captured)
	}
}

// V31-005 (VR13-EE) — AN ERROR STEP'S CHECKS ARE JUDGED, AND RECORDED AS ENFORCED.
//
// Two halves, and the second is the one that would have been missed. The judge now evaluates a body
// check on an expected error (internal/mcp), so a chain's error step can fail on its content. But the
// ENFORCED marker — the thing that tells a reader which claims were actually evaluated — was gated on
// `want.ErrorPlane == mcp.PlaneNone`, so an error step's checks would have been judged and then
// reported as though nothing had been checked. A step that proves its claim and says it proved
// nothing is the same defect this round is about, pointing the other way.
//
// Three separate Run calls, one step each: a chain stops judging after its first failure, so putting
// these in one chain would measure the chain's ordering rather than the step's marker.
func TestMCPStep_ErrorStepBodyIsJudgedAndRecordedAsEnforced(t *testing.T) {
	run := func(t *testing.T, body []mcp.BodyAssert) report.StepResult {
		t.Helper()
		srv := httptest.NewServer(fakeMCP(true, "object not found"))
		defer srv.Close()
		cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
		res := Run("tr-x", []Step{MCPStep("del", cl, "t", "{}",
			mcp.Expect{ErrorPlane: mcp.PlaneTool, Body: body}, nil)})
		if len(res.Steps) != 1 {
			t.Fatalf("want one step, got %d", len(res.Steps))
		}
		return res.Steps[0]
	}

	t.Run("a check that holds: passed, and counted as enforced", func(t *testing.T) {
		st := run(t, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "object not found"}})
		if st.Status != "passed" {
			t.Fatalf("status = %q (%s), want passed", st.Status, st.Observed)
		}
		if st.AssertionsEnforcedCount != 1 {
			t.Errorf("AssertionsEnforcedCount = %d, want 1 — the check WAS evaluated, so it must say so",
				st.AssertionsEnforcedCount)
		}
	})

	t.Run("a check that misses: failed, with the tool-error observed", func(t *testing.T) {
		st := run(t, []mcp.BodyAssert{{Op: mcp.BodyContainsOp, Value: "zzz"}})
		if st.Status != "failed" {
			t.Fatalf("status = %q (%s), want failed", st.Status, st.Observed)
		}
		// the judge's tool-error text, with its closing pointer replaced by the chain
		// step's own (failed_claims — the per-claim record that really exists).
		if st.Observed != chainBodyNote(mcp.BodyAssertObservedToolError) || st.Observed == mcp.BodyAssertObservedToolError {
			t.Errorf("Observed = %q, want BodyAssertObservedToolError with the chain pointer", st.Observed)
		}
		if len(st.AssertionsEnforced) != 1 {
			t.Fatalf("AssertionsEnforced = %v, want one entry", st.AssertionsEnforced)
		}
	})

	t.Run("no check at all: passed, and nothing is claimed", func(t *testing.T) {
		st := run(t, nil)
		if st.Status != "passed" {
			t.Fatalf("status = %q (%s), want passed — an error step with no content check is unchanged", st.Status, st.Observed)
		}
		if st.AssertionsEnforcedCount != 0 {
			t.Errorf("AssertionsEnforcedCount = %d, want 0", st.AssertionsEnforcedCount)
		}
	})
}

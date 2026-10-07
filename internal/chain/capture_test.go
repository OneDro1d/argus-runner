package chain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// ── CHAIN STEP CAPTURE ────────────────────────────────────────────────────────────────────
//
// The gap this closes, named by three consecutive triage rounds (Memstore 2026-07-22 RC-1/HYG-1
// and the two before it): the chain executor resolved ALL step args once, up front, from
// ${cid}/${VAR}, and there was no way to thread one step's RESPONSE into a later step's args.
// With server-generated ids (Memstore mints document ids per write) a self-contained
// write → read-that-exact-doc chain was INEXPRESSIBLE. Two measured consequences:
//   - scenarios pinned hand-seeded fixtures instead; one environment reset destroyed the fixture
//     and 7 of 8 Memstore failures fell out of it at once.
//   - CLEANUP could not delete what a chain created → 197 leftover libraries across 15 runs.
//
// The contract: a step declares `save: {"<var>": "<path>"}`; later steps reference `${saved.<var>}`
// in their args. `${saved.…}` deliberately contains a dot so the up-front varRe pass cannot touch
// it — it survives to be bound at run time.

func TestCaptureFrom_ReadsThroughTheMCPContentTextJSON(t *testing.T) {
	// the real MCP shape: the useful payload is JSON *inside* result.content[0].text
	raw := json.RawMessage(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text",` +
		`"text":"{\"id\":\"7d702c8b-af8d-4bbb-9166-d9adfb729603\",\"scope\":\"group\",\"never_expires\":false}"}],` +
		`"isError":false}}`)
	for path, want := range map[string]string{
		"id":            "7d702c8b-af8d-4bbb-9166-d9adfb729603",
		"scope":         "group",
		"never_expires": "false",
	} {
		got, ok := captureFrom(raw, path)
		if !ok || got != want {
			t.Errorf("captureFrom(%q) = %q, ok=%v; want %q", path, got, ok, want)
		}
	}
}

func TestCaptureFrom_NestedAndArrayPaths(t *testing.T) {
	raw := json.RawMessage(`{"result":{"content":[{"type":"text",` +
		`"text":"{\"doc\":{\"id\":\"D1\"},\"items\":[{\"id\":\"I0\"},{\"id\":\"I1\"}]}"}]}}`)
	for path, want := range map[string]string{
		"doc.id":     "D1",
		"items.0.id": "I0",
		"items.1.id": "I1",
	} {
		if got, ok := captureFrom(raw, path); !ok || got != want {
			t.Errorf("captureFrom(%q) = %q, ok=%v; want %q", path, got, ok, want)
		}
	}
}

// Not every SUT wraps its payload in content[0].text — some return a structured result directly.
// Fall back to the envelope's `result` object so both shapes work without the author knowing which.
func TestCaptureFrom_FallsBackToTheResultObject(t *testing.T) {
	raw := json.RawMessage(`{"result":{"document_id":"abc-123","isError":false}}`)
	if got, ok := captureFrom(raw, "document_id"); !ok || got != "abc-123" {
		t.Errorf("fallback to result object: got %q ok=%v", got, ok)
	}
}

func TestCaptureFrom_MissingPathReportsNotFound(t *testing.T) {
	raw := json.RawMessage(`{"result":{"content":[{"type":"text","text":"{\"id\":\"x\"}"}]}}`)
	if got, ok := captureFrom(raw, "nope"); ok {
		t.Errorf("a missing path must report ok=false, got %q", got)
	}
	if _, ok := captureFrom(json.RawMessage(`not json`), "id"); ok {
		t.Error("an unparseable envelope must report ok=false")
	}
}

func fakeCaptureMCP(t *testing.T, respText string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			text := respText
			if text == "" { // echo the args back so a test can assert what was actually SENT
				b, _ := json.Marshal(req.Params.Arguments)
				text = string(b)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}},
					"isError": false}})
		}
	}))
}

// THE headline case: write → read-back-THAT-doc, with no pinned fixture anywhere. This is the
// chain that Memstore's CRUD-002 could not express, and the reason it pinned a01d7653-… instead.
func TestRun_CapturedVarIsLateBoundIntoALaterStepsArgs(t *testing.T) {
	writeSrv := fakeCaptureMCP(t, `{"id":"srv-generated-42"}`)
	defer writeSrv.Close()

	// the read server records exactly what arguments reached it — the only proof that matters
	var sentArgs string
	readSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			b, _ := json.Marshal(req.Params.Arguments)
			sentArgs = string(b)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}},
					"isError": false}})
		}
	}))
	defer readSrv.Close()

	wcl := &mcp.Client{ServerURL: writeSrv.URL, Transport: mcp.Streamable}
	rcl := &mcp.Client{ServerURL: readSrv.URL, Transport: mcp.Streamable}

	res := Run("tr-x", []Step{
		MCPStep("write-doc", wcl, "memstore_write", `{"title":"t"}`,
			mcp.Expect{ErrorPlane: mcp.PlaneNone}, map[string]scenario.SaveSpec{"docId": {Path: "id"}}),
		MCPStep("read-back", rcl, "memstore_read", `{"document_id":"${saved.docId}"}`,
			mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
	})

	if res.Status != "passed" {
		t.Fatalf("chain should pass: %s / %+v", res.Status, res.Failure)
	}
	if !strings.Contains(sentArgs, "srv-generated-42") {
		t.Errorf("step 2 must SEND the captured id; it sent: %s", sentArgs)
	}
	if strings.Contains(sentArgs, "${saved") {
		t.Errorf("the literal placeholder reached the SUT: %s", sentArgs)
	}
}

// A reference to a variable no earlier step saved must FAIL THE STEP LOUDLY. Sending the literal
// "${saved.docId}" to the SUT would produce a confusing downstream error ("must be a valid UUID")
// that blames the SUT for a scenario-authoring mistake — exactly the misattribution this whole
// line of work exists to stop.
func TestRun_UnresolvedSavedRefFailsTheStepWithANamedError(t *testing.T) {
	srv := fakeCaptureMCP(t, "")
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}

	res := Run("tr-x", []Step{
		MCPStep("read-back", cl, "memstore_read", `{"document_id":"${saved.missing}"}`,
			mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
	})
	if res.Status == "passed" {
		t.Fatal("a step referencing an unsaved variable must not pass")
	}
	obs := res.Steps[0].Observed
	if !strings.Contains(obs, "saved.missing") {
		t.Errorf("the error must NAME the unresolved variable, got: %s", obs)
	}
	if !strings.Contains(strings.ToLower(obs), "save") {
		t.Errorf("the error should point at the save mechanism, got: %s", obs)
	}
}

// A save whose path does not exist in the response is an authoring error too — fail the SAVING
// step, so the break is reported where the mistake is rather than two steps later.
func TestRun_SaveWithAMissingPathFailsTheSavingStep(t *testing.T) {
	srv := fakeCaptureMCP(t, `{"id":"x"}`)
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}

	res := Run("tr-x", []Step{
		MCPStep("write-doc", cl, "memstore_write", `{}`,
			mcp.Expect{ErrorPlane: mcp.PlaneNone}, map[string]scenario.SaveSpec{"docId": {Path: "no.such.path"}}),
	})
	if res.Status == "passed" {
		t.Fatal("a save with an unresolvable path must fail its own step")
	}
	if !strings.Contains(res.Steps[0].Observed, "no.such.path") {
		t.Errorf("the error must name the failing path, got: %s", res.Steps[0].Observed)
	}
}

// Capture must not disturb the break contract: a chain with no `save` anywhere behaves exactly as
// one with, and a mid-chain failure produces the VR12-CH1 shape.
//
// ⛔ UPDATED BY V30-001: it used to assert `ran == 2` and `Steps[2].Status == "skipped"`. The
// executor now CONTINUES past a failure, so all three run — and the third can never be green.
func TestRun_NoCaptureBehavesTheSameAsCapture(t *testing.T) {
	ran := 0
	res := Run("tr-x", []Step{
		{Name: "a", Run: func(string, map[string]string) report.StepResult { ran++; return report.StepResult{Status: "passed"} }},
		{Name: "b", Run: func(string, map[string]string) report.StepResult {
			ran++
			return report.StepResult{Status: "failed", Observed: "boom"}
		}},
		{Name: "c", Run: func(string, map[string]string) report.StepResult { ran++; return report.StepResult{Status: "passed"} }},
	})
	if ran != 3 {
		t.Errorf("every step must be attempted (VR12-CH1 rule 2); ran %d", ran)
	}
	if res.Status != "failed" || res.Steps[2].Status != report.StepRanAfterFailureOK {
		t.Errorf("break semantics regressed: %s / %+v", res.Status, res.Steps)
	}
}

// GUARD (V31-003) — THE VALIDATOR'S ${saved.<var>} GRAMMAR IS THIS RUNTIME'S GRAMMAR.
//
// V31-003 makes the validator refuse a check carrying a placeholder nothing fills in, and it treats
// ${saved.<var>} as fillable — on the strength of a pattern declared in internal/scenario. If the two
// ever drift, the validator accepts a reference this runtime leaves literal, and the literal text
// reaches the SUT: exactly the misattribution bindSaved exists to prevent. They cannot drift while
// capture.go compiles the shared constant, and this asserts that it does.
//
// ⚠ It lives here because savedRefRe is unexported in package chain and cannot be read anywhere else.
func TestSavedRefGrammarIsTheValidatorsGrammar(t *testing.T) {
	if savedRefRe.String() != scenario.SavedRefPattern {
		t.Fatalf("the chain runtime compiles %q while the validator declares %q — a reference the validator accepts could be left literal by the runtime",
			savedRefRe.String(), scenario.SavedRefPattern)
	}
}

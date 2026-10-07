package chain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// fakeMCP answers initialize + tools/call; `fail` decides the tool plane, and a failing call
// carries a REAL error string in content[0].text — the thing PROB-1 is about surfacing.
func fakeMCP(fail bool, errText string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			text := "ok"
			if fail {
				text = errText
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
				"isError": fail,
			}})
		}
	}
}

// PROB-1 (2026-07-22) — a chain step's FAILURE must carry the SUT's own error envelope.
//
// Found by evaluating the Memstore triage of run 20260722T1258278df3b4. Chain-step failures reported
// only `observed: "responder returned result.isError:true"` and nothing else, because MCPStep built
// a StepResult from {Status, Observed} and DISCARDED the raw envelope — MCPEnvelope was populated
// only on the single-step path (argus/mcp_scenario.go). The SUT's actual error string is the whole
// discriminator: "object not found" means the pinned fixture is gone (TEST_BUG), while "visibility
// check failed" would mean a tenancy/RLS regression against pre-existing rows (CODE_BUG). Unable to
// read it, that triage left 7 of its 8 verdicts carrying an un-excluded CODE_BUG alternative, and
// named this fix itself: "That alone would have closed Clusters A and B."
func TestMCPStep_FailedStepCarriesTheSUTErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(fakeMCP(true, "visibility check failed"))
	defer srv.Close()

	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	st := MCPStep("read-fixture-doc", cl, "memstore_read", `{"document_id":"a01d7653"}`,
		mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil).Run("tr-x-CRUD-002-1", nil)

	if st.Status != "failed" {
		t.Fatalf("expected the step to fail, got %q (%s)", st.Status, st.Observed)
	}
	if len(st.MCPEnvelope) == 0 {
		t.Fatal("a FAILED chain step must carry the SUT's envelope — without it the error text is " +
			"unreachable and TEST_BUG cannot be told from CODE_BUG")
	}
	if !strings.Contains(string(st.MCPEnvelope), "visibility check failed") {
		t.Errorf("the envelope must carry the SUT's real error text, got: %s", st.MCPEnvelope)
	}
	var probe map[string]any
	if err := json.Unmarshal(st.MCPEnvelope, &probe); err != nil {
		t.Errorf("the envelope must be valid JSON: %v", err)
	}
}

// A PASSING step must NOT carry an envelope: report.json already runs to ~120KB for a 45-scenario
// suite, and attaching every successful response would bloat it for zero diagnostic value. The
// envelope is evidence for a failure, not a transcript.
func TestMCPStep_PassingStepCarriesNoEnvelope(t *testing.T) {
	srv := httptest.NewServer(fakeMCP(false, ""))
	defer srv.Close()

	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}
	st := MCPStep("write-doc", cl, "memstore_write", `{}`,
		mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil).Run("tr-x-CRUD-002-1", nil)
	if st.Status != "passed" {
		t.Fatalf("expected pass, got %q (%s)", st.Status, st.Observed)
	}
	if len(st.MCPEnvelope) != 0 {
		t.Errorf("a PASSING step must not carry an envelope (report bloat): %s", st.MCPEnvelope)
	}
}

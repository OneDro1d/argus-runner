package mcp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// UC-51 / UC-27(legacy-SSE) / VR-N8: the suite's OWN both-transport client speaks
// legacy HTTP+SSE to the suite's OWN Camp-B server — the recursive dogfood — and the
// two-plane judge passes on success, the tool plane, and the protocol plane.
func TestRecursiveDogfood_LegacySSE(t *testing.T) {
	okTool := mcpserver.Tool{Name: "runner__echo", Namespace: mcpserver.NSRunner,
		InputSchema: map[string]any{"type": "object"},
		Handler: func(_ json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
			return mcpserver.Ok(map[string]any{"echo": "hi"})
		}}
	boomTool := mcpserver.Tool{Name: "runner__boom", Namespace: mcpserver.NSRunner,
		InputSchema: map[string]any{"type": "object"},
		Handler: func(_ json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
			return mcpserver.ToolErr(map[string]any{"observed": "boom"})
		}}
	srv, err := mcpserver.NewServer(auth.Config{RunnerToken: "rdog", AuthorToken: "adog"}, okTool, boomTool)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpserver.NewHTTPHandler(srv))
	defer ts.Close()

	cl := &mcp.Client{ServerURL: ts.URL, Transport: mcp.LegacySSE, Token: "rdog"}

	if v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "runner__echo", RequestID: "tr-dog"}), mcp.Expect{ErrorPlane: mcp.PlaneNone}); !v.Pass {
		t.Fatalf("dogfood success failed: %+v", v)
	}
	if v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "runner__boom"}), mcp.Expect{ErrorPlane: mcp.PlaneTool}); !v.Pass {
		t.Fatalf("dogfood tool-plane error failed: %+v", v)
	}
	if v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "runner__nope"}), mcp.Expect{ErrorPlane: mcp.PlaneProtocol, ErrorCode: -32601}); !v.Pass {
		t.Fatalf("dogfood protocol-plane (unknown tool -32601) failed: %+v", v)
	}
}

// fakeStreamable is a minimal conformant Streamable-HTTP MCP server (single POST /mcp;
// Mcp-Session-Id header; declares protocolVersion 2024-11-05 to test the client's
// version-tolerance against its offered 2025-03-26 — UC-28).
func fakeStreamable() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": `{"ok":true}`}},
				"isError": req.Params.Name == "fail_tool",
			}})
		}
	}
}

// UC-27 (Streamable HTTP branch) + UC-28 (revision tolerance): the client speaks the
// streamable handshake and judges success + the tool plane.
func TestStreamable_SuccessAndToolError(t *testing.T) {
	ts := httptest.NewServer(fakeStreamable())
	defer ts.Close()
	cl := &mcp.Client{ServerURL: ts.URL + "/mcp", Transport: mcp.Streamable, Token: "x"}

	if v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "ok_tool"}), mcp.Expect{ErrorPlane: mcp.PlaneNone}); !v.Pass {
		t.Fatalf("streamable success failed: %+v", v)
	}
	if v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "fail_tool"}), mcp.Expect{ErrorPlane: mcp.PlaneTool}); !v.Pass {
		t.Fatalf("streamable tool-plane error failed: %+v", v)
	}
}

// UC-30 / VR-J8: an unreachable endpoint is classified as unreachable (gate-infra), not a tool failure.
func TestUnreachable(t *testing.T) {
	cl := &mcp.Client{ServerURL: "http://127.0.0.1:9/mcp", Transport: mcp.Streamable, HTTP: &http.Client{Timeout: 2 * time.Second}}
	v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "x"}), mcp.Expect{ErrorPlane: mcp.PlaneNone})
	if v.Plane != "unreachable" {
		t.Fatalf("an unreachable endpoint must be classified 'unreachable', got %+v", v)
	}
}

// V31-005 (VR13-EE) — the WIRE path for a protocol error's `data`.
//
// RPCError.Data is new, and every other test in this round builds the error object in Go. This one
// proves it survives the wire: our own server refuses a product-hat token with a JSON-RPC error that
// carries `data.instance_id`, and a check written against that path resolves through the serialized
// error object the judge now matches on.
func TestRecursiveDogfood_ProtocolErrorBodyIsJudged(t *testing.T) {
	probe := mcpserver.Tool{Name: "author__probe", Namespace: mcpserver.NSAuthor,
		InputSchema: map[string]any{"type": "object"},
		Handler: func(_ json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
			return mcpserver.Ok(map[string]any{})
		}}
	srv, err := mcpserver.NewServer(auth.Config{RunnerToken: "rdog", AuthorToken: "adog"}, probe)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpserver.NewHTTPHandler(srv))
	defer ts.Close()

	// the RUNNER token against an AUTHOR tool — refused on the protocol plane, with data
	cl := &mcp.Client{ServerURL: ts.URL, Transport: mcp.LegacySSE, Token: "rdog"}

	if v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "author__probe"}),
		mcp.Expect{ErrorPlane: mcp.PlaneProtocol,
			Body: []mcp.BodyAssert{{Field: "data.instance_id", Op: mcp.BodyExistsOp}}}); !v.Pass {
		t.Fatalf("a check on the error's own data did not resolve over the wire: %+v", v)
	}
	v := mcp.Judge(cl.Call(mcp.CallInput{Tool: "author__probe"}),
		mcp.Expect{ErrorPlane: mcp.PlaneProtocol,
			Body: []mcp.BodyAssert{{Field: "data.no_such_field", Op: mcp.BodyExistsOp}}})
	if v.Pass {
		t.Fatalf("a check on a field the error does not carry PASSED: %+v", v)
	}
	if v.Plane != mcp.PlaneBody {
		t.Errorf("Plane = %q, want %q", v.Plane, mcp.PlaneBody)
	}
}

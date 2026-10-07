package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// VR2-13 / TS2-12 (V15-007) — the router wraps an upstream answer EXACTLY ONCE.
//
// ── WHAT WAS WRONG ────────────────────────────────────────────────────────────────────────────────
//
// Forward used to return `res.Raw` — the upstream's COMPLETE JSON-RPC envelope — and the proxy handed
// that to mcpserver.Ok, which marshals whatever it is given into the text of a content block. So an
// envelope went inside an envelope and every caller unwrapped twice:
//
//	{"result":{"content":[{"text":"{\"result\":{\"content\":[{\"text\":\"{...the real answer...}\"}]}}"}]}}
//
// Measured live 2026-08-12: exactly 2 envelopes deep on BOTH compose and k3d, so it was systemic
// rather than one tier's accident.
//
// ── WHY THE DEPTH IS COUNTED AND NOT EYEBALLED ────────────────────────────────────────────────────
//
// "Looks unwrapped" is not a measurement. These cases count the nesting the same way the live probe
// did — decode, look for another envelope inside, repeat — so a future change that reintroduces a
// layer fails here instead of being noticed months later by someone parsing a string that should
// have been an object.

// upstreamStub answers initialize, then one tools/call whose result carries `payload` as the text of
// a single content block — the shape every real Argus tool produces.
func upstreamStub(t *testing.T, payload string, isErr bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "s1")
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"stub","version":"1"}}}`))
			return
		}
		body := map[string]any{"content": []any{map[string]any{"type": "text", "text": payload}}}
		if isErr {
			body["isError"] = true
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": body})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// envelopeDepth counts how many JSON-RPC envelopes are nested in v, the same way the live probe did.
func envelopeDepth(v any) int {
	depth := 0
	for {
		m, ok := v.(map[string]any)
		if !ok {
			return depth
		}
		res, ok := m["result"].(map[string]any)
		if !ok {
			return depth
		}
		depth++
		c, ok := res["content"].([]any)
		if !ok || len(c) == 0 {
			return depth
		}
		blk, ok := c[0].(map[string]any)
		if !ok {
			return depth
		}
		txt, ok := blk["text"].(string)
		if !ok {
			return depth
		}
		var next any
		if err := json.Unmarshal([]byte(txt), &next); err != nil {
			return depth // the text is not itself an envelope — this is the bottom
		}
		v = next
	}
}

func TestForward_ReturnsTheUpstreamPayloadNotItsEnvelope(t *testing.T) {
	srv := upstreamStub(t, `{"dashboard_url":"http://localhost:3000/d/x","note":"n"}`, false)

	out, isErr, err := MCPForwarder{}.Forward(
		Target{URL: srv.URL + "/mcp", Token: "t", Plane: "runner"},
		"runner__get_dashboard_url", json.RawMessage(`{"instance_id":"suta"}`))
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if isErr {
		t.Fatal("a successful tool answer was reported as isError")
	}

	// THE POINT: what comes back is the tool's own object, not a JSON-RPC envelope around it.
	if d := envelopeDepth(out); d != 0 {
		t.Fatalf("Forward returned %d JSON-RPC envelope(s); it must return the PAYLOAD, so the proxy's\n"+
			"single wrap is the only one. got: %#v", d, out)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want the decoded object — a caller should not have to parse a string", out)
	}
	if m["dashboard_url"] != "http://localhost:3000/d/x" {
		t.Fatalf("the tool's own fields did not survive the hop: %#v", m)
	}
}

// isError must NOT be flattened away. A tool that was REACHED and answered "failed" arriving as a
// success is the silent-wrong-answer class this codebase exists to remove.
func TestForward_PropagatesUpstreamIsError(t *testing.T) {
	srv := upstreamStub(t, `{"error":"scenario not found"}`, true)

	out, isErr, err := MCPForwarder{}.Forward(
		Target{URL: srv.URL + "/mcp", Token: "t", Plane: "author"},
		"author__read_scenario", json.RawMessage(`{"instance_id":"suta"}`))
	if err != nil {
		t.Fatalf("forward: %v", err) // reached and answered — NOT a transport error
	}
	if !isErr {
		t.Fatal("the upstream answered isError:true and the router reported success")
	}
	if m, ok := out.(map[string]any); !ok || m["error"] != "scenario not found" {
		t.Fatalf("the tool's own error payload did not survive: %#v", out)
	}
}

// A tool is entitled to answer in prose. That must come back as the text, not as a parse failure and
// not wrapped in a synthetic envelope.
func TestForward_NonJSONTextComesBackAsText(t *testing.T) {
	srv := upstreamStub(t, "plain words, not json", false)

	out, _, err := MCPForwarder{}.Forward(
		Target{URL: srv.URL + "/mcp", Token: "t", Plane: "runner"},
		"runner__get_tail_logs", json.RawMessage(`{"instance_id":"suta"}`))
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if s, ok := out.(string); !ok || s != "plain words, not json" {
		t.Fatalf("prose answer mangled: %#v", out)
	}
}

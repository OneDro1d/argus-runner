package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The unit half of the join defect. See localiseInstanceID for why it existed.
func TestLocaliseInstanceID(t *testing.T) {
	cases := []struct {
		name, plane string
		in, want    map[string]any
	}{
		{"runner rewrites", "runner",
			map[string]any{"instance_id": "orderservice-compose", "layer": "http-ingestion"},
			map[string]any{"instance_id": "local", "layer": "http-ingestion"}},
		{"author keeps the real id", "author",
			map[string]any{"instance_id": "orderservice-compose"},
			map[string]any{"instance_id": "orderservice-compose"}},
		{"runner without an id is untouched", "runner",
			map[string]any{"run_id": "r1"}, map[string]any{"run_id": "r1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := localiseInstanceID(c.in, c.plane).(map[string]any)
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(c.want)
			if string(gj) != string(wj) {
				t.Fatalf("got %s, want %s", gj, wj)
			}
		})
	}
	// A non-object payload must pass through rather than panic.
	if got := localiseInstanceID("not-an-object", "runner"); got != "not-an-object" {
		t.Fatalf("scalar args were altered: %v", got)
	}
	// The caller's map must NOT be mutated — a router that rewrote its input would make the same
	// call behave differently the second time.
	in := map[string]any{"instance_id": "suta"}
	_ = localiseInstanceID(in, "runner")
	if in["instance_id"] != "suta" {
		t.Fatalf("the input map was mutated: %v", in)
	}
}

// THE ASSERTION THE ORIGINAL TESTS WERE MISSING. They forwarded through a stub that echoed whatever
// it was given, so they proved the right TARGET was chosen and never that the PAYLOAD was one the
// executor could answer. This one records what actually went over the wire.
func TestMCPForwarder_SendsLocalToTheRunnerPlaneAndTheRealIDToTheAuthorPlane(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "s1")
		if req.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"stub","version":"1"}}}`))
			return
		}
		if v, ok := req.Params.Arguments["instance_id"].(string); ok {
			seen = v
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer srv.Close()

	f := MCPForwarder{}
	args := json.RawMessage(`{"instance_id":"orderservice-compose"}`)

	if _, _, err := f.Forward(Target{URL: srv.URL + "/mcp", Token: "t", Plane: "runner"}, "runner__run", args); err != nil {
		t.Fatalf("runner forward: %v", err)
	}
	if seen != InEnvInstanceID {
		t.Fatalf("the executor received instance_id=%q, want %q — it serves that one only and refuses anything else by name", seen, InEnvInstanceID)
	}

	if _, _, err := f.Forward(Target{URL: srv.URL + "/mcp", Token: "t", Plane: "author"}, "author__list_scenarios", args); err != nil {
		t.Fatalf("author forward: %v", err)
	}
	if seen != "orderservice-compose" {
		t.Fatalf("the control plane received instance_id=%q — rewriting there would address the wrong instance entirely", seen)
	}
}

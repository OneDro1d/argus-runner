package chain

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// uniqueIDMCP is an MCP server that behaves like social MCP since 2026-09-24: it refuses a
// tools/call whose _meta.request_id it has already seen, with "request_id reused". It records every
// id it was sent.
type uniqueIDMCP struct {
	mu   sync.Mutex
	seen []string
}

func (u *uniqueIDMCP) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta struct {
					RequestID string `json:"request_id"`
				} `json:"_meta"`
			} `json:"params"`
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
			u.mu.Lock()
			reused := false
			for _, s := range u.seen {
				if s == req.Params.Meta.RequestID {
					reused = true
				}
			}
			u.seen = append(u.seen, req.Params.Meta.RequestID)
			u.mu.Unlock()
			text := "ok"
			if reused {
				text = "request_id reused"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
				"isError": reused,
			}})
		}
	}
}

// Reported by the social MCP session 2026-09-24: every MCP step of a chain sent the chain's one
// correlation id as its request id, so against a SUT that enforces uniqueness every step after the
// first failed with "request_id reused". Each call now gets its own id, still prefixed by the
// correlation id so the logs keep joining — and a whole-chain re-fire (the throttle retry reuses the
// correlation id) gets fresh ones too.
func TestChain_EveryMCPStepSendsItsOwnRequestID(t *testing.T) {
	sut := &uniqueIDMCP{}
	srv := httptest.NewServer(sut.handler())
	defer srv.Close()
	cl := &mcp.Client{ServerURL: srv.URL, Transport: mcp.Streamable}

	const cid = "tr-run1-CHAIN-001-0badf00d"
	steps := func() []Step {
		return []Step{
			MCPStep("create", cl, "t_create", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
			MCPStep("read", cl, "t_read", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
			MCPStep("delete", cl, "t_delete", `{}`, mcp.Expect{ErrorPlane: mcp.PlaneNone}, nil),
		}
	}
	for attempt := 1; attempt <= 2; attempt++ {
		res := Run(cid, steps())
		if res.Status != "passed" {
			obs := ""
			if res.Failure != nil {
				obs = res.Failure.Observed
			}
			t.Fatalf("attempt %d: chain status = %q (%s), want passed — a step reused a request id", attempt, res.Status, obs)
		}
		if res.CorrelationID != cid {
			t.Fatalf("attempt %d: the scenario's correlation id changed to %q; only the request ids are per call", attempt, res.CorrelationID)
		}
	}
	if len(sut.seen) != 6 {
		t.Fatalf("the SUT saw %d calls, want 6 (3 steps x 2 attempts): %v", len(sut.seen), sut.seen)
	}
	for _, id := range sut.seen {
		if !strings.HasPrefix(id, cid+".") {
			t.Errorf("request id %q does not start with the correlation id %q — the log join is lost", id, cid)
		}
	}
}

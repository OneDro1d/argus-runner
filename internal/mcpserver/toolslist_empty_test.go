package mcpserver

import (
	"encoding/json"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// A principal who may see NO tool must get `"tools": []`, not `"tools": null`. Claude Code validates
// tools/list strictly, so a null here is the same failure as a null inputSchema.required: the client
// rejects the answer and offers OAuth instead (2026-09-27).
func TestToolsList_NoVisibleToolIsAnEmptyArray(t *testing.T) {
	prin := Principal{Hat: role.Product, Workspace: "ws_1"}
	s, err := NewServerWithAuth(fixedPrin{prin},
		Tool{
			Name: "runner__get_tail_logs", Namespace: NSBuilder, Hidden: true, Description: "d",
			InputSchema: map[string]any{"type": "object", "required": []string{}},
			Handler:     func(json.RawMessage, Principal) Outcome { return Ok(nil) },
		},
	)
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	s.NewSession("s")
	s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))

	raw, _ := s.Dispatch("s", "tok", reqBytes(2, "tools/list", nil))
	var r struct {
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if got := string(r.Result["tools"]); got != "[]" {
		t.Fatalf(`tools/list with no visible tool: "tools" = %s, want []`, got)
	}
}

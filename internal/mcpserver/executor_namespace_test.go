package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// executorFixture registers one executor__ tool alongside an author__ one, so the isolation tests can
// prove the THIRD namespace (AC-16) is disjoint from the other two, not merely additive.
func executorFixture(t *testing.T) *Server {
	t.Helper()
	s, err := NewServerWithAuth(fixedAuth(role.Executor),
		Tool{
			Name: "executor__poll", Namespace: NSExecutor, Description: "d",
			InputSchema: map[string]any{"type": "object"},
			Handler:     func(json.RawMessage, Principal) Outcome { return Ok(map[string]any{"ran": true}) },
		},
		Tool{
			Name: "author_thing", Namespace: NSAuthor, Description: "d",
			InputSchema: map[string]any{"type": "object"},
			Handler:     func(json.RawMessage, Principal) Outcome { return Ok(map[string]any{"ran": true}) },
		},
	)
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	return s
}

// An executor-hat caller sees the executor tool and NOT the author tool (AC-16 deliverable 1: "never
// visible to the runner or author namespaces").
func TestExecutorNamespace_ExecutorHatSeesOnlyExecutorTools(t *testing.T) {
	s := executorFixture(t)
	tools := s.VisibleToolsFor(role.Executor)
	if len(tools) != 1 || tools[0].Name != "executor__poll" {
		t.Fatalf("VisibleToolsFor(executor) = %+v, want exactly [executor__poll]", tools)
	}
}

// A runner (product) hat and an author (test) hat must NEVER see an executor__ tool — the namespace
// isolation test the brief calls for.
func TestExecutorNamespace_InvisibleToRunnerAndAuthor(t *testing.T) {
	for _, hat := range []role.Role{role.Product, role.Test} {
		s := executorFixture(t)
		for _, tool := range s.VisibleToolsFor(hat) {
			if tool.Namespace == NSExecutor {
				t.Fatalf("hat %q can see executor tool %q — the executor namespace leaked", hat, tool.Name)
			}
		}
	}
}

// tools/list must never publish executor__poll to a non-executor hat, and tools/call must refuse it —
// not just hide it from the listing.
func TestExecutorNamespace_ToolsCallRefusedForOtherHats(t *testing.T) {
	for _, hat := range []role.Role{role.Product, role.Test} {
		s, err := NewServerWithAuth(fixedAuth(hat),
			Tool{
				Name: "executor__poll", Namespace: NSExecutor, Description: "d",
				InputSchema: map[string]any{"type": "object"},
				Handler:     func(json.RawMessage, Principal) Outcome { return Ok(map[string]any{"ran": true}) },
			})
		if err != nil {
			t.Fatalf("NewServerWithAuth: %v", err)
		}
		s.NewSession("s")
		s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
		s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))

		raw, _ := s.Dispatch("s", "tok", reqBytes(2, "tools/list", nil))
		if strings.Contains(string(raw), "executor__poll") {
			t.Fatalf("hat %q: tools/list published executor__poll: %s", hat, raw)
		}

		raw, _ = s.Dispatch("s", "tok", reqBytes(3, "tools/call", map[string]any{"name": "executor__poll", "arguments": map[string]any{}}))
		var r Response
		_ = json.Unmarshal(raw, &r)
		if r.Error == nil || r.Error.Code != CodeUnauthorized {
			t.Fatalf("hat %q calling executor__poll: got %+v, want CodeUnauthorized", hat, r.Error)
		}
	}
}

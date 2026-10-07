package mcpserver

// builder_namespace_test.go — AC-17: the NSBuilder namespace and the Hidden dispatch mechanism.
// TEST-FIRST: NSBuilder is a FOURTH, disjoint scope visible only to the builder principal shape
// (Hat: role.Product, a non-empty Workspace — the runner-scope OAuth token's own shape,
// cloudauth.go), never to the in-environment product hat's local (workspace-less) token, the author
// hat, or the executor hat. A Hidden tool stays dispatchable while absent from tools/list.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// fixedPrin authenticates every token to a FIXED Principal — for the one boundary NSBuilder needs
// that the hat-only fixedAuth helper cannot express: a workspace.
type fixedPrin struct{ p Principal }

func (f fixedPrin) Authenticate(string) (Principal, error) { return f.p, nil }

// builderFixture registers one builder__ tool (Namespace NSBuilder) alongside a runner__ one, so the
// isolation tests can prove the namespace is disjoint from NSRunner even though both compile down to
// role.Product.
func builderFixture(t *testing.T, auth Authenticator) *Server {
	t.Helper()
	s, err := NewServerWithAuth(auth,
		Tool{
			Name: "runner__run", Namespace: NSBuilder, Description: "d",
			InputSchema: map[string]any{"type": "object"},
			Handler:     func(json.RawMessage, Principal) Outcome { return Ok(map[string]any{"ran": true}) },
		},
		Tool{
			Name: "runner__validate_config", Namespace: NSRunner, Description: "d",
			InputSchema: map[string]any{"type": "object"},
			Handler:     func(json.RawMessage, Principal) Outcome { return Ok(map[string]any{"ran": true}) },
		},
	)
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	return s
}

// TestBuilderNamespace_BuilderPrincipalSeesIt: the runner-scope OAuth shape (Hat: role.Product,
// Workspace non-empty) sees the NSBuilder tool.
func TestBuilderNamespace_BuilderPrincipalSeesIt(t *testing.T) {
	prin := Principal{Hat: role.Product, Workspace: "ws_1", Subject: "sub_1"}
	s := builderFixture(t, fixedPrin{prin})
	tools := s.VisibleToolsForPrincipal(prin)
	found := false
	for _, tl := range tools {
		if tl.Name == "runner__run" && tl.Namespace == NSBuilder {
			found = true
		}
	}
	if !found {
		t.Fatalf("VisibleToolsForPrincipal(builder) = %+v, want it to include the NSBuilder runner__run", tools)
	}
}

// TestBuilderNamespace_LocalProductTokenNeverSeesIt: the SAME hat (role.Product) with NO workspace —
// the in-environment product hat's local static token — must never see the relay. Only the
// workspace-bound shape does (types.go's NSBuilder doc).
func TestBuilderNamespace_LocalProductTokenNeverSeesIt(t *testing.T) {
	prin := Principal{Hat: role.Product} // no workspace: the local static token's shape
	s := builderFixture(t, fixedPrin{prin})
	for _, tl := range s.VisibleToolsForPrincipal(prin) {
		if tl.Namespace == NSBuilder {
			t.Fatalf("a workspace-less product principal can see NSBuilder tool %q — the relay leaked to the local router", tl.Name)
		}
	}
}

// TestBuilderNamespace_InvisibleToAuthorAndExecutor: the author and executor hats never see it either,
// even WITH a workspace on the principal (author tokens carry one too).
func TestBuilderNamespace_InvisibleToAuthorAndExecutor(t *testing.T) {
	for _, hat := range []role.Role{role.Test, role.Executor} {
		prin := Principal{Hat: hat, Workspace: "ws_1"}
		s := builderFixture(t, fixedPrin{prin})
		for _, tl := range s.VisibleToolsForPrincipal(prin) {
			if tl.Namespace == NSBuilder {
				t.Fatalf("hat %q can see NSBuilder tool %q — the builder namespace leaked", hat, tl.Name)
			}
		}
	}
}

// TestBuilderNamespace_ToolsCallRefusedForNonBuilder: tools/call refuses the NSBuilder tool by name
// (CodeUnauthorized) to a non-builder caller — not merely hidden from tools/list.
func TestBuilderNamespace_ToolsCallRefusedForNonBuilder(t *testing.T) {
	prin := Principal{Hat: role.Product} // no workspace
	s := builderFixture(t, fixedPrin{prin})
	s.NewSession("s")
	s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))

	raw, _ := s.Dispatch("s", "tok", reqBytes(2, "tools/list", nil))
	if strings.Contains(string(raw), `"runner__run"`) {
		t.Fatalf("tools/list published the NSBuilder runner__run to a non-builder principal: %s", raw)
	}

	raw, _ = s.Dispatch("s", "tok", reqBytes(3, "tools/call", map[string]any{"name": "runner__run", "arguments": map[string]any{}}))
	var r Response
	_ = json.Unmarshal(raw, &r)
	if r.Error == nil || r.Error.Code != CodeUnauthorized {
		t.Fatalf("non-builder calling the NSBuilder runner__run: got %+v, want CodeUnauthorized", r.Error)
	}
}

// TestHiddenTool_DispatchableButNeverListed: a Hidden tool (get_tail_logs/get_sagas on the hub path,
// AC-17) is absent from tools/list but still answers tools/call — the distinguished refusal, never a
// generic "unknown tool".
func TestHiddenTool_DispatchableButNeverListed(t *testing.T) {
	prin := Principal{Hat: role.Product, Workspace: "ws_1"}
	s, err := NewServerWithAuth(fixedPrin{prin},
		Tool{
			Name: "runner__get_tail_logs", Namespace: NSBuilder, Hidden: true, Description: "d",
			InputSchema: map[string]any{"type": "object"},
			Handler: func(json.RawMessage, Principal) Outcome {
				return ToolErr(map[string]any{"error": "not available through the hub — local router only"})
			},
		},
	)
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	s.NewSession("s")
	s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))

	raw, _ := s.Dispatch("s", "tok", reqBytes(2, "tools/list", nil))
	if strings.Contains(string(raw), "runner__get_tail_logs") {
		t.Fatalf("Hidden tool published in tools/list: %s", raw)
	}
	if got := s.VisibleToolsForPrincipal(prin); len(got) != 0 {
		t.Fatalf("VisibleToolsForPrincipal includes a Hidden tool: %+v", got)
	}

	raw, _ = s.Dispatch("s", "tok", reqBytes(3, "tools/call", map[string]any{"name": "runner__get_tail_logs", "arguments": map[string]any{}}))
	if !strings.Contains(string(raw), "not available through the hub — local router only") {
		t.Fatalf("Hidden tool call did not return its distinguished refusal: %s", raw)
	}
	if strings.Contains(string(raw), "unknown tool") {
		t.Fatalf("Hidden tool call fell through to the generic unknown-tool refusal: %s", raw)
	}
}

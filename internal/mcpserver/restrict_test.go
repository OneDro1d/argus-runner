package mcpserver

// restrict_test.go — Server.Restrict (AC-F3): ONE decision per (tool, caller) that both hides the tool from tools/list
// and refuses it on tools/call, so the two can never disagree. Nil keeps every server exactly as before.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

func restrictFixture(t *testing.T, prin Principal, restrict func(Tool, Principal) *RPCError) (*Server, *int) {
	t.Helper()
	ran := 0
	h := func(json.RawMessage, Principal) Outcome { ran++; return Ok(map[string]any{"ran": true}) }
	s, err := NewServerWithAuth(fixedPrin{prin},
		Tool{Name: "author_open", Namespace: NSAuthor, Description: "d", InputSchema: map[string]any{"type": "object"}, Handler: h},
		Tool{Name: "author_secret", Namespace: NSAuthor, Description: "d", InputSchema: map[string]any{"type": "object"},
			Aliases: []string{"author__secret"}, Handler: h},
	)
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	s.Restrict = restrict
	s.NewSession("s")
	s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))
	return s, &ran
}

// the restriction under test: author_secret is refused to a caller whose Subject is "lim"
func refuseSecretToLim(t Tool, p Principal) *RPCError {
	if t.Name == "author_secret" && p.Subject == "lim" {
		return &RPCError{Code: CodeUnauthorized, Message: "author_secret is not available to lim", Data: map[string]any{"tool": t.Name}}
	}
	return nil
}

func restrictCall(t *testing.T, s *Server, name string) Response {
	t.Helper()
	raw, _ := s.Dispatch("s", "tok", reqBytes(2, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}}))
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("tools/call %s: %v %s", name, err, raw)
	}
	return r
}

func TestRestrict_HidesAndRefusesTheSameTools(t *testing.T) {
	s, ran := restrictFixture(t, Principal{Hat: role.Test, Subject: "lim"}, refuseSecretToLim)
	names := strings.Join(listedNames(t, s, "s", "tok"), ",")
	if strings.Contains(names, "author_secret") || !strings.Contains(names, "author_open") {
		t.Fatalf("tools/list for the restricted caller = %s; want author_open only", names)
	}
	if n := len(s.VisibleToolsForPrincipal(Principal{Hat: role.Test, Subject: "lim"})); n != 1 {
		t.Fatalf("VisibleToolsForPrincipal = %d tools for the restricted caller, want 1 (the same filter as tools/list)", n)
	}
	for _, name := range []string{"author_secret", "author__secret"} {
		r := restrictCall(t, s, name)
		if r.Error == nil || r.Error.Code != CodeUnauthorized || r.Error.Message != "author_secret is not available to lim" {
			t.Fatalf("tools/call %s by the restricted caller = %+v, want the Restrict answer itself (-32001, its message)", name, r.Error)
		}
	}
	if *ran != 0 {
		t.Fatalf("a refused call ran the handler %d time(s); the refusal must come before the handler", *ran)
	}
	if r := restrictCall(t, s, "author_open"); r.Error != nil || *ran != 1 {
		t.Fatalf("an unrestricted tool: error %+v, handler ran %d; want it served", r.Error, *ran)
	}
}

func TestRestrict_AnotherCallerSeesAndCallsEverything(t *testing.T) {
	s, ran := restrictFixture(t, Principal{Hat: role.Test, Subject: "full"}, refuseSecretToLim)
	names := strings.Join(listedNames(t, s, "s", "tok"), ",")
	if !strings.Contains(names, "author_secret") || !strings.Contains(names, "author_open") {
		t.Fatalf("tools/list = %s; want both tools for an unrestricted caller", names)
	}
	if r := restrictCall(t, s, "author_secret"); r.Error != nil || *ran != 1 {
		t.Fatalf("author_secret for an unrestricted caller: error %+v, ran %d", r.Error, *ran)
	}
}

func TestRestrict_NilChangesNothing(t *testing.T) {
	s, ran := restrictFixture(t, Principal{Hat: role.Test, Subject: "lim"}, nil)
	if n := len(listedNames(t, s, "s", "tok")); n != 2 {
		t.Fatalf("with no Restrict, tools/list has %d tools, want 2", n)
	}
	if r := restrictCall(t, s, "author_secret"); r.Error != nil || *ran != 1 {
		t.Fatalf("with no Restrict, author_secret: error %+v, ran %d", r.Error, *ran)
	}
}

// The namespace check runs first: a product-hat caller gets the scope refusal it always got, never the Restrict
// answer, and Restrict is not consulted for a tool the caller's hat cannot use at all.
func TestRestrict_NamespaceRefusalComesFirst(t *testing.T) {
	asked := 0
	s, _ := restrictFixture(t, Principal{Hat: role.Product, Subject: "lim"}, func(t Tool, p Principal) *RPCError {
		asked++
		return refuseSecretToLim(t, p)
	})
	r := restrictCall(t, s, "author_secret")
	if r.Error == nil || !strings.HasPrefix(r.Error.Message, "not permitted for the product scope") {
		t.Fatalf("a product-hat call = %+v, want the namespace refusal", r.Error)
	}
	if n := len(listedNames(t, s, "s", "tok")); n != 0 {
		t.Fatalf("the product hat sees %d author tools, want 0", n)
	}
	if asked != 0 {
		t.Fatalf("Restrict was consulted %d time(s) for tools the hat cannot see or call", asked)
	}
}

// The local router's per-session sets (SessionTools) go through the same one decision: a restricted tool is absent from
// the set's tools/list and refused on its tools/call, so the list and the refusal cannot disagree there either.
func TestRestrict_ASessionSetIsHiddenAndRefusedTheSameWay(t *testing.T) {
	ran := 0
	h := func(json.RawMessage, Principal) Outcome { ran++; return Ok(map[string]any{"ran": true}) }
	s, err := NewServerWithAuth(fixedPrin{Principal{Hat: role.Test, Subject: "lim"}})
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	set, skipped := NewToolSet(
		Tool{Name: "author_open", Namespace: NSAuthor, Description: "d", InputSchema: map[string]any{"type": "object"}, Handler: h},
		Tool{Name: "author_secret", Namespace: NSAuthor, Description: "d", InputSchema: map[string]any{"type": "object"}, Handler: h},
	)
	if len(skipped) != 0 {
		t.Fatalf("NewToolSet skipped %v", skipped)
	}
	s.SessionTools = func(string, Principal) SessionSet { return SessionSet{Tools: set} }
	s.Restrict = refuseSecretToLim
	s.NewSession("s")
	s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))
	names := strings.Join(listedNames(t, s, "s", "tok"), ",")
	if strings.Contains(names, "author_secret") || !strings.Contains(names, "author_open") {
		t.Fatalf("the session set's tools/list for the restricted caller = %s; want author_open only", names)
	}
	r := restrictCall(t, s, "author_secret")
	if r.Error == nil || r.Error.Message != "author_secret is not available to lim" || ran != 0 {
		t.Fatalf("tools/call author_secret from the session set = %+v (handler ran %d); want the Restrict answer, nothing run", r.Error, ran)
	}
}

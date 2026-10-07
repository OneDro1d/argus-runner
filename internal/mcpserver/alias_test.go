package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// fixedHat authenticates any token to one hat — the alias tests are about NAME resolution and scope,
// not about credential parsing.
type fixedHat struct{ hat role.Role }

func (f fixedHat) Authenticate(string) (Principal, error) { return Principal{Hat: f.hat}, nil }

func fixedAuth(r role.Role) Authenticator { return fixedHat{r} }

func aliasFixture(t *testing.T) *Server {
	t.Helper()
	s, err := NewServerWithAuth(fixedAuth(role.Test),
		Tool{
			Name: "author_thing", Namespace: NSAuthor, Description: "d",
			InputSchema: map[string]any{"type": "object"},
			Aliases:     []string{"author__thing"},
			Handler:     func(json.RawMessage, Principal) Outcome { return Ok(map[string]any{"ran": true}) },
		})
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	return s
}

// The whole point: the OLD name still works, so a caller that has not been redeployed is not broken.
func TestAlias_LegacyNameStillDispatches(t *testing.T) {
	s := aliasFixture(t)
	s.NewSession("s")
	s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))

	for _, name := range []string{"author_thing", "author__thing"} {
		raw, _ := s.Dispatch("s", "tok", reqBytes(2, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}}))
		var r Response
		_ = json.Unmarshal(raw, &r)
		if r.Error != nil {
			t.Fatalf("tools/call %q: RPC error %d %s — an alias that does not dispatch is the outage it exists to prevent",
				name, r.Error.Code, r.Error.Message)
		}
	}
}

// ...but nobody NEW learns it. An alias in tools/list would advertise two tools where there is one.
func TestAlias_IsInvisibleToToolsList(t *testing.T) {
	s := aliasFixture(t)
	s.NewSession("s")
	s.Dispatch("s", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("s", "tok", reqBytes(0, "notifications/initialized", nil))
	raw, _ := s.Dispatch("s", "tok", reqBytes(3, "tools/list", nil))
	if strings.Contains(string(raw), "author__thing") {
		t.Fatalf("tools/list published the alias: %s", raw)
	}
	if !strings.Contains(string(raw), "author_thing") {
		t.Fatalf("tools/list lost the real tool: %s", raw)
	}
	if n := len(s.VisibleToolsFor(role.Test)); n != 1 {
		t.Fatalf("VisibleToolsFor = %d tools, want 1 — the alias must not count as a tool", n)
	}
}

// The alias carries the SAME Namespace, so it is subject to the SAME scope check. A back-compat name
// that skipped the holdout would be a silent privilege escalation with a deprecation comment on it.
func TestAlias_IsNotAScopeBackDoor(t *testing.T) {
	s, err := NewServerWithAuth(fixedAuth(role.Product),
		Tool{
			Name: "author_thing", Namespace: NSAuthor, Description: "d",
			InputSchema: map[string]any{"type": "object"},
			Aliases:     []string{"author__thing"},
			Handler:     func(json.RawMessage, Principal) Outcome { return Ok(map[string]any{"ran": true}) },
		})
	if err != nil {
		t.Fatalf("NewServerWithAuth: %v", err)
	}
	s.NewSession("p")
	s.Dispatch("p", "tok", reqBytes(1, "initialize", map[string]any{"protocolVersion": ProtocolVersion}))
	s.Dispatch("p", "tok", reqBytes(0, "notifications/initialized", nil))
	for _, name := range []string{"author_thing", "author__thing"} {
		raw, _ := s.Dispatch("p", "tok", reqBytes(2, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}}))
		var r Response
		_ = json.Unmarshal(raw, &r)
		if r.Error == nil || r.Error.Code != CodeUnauthorized {
			t.Fatalf("product hat calling %q: got %+v, want CodeUnauthorized", name, r.Error)
		}
	}
}

// A collision must be refused at construction, not resolved by declaration order.
func TestAlias_CollisionIsRefusedAtConstruction(t *testing.T) {
	mk := func(name string) Tool {
		return Tool{Name: name, Namespace: NSAuthor, InputSchema: map[string]any{"type": "object"},
			Handler: func(json.RawMessage, Principal) Outcome { return Ok(nil) }}
	}
	shadowed := mk("author_b")
	shadowing := mk("author_a")
	shadowing.Aliases = []string{"author_b"}
	// Declared BEFORE the tool it would shadow — the ordering that a single-pass registration would
	// get wrong by silently overwriting.
	if _, err := NewServerWithAuth(fixedAuth(role.Test), shadowing, shadowed); err == nil {
		t.Fatal("an alias colliding with a later tool was accepted")
	}
	if _, err := NewServerWithAuth(fixedAuth(role.Test), shadowed, shadowing); err == nil {
		t.Fatal("an alias colliding with an earlier tool was accepted")
	}
}

package mcpserver

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// C8 (M25-FX4): a non-matching instance_id must be REJECTED with a clear error, not
// silently served the local instance's data (a latent multi-tenant foot-gun).
func TestInstanceIDValidated(t *testing.T) {
	env := toolcore.Env{Instance: "local", Grafana: "http://localhost:3000"}
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", rtok)
	call := func(instance string) string {
		args := map[string]any{}
		if instance != "" {
			args["instance_id"] = instance
		}
		raw, _ := s.Dispatch("s", rtok, reqBytes(1, "tools/call", map[string]any{
			"name": "runner__get_dashboard_url", "arguments": args,
		}))
		return string(raw)
	}
	// bogus instance -> a clear error naming it
	bogus := call("bogus-instance")
	if !strings.Contains(bogus, "bogus-instance") || !strings.Contains(strings.ToLower(bogus), "instance") {
		t.Errorf("non-local instance_id must be rejected with a clear error: %s", bogus)
	}
	// the correct instance works
	if ok := call("local"); !strings.Contains(ok, "argus-overview") {
		t.Errorf("instance_id=local must still work: %s", ok)
	}
	// UC031: an omitted instance_id is a structured parameter error (never silently served)
	if miss := call(""); !strings.Contains(miss, "instance_id is required") {
		t.Errorf("omitted instance_id must be rejected: %s", miss)
	}
}

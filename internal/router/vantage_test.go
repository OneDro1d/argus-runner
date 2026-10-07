package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveFromVantage_RewritesLoopbackAndKeepsEverythingElse(t *testing.T) {
	cases := []struct {
		name, in, alias, want string
	}{
		{"localhost with port+path", "http://localhost:8765/sse", "host.docker.internal", "http://host.docker.internal:8765/sse"},
		{"127.0.0.1", "http://127.0.0.1:9765/mcp", "host.docker.internal", "http://host.docker.internal:9765/mcp"},
		{"ipv6 loopback", "http://[::1]:8080/mcp", "host.docker.internal", "http://host.docker.internal:8080/mcp"},
		{"no port", "http://localhost/mcp", "host.docker.internal", "http://host.docker.internal/mcp"},
		{"query preserved", "http://localhost:8765/sse?x=1", "host.docker.internal", "http://host.docker.internal:8765/sse?x=1"},
		{"https preserved", "https://localhost:8443/mcp", "host.docker.internal", "https://host.docker.internal:8443/mcp"},

		// A real host resolves the same from either vantage. Redirecting it would be worse than a
		// failed connection: the call would succeed against the WRONG server.
		{"real host untouched", "https://argus-dev.onedroid.ai/mcp", "host.docker.internal", "https://argus-dev.onedroid.ai/mcp"},
		{"lan host untouched", "http://192.168.1.9:8765/sse", "host.docker.internal", "http://192.168.1.9:8765/sse"},
		{"service name untouched", "http://executor:8080/mcp", "host.docker.internal", "http://executor:8080/mcp"},

		// No alias = a host process, whose vantage is the one the URL was written from.
		{"no alias is no rewrite", "http://localhost:8765/sse", "", "http://localhost:8765/sse"},

		// Unparseable input is returned as-is so the failure lands at the connection, where it means
		// something, rather than being silently "repaired" into a guess.
		{"garbage untouched", "not a url", "host.docker.internal", "not a url"},
		{"empty untouched", "", "host.docker.internal", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveFromVantage(c.in, c.alias); got != c.want {
				t.Fatalf("ResolveFromVantage(%q, %q) = %q, want %q", c.in, c.alias, got, c.want)
			}
		})
	}
}

// The rewrite must NOT touch the stored state. `router status`, and any host-process router started
// later against the same state file, must still see the host's own spelling.
func TestResolveFromVantage_LeavesTheStoredStateAlone(t *testing.T) {
	st, _, _, err := UpsertFolder(State{}, FolderSpec{
		Path: "/w/test", Hat: "test", InstanceID: "suta",
		Executor: Upstream{URL: "http://localhost:8765/sse", Token: "tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = ResolveFromVantage(st.Folders[0].Upstreams["suta"].URL, "host.docker.internal")
	if got := st.Folders[0].Upstreams["suta"].URL; got != "http://localhost:8765/sse" {
		t.Fatalf("the stored URL was rewritten: %q", got)
	}
}

// End to end through the forwarder: with an alias set, the call must land on the host the alias
// names. A stub server answers on 127.0.0.1 and the alias points back at it, which is exactly the
// container's situation inverted — proving the rewrite is applied on the wire, not just computed.
func TestMCPForwarder_AppliesTheVantageOnTheWire(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "s1")
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"stub","version":"1"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:<port>. Point the alias at the same address in its OTHER spelling,
	// so a successful call proves the host component was actually replaced.
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	f := MCPForwarder{HostAlias: "localhost"}
	out, _, err := f.Forward(Target{URL: "http://127.0.0.1:" + port + "/mcp", Token: "t", Plane: "runner"}, "runner__run", json.RawMessage(`{"instance_id":"suta"}`))
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if out == nil {
		t.Fatal("no payload came back")
	}
	if !strings.HasPrefix(gotHost, "localhost:") {
		t.Fatalf("the request went to %q — the alias was not applied on the wire", gotHost)
	}
}

// The bind address is a SEPARATE question from the address clients use, and conflating them made a
// containerised router report healthy while every request from the host was reset.
func TestBindAddr_DefaultsToLoopbackAndOnlyAnExplicitValueWidensIt(t *testing.T) {
	if got := BindAddr(9765, ""); got != "127.0.0.1:9765" {
		t.Fatalf("the default bind must be loopback (VR-R2), got %q", got)
	}
	if got := BindAddr(9765, "   "); got != "127.0.0.1:9765" {
		t.Fatalf("whitespace is not a declaration, got %q", got)
	}
	// Declared explicitly by the compose unit, where the loopback restriction is enforced by the
	// `127.0.0.1:9765:9765` publish instead of by the in-container bind.
	if got := BindAddr(9765, "0.0.0.0"); got != "0.0.0.0:9765" {
		t.Fatalf("an explicit bind must be honoured, got %q", got)
	}
	// ListenAddr keeps meaning "where a client should go", which is what .mcp.json records.
	if got := ListenAddr(9765); got != "127.0.0.1:9765" {
		t.Fatalf("ListenAddr is the CLIENT's address and must stay loopback, got %q", got)
	}
}

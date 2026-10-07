package mcp

import (
	"net/url"
	"testing"
)

// Regression for a live-Memstore finding: the legacy-SSE endpoint event must be resolved
// against the SSE URL (RFC 3986), NOT concatenated as base+data — which double-prefixed
// when the base had a path (.../mcp + /mcp/message -> /mcp/mcp/message, a 404).
func TestResolveEndpoint(t *testing.T) {
	cases := []struct{ sse, data, want string }{
		// Memstore: base carries /mcp; the endpoint is root-relative + prefixed.
		{"http://localhost:8090/mcp/sse", "/mcp/message?sessionId=s1", "http://localhost:8090/mcp/message?sessionId=s1"},
		// Argus-own server at root: root-relative, no prefix.
		{"http://localhost:8765/sse", "/message?sessionId=s1", "http://localhost:8765/message?sessionId=s1"},
		// A relative endpoint resolves against the SSE path's directory.
		{"http://h:9/mcp/sse", "message?sessionId=s1", "http://h:9/mcp/message?sessionId=s1"},
		// An absolute endpoint is used as-is.
		{"http://h:9/mcp/sse", "http://h:9/other/message?sessionId=s1", "http://h:9/other/message?sessionId=s1"},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.sse)
		got, err := resolveEndpoint(u, c.data)
		if err != nil || got != c.want {
			t.Errorf("resolveEndpoint(%q,%q) = %q,%v; want %q", c.sse, c.data, got, err, c.want)
		}
	}
}

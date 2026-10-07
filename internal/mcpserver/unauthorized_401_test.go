package mcpserver

// unauthorized_401_test.go — /mcp must answer HTTP 401 + WWW-Authenticate (D-CP-MCP.AUTH /
// the MCP authorization spec §2.1, RFC 6750 §3) for a request with no bearer token, or an invalid/expired
// one — TODAY it answers 200 with a JSON-RPC -32001 error, which an HTTP-level client (or an intermediary
// that only looks at the status) reads as a successful call. The JSON-RPC error body must still ride
// along for a client that reads only that. A request with a VALID token is unchanged (200).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rawStreamablePost is streamablePost (stateless_test.go) without the forced Authorization header, so a
// no-token request can be built at all.
func rawStreamablePost(t *testing.T, url string, token string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/mcp", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func TestMCP_NoToken_401WithWWWAuthenticate(t *testing.T) {
	a := newSrv(t)
	h := NewHTTPHandler(a)
	h.WWWAuthenticate = `Bearer realm="argus"`
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, body := rawStreamablePost(t, ts.URL, "", reqBytes(1, "initialize", map[string]any{}))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401; body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="argus"` {
		t.Fatalf("WWW-Authenticate = %q, want Bearer realm=\"argus\"", got)
	}
	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode JSON-RPC body: %v (%s)", err, body)
	}
	if r.Error == nil || r.Error.Code != CodeUnauthorized {
		t.Fatalf("JSON-RPC error plane must still carry -32001 for a client that reads only that; got %+v", r.Error)
	}
}

func TestMCP_InvalidToken_401WithWWWAuthenticate(t *testing.T) {
	a := newSrv(t)
	h := NewHTTPHandler(a)
	h.WWWAuthenticate = `Bearer realm="argus"`
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, body := rawStreamablePost(t, ts.URL, "not-a-real-token", reqBytes(1, "initialize", map[string]any{}))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid/expired token: status = %d, want 401; body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="argus"` {
		t.Fatalf("WWW-Authenticate = %q, want Bearer realm=\"argus\"", got)
	}
}

// GUARD: a VALID token must see UNCHANGED behavior — 200, no WWW-Authenticate, the normal JSON-RPC
// result. The 401 path must never fire for a caller that authenticated fine.
func TestMCP_ValidRunnerToken_Unchanged200(t *testing.T) {
	a := newSrv(t)
	h := NewHTTPHandler(a)
	h.WWWAuthenticate = `Bearer realm="argus"`
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, body := rawStreamablePost(t, ts.URL, rtok, reqBytes(1, "initialize", map[string]any{}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate must be absent on a successful call; got %q", got)
	}
	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if r.Error != nil {
		t.Fatalf("valid token must not error: %+v", r.Error)
	}
}

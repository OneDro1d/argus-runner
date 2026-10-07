package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpcMethod reads the JSON-RPC method from a POST body (test helper).
func rpcMethod(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	var m struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Method
}

// streamableSUT builds a fake streamable-http MCP SUT. initFn/listFn write the response for the
// initialize and tools/list POSTs respectively; a nil fn writes a default success. It records
// whether tools/call was ever invoked (must stay false — the preflight is read-only).
func streamableSUT(t *testing.T, wantToken string, initFn, listFn func(w http.ResponseWriter)) (*httptest.Server, *bool) {
	t.Helper()
	toolCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			t.Errorf("expected Authorization: Bearer %s, got %q", wantToken, r.Header.Get("Authorization"))
		}
		switch rpcMethod(r) {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-123")
			w.Header().Set("Content-Type", "application/json")
			if initFn != nil {
				initFn(w)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"fake","version":"1"}}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			if listFn != nil {
				listFn(w)
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
		case "tools/call":
			toolCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &toolCalled
}

func TestPreflight_Streamable_OK(t *testing.T) {
	srv, toolCalled := streamableSUT(t, "smcp_good", nil, nil)
	c := &Client{ServerURL: srv.URL, Transport: Streamable, Token: "smcp_good"}
	got := c.Preflight()
	if !got.OK || got.AuthRejected || got.Unreachable {
		t.Fatalf("want OK, got %+v", got)
	}
	if *toolCalled {
		t.Fatal("preflight must NOT invoke tools/call (read-only)")
	}
}

func TestPreflight_Streamable_HTTP401AtInit(t *testing.T) {
	init401 := func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }
	srv, _ := streamableSUT(t, "", init401, nil)
	c := &Client{ServerURL: srv.URL, Transport: Streamable, Token: "bad"}
	got := c.Preflight()
	if !got.AuthRejected {
		t.Fatalf("want AuthRejected, got %+v", got)
	}
	if got.HTTPStatus != 401 {
		t.Fatalf("want HTTPStatus 401, got %d", got.HTTPStatus)
	}
}

// The exact Social gateway envelope: HTTP 200 body with a top-level JSON-RPC -32001 "Unauthorized".
func TestPreflight_Streamable_RPC32001AtInit(t *testing.T) {
	initErr := func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"Unauthorized"}}`))
	}
	srv, _ := streamableSUT(t, "", initErr, nil)
	c := &Client{ServerURL: srv.URL, Transport: Streamable, Token: "smcp_stale"}
	got := c.Preflight()
	if !got.AuthRejected {
		t.Fatalf("want AuthRejected, got %+v", got)
	}
	if got.RPCCode != -32001 {
		t.Fatalf("want RPCCode -32001, got %d", got.RPCCode)
	}
	if !strings.Contains(strings.ToLower(got.Detail), "unauthorized") {
		t.Fatalf("detail should surface the reason, got %q", got.Detail)
	}
}

// Token accepted at initialize but rejected at tools/list (a SUT that gates only past-handshake) —
// the preflight must still catch it, which is why it does tools/list, not initialize-only.
func TestPreflight_Streamable_AuthGatedAtToolsList(t *testing.T) {
	listErr := func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32001,"message":"Unauthorized"}}`))
	}
	srv, _ := streamableSUT(t, "", nil, listErr)
	c := &Client{ServerURL: srv.URL, Transport: Streamable, Token: "smcp_partial"}
	got := c.Preflight()
	if !got.AuthRejected {
		t.Fatalf("want AuthRejected at tools/list, got %+v", got)
	}
	if !strings.Contains(got.Detail, "tools/list") {
		t.Fatalf("detail should name the step, got %q", got.Detail)
	}
}

// HTTP 403 is DISTINCT from a token rejection — it must classify Forbidden (not AuthRejected), so
// onboarding does not tell the user to re-seed a token that may be fine (Origin/proxy/policy block).
func TestPreflight_Streamable_HTTP403NotAuthRejected(t *testing.T) {
	init403 := func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) }
	srv, _ := streamableSUT(t, "", init403, nil)
	c := &Client{ServerURL: srv.URL, Transport: Streamable, Token: "valid_but_wrong_origin"}
	got := c.Preflight()
	if got.AuthRejected {
		t.Fatalf("403 must NOT be AuthRejected (may be Origin/proxy, not a token issue), got %+v", got)
	}
	if !got.Forbidden || got.HTTPStatus != 403 {
		t.Fatalf("want Forbidden 403, got %+v", got)
	}
}

func TestPreflight_Streamable_Unreachable(t *testing.T) {
	// point at a definitely-closed endpoint
	c := &Client{ServerURL: "http://127.0.0.1:1/mcp", Transport: Streamable, Token: "x"}
	got := c.Preflight()
	if !got.Unreachable {
		t.Fatalf("want Unreachable, got %+v", got)
	}
	if got.AuthRejected || got.OK {
		t.Fatalf("unreachable must not be OK/AuthRejected, got %+v", got)
	}
}

// A non-auth JSON-RPC error at initialize must NOT be mislabeled as an auth rejection.
func TestPreflight_Streamable_NonAuthErrorNotRejected(t *testing.T) {
	initErr := func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`))
	}
	srv, _ := streamableSUT(t, "", initErr, nil)
	c := &Client{ServerURL: srv.URL, Transport: Streamable, Token: "smcp_good"}
	got := c.Preflight()
	if got.AuthRejected {
		t.Fatalf("a -32603 internal error must NOT be an auth rejection, got %+v", got)
	}
	if got.OK {
		t.Fatalf("a -32603 internal error must not be OK, got %+v", got)
	}
}

func TestPreflight_LegacySSE_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sse") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}))
	defer srv.Close()
	c := &Client{ServerURL: srv.URL, Transport: LegacySSE, Token: "bad"}
	got := c.Preflight()
	if !got.AuthRejected || got.HTTPStatus != 401 {
		t.Fatalf("want AuthRejected 401, got %+v", got)
	}
}

func TestPreflight_LegacySSE_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sse") {
			if r.Header.Get("Authorization") != "Bearer good" {
				t.Errorf("token not forwarded on GET /sse")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: endpoint\ndata: /message?sessionId=abc\n\n")
			return
		}
	}))
	defer srv.Close()
	c := &Client{ServerURL: srv.URL, Transport: LegacySSE, Token: "good"}
	got := c.Preflight()
	if !got.OK || got.AuthRejected {
		t.Fatalf("want OK, got %+v", got)
	}
}

package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// VR2-12 / TS2-11 (V15-006) — a non-2xx upstream is reported AS a status code.
//
// ── THE LIVE FAILURE THIS PINS ────────────────────────────────────────────────────────────────────
//
// The router's managed upstream was registered at /sse. The executor serves that path for GET only
// (internal/mcpserver/transport.go), so a POST fell through to a bare 404 with a text/plain body.
//
// An HTTP 404 is NOT a transport failure: postMCP returns a nil error and the body reads cleanly. So
// it flowed all the way to json.Unmarshal, failed there, and surfaced as:
//
//	transport: unparseable JSON-RPC response
//
// That is true about the BODY'S SHAPE and silent about the CAUSE. It described a serialisation
// problem for what was really "you asked for a URL this server does not serve", and cost a diagnosis
// round on 2026-08-12 before a direct probe of the same tunnel settled it:
//
//	POST <tunnel>/mcp -> 200 application/json, a real JSON-RPC result
//	POST <tunnel>/sse -> 404 text/plain, "404 page not found"
//
// ── WHAT THESE CASES ASSERT ───────────────────────────────────────────────────────────────────────
//
// Not merely "an error happened" — that was already true and was the problem. They assert the message
// NAMES THE STATUS, and that the old parse-shaped wording is gone, because a message that is
// technically correct and points at the wrong layer is the defect.

func TestCallStreamable_404IsReportedAsAStatusNotAParseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // exactly what the executor does for POST /sse
	}))
	defer srv.Close()

	res := (&Client{ServerURL: srv.URL + "/sse", Transport: Streamable, Token: "t"}).
		Call(CallInput{Tool: "runner__run", Args: map[string]any{"instance_id": "x"}})

	if res.TransportErr == "" {
		t.Fatal("a 404 upstream produced no transport error at all")
	}
	if !strings.Contains(res.TransportErr, "404") {
		t.Errorf("the message does not name the STATUS, which is the fact that identifies the problem.\n"+
			"  got: %q", res.TransportErr)
	}
	if strings.Contains(res.TransportErr, "unparseable") {
		t.Errorf("still reported as a parse failure — that points at the wrong layer and is what cost a\n"+
			"  diagnosis round. got: %q", res.TransportErr)
	}
}

// 401 is the case where naming the status changes what the reader DOES: a credential problem and a
// wrong-URL problem have nothing in common except that neither parses.
func TestCallStreamable_401NamesTheStatusAndCarriesTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token rejected by the gateway"))
	}))
	defer srv.Close()

	res := (&Client{ServerURL: srv.URL + "/mcp", Transport: Streamable, Token: "bad"}).
		Call(CallInput{Tool: "runner__run", Args: map[string]any{"instance_id": "x"}})

	if !strings.Contains(res.TransportErr, "401") {
		t.Errorf("the message does not name the status: %q", res.TransportErr)
	}
	if !strings.Contains(res.TransportErr, "token rejected") {
		t.Errorf("the body excerpt — the evidence for the status — did not survive: %q", res.TransportErr)
	}
}

// The excerpt must be BOUNDED and single-line. A proxy's HTML error page is thousands of bytes of no
// interest, and an unbounded excerpt turns one bad response into an unreadable log entry.
func TestHTTPStatusErr_ExcerptIsBoundedAndSingleLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>\n<head><title>502 Bad Gateway</title></head>\n" + strings.Repeat("x", 5000)))
	}))
	defer srv.Close()

	res := (&Client{ServerURL: srv.URL + "/mcp", Transport: Streamable, Token: "t"}).
		Call(CallInput{Tool: "runner__run", Args: map[string]any{"instance_id": "x"}})

	if !strings.Contains(res.TransportErr, "502") {
		t.Fatalf("status not named: %q", res.TransportErr)
	}
	if len(res.TransportErr) > 200 {
		t.Errorf("the message is %d bytes — the excerpt is not bounded: %q", len(res.TransportErr), res.TransportErr)
	}
	if strings.ContainsAny(res.TransportErr, "\r\n") {
		t.Errorf("the message spans lines, so it cannot be read in a log: %q", res.TransportErr)
	}
}

// GUARD AGAINST OVER-CORRECTION: a healthy 200 must be untouched by this change. A status check that
// also fired on success would break every working call.
func TestCallStreamable_HealthyResponseIsUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "s1")
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`))
	}))
	defer srv.Close()

	res := (&Client{ServerURL: srv.URL + "/mcp", Transport: Streamable, Token: "t"}).
		Call(CallInput{Tool: "runner__run", Args: map[string]any{"instance_id": "x"}})

	if res.TransportErr != "" {
		t.Fatalf("a healthy 200 was reported as a transport error: %q", res.TransportErr)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `{"ok":true}` {
		t.Fatalf("the payload did not survive: %#v", res.Content)
	}
}

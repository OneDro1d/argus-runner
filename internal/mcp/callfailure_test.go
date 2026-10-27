package mcp

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// AC-D70.2: a call that brings back no JSON-RPC answer is CLASSIFIED (Failure, HTTPStatus) and keeps its full text for a
// log (FailureDetail), so the local router can tell the agent what happened without quoting the address or the
// upstream's error page. The fields the runner's judge reads (Unreachable, TransportErr, TimedOut) are unchanged.

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

// serveURL serves h for this test and returns its address.
func serveURL(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL
}

// rpcServer answers initialize normally and hands tools/call to call.
func rpcServer(t *testing.T, call http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"initialize"`):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"s","version":"1"}}}`)
		case strings.Contains(string(body), `"tools/call"`):
			call(w, r)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestCallFailure_Classified(t *testing.T) {
	const page = "MARKER-PAGE first line\nsecond line"
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	for _, c := range []struct {
		name        string
		url         func() string
		transport   Transport
		timeout     time.Duration
		kind        ListFailureKind
		status      int
		unreachable bool
		timedOut    bool
		detailHas   string
		transportIs string // the TransportErr prefix, unchanged by AC-D70.2 ("" = empty)
	}{
		{name: "nothing listens", transport: Streamable, kind: ListUnreachable, unreachable: true,
			url: func() string { return "http://" + closedAddr(t) + "/mcp" }, detailHas: "initialize: Post \"http://127.0.0.1:"},
		{name: "HTTP 500 at initialize", transport: Streamable, kind: ListHTTPStatus, status: 500, detailHas: "MARKER-PAGE",
			url: func() string {
				return serveURL(t, func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(500)
					io.WriteString(w, page)
				})
			}, transportIs: "initialize: HTTP 500 from the upstream: MARKER-PAGE first line"},
		{name: "HTTP 502 at tools/call", transport: Streamable, kind: ListHTTPStatus, status: 502, detailHas: "tools/call: HTTP 502",
			url: func() string {
				return rpcServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502); io.WriteString(w, page) }).URL
			}, transportIs: "tools/call: HTTP 502 from the upstream"},
		{name: "dropped at tools/call", transport: Streamable, kind: ListUnreachable, detailHas: "tools/call: Post \"http://127.0.0.1:",
			url: func() string {
				return rpcServer(t, func(w http.ResponseWriter, _ *http.Request) {
					c, _, _ := w.(http.Hijacker).Hijack()
					c.Close()
				}).URL
			}, transportIs: "tools/call: Post"},
		{name: "timed out at tools/call", transport: Streamable, timeout: 300 * time.Millisecond, kind: ListTimedOut, timedOut: true,
			detailHas: "tools/call", url: func() string {
				return rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-hang:
					case <-r.Context().Done():
					}
				}).URL
			}, transportIs: "tools/call: Post"},
		// The headers come, the body never does: the READ timed out. TimedOut stays false here, as before AC-D70.2
		// (the judge's field is set only for the send); Failure says what happened.
		{name: "body never comes at tools/call", transport: Streamable, timeout: 300 * time.Millisecond, kind: ListTimedOut,
			detailHas: "tools/call read: ", url: func() string {
				return rpcServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					select {
					case <-hang:
					case <-r.Context().Done():
					}
				}).URL
			}, transportIs: "tools/call read: "},
		{name: "not JSON-RPC", transport: Streamable, kind: ListBadAnswer, detailHas: "unparseable",
			url: func() string {
				return rpcServer(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "not json") }).URL
			}, transportIs: "unparseable JSON-RPC response"},
		{name: "legacy: nothing listens", transport: LegacySSE, kind: ListUnreachable, unreachable: true,
			url: func() string { return "http://" + closedAddr(t) }, detailHas: "GET /sse: Get \"http://127.0.0.1:"},
		{name: "legacy: GET /sse 404", transport: LegacySSE, kind: ListHTTPStatus, status: 404, detailHas: "GET /sse status 404",
			url: func() string {
				return serveURL(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
			}, transportIs: "GET /sse status 404"},
		{name: "undeclared transport", transport: "carrier-pigeon", kind: ListBadAnswer, detailHas: "carrier-pigeon",
			url: func() string { return "http://127.0.0.1:9/mcp" }, transportIs: "unknown/undeclared transport"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := c.url()
			res := (&Client{ServerURL: u, Transport: c.transport, Timeout: c.timeout}).Call(CallInput{Tool: "x"})
			if res.Failure != c.kind || res.HTTPStatus != c.status || res.Unreachable != c.unreachable || res.TimedOut != c.timedOut {
				t.Fatalf("got Failure %q HTTPStatus %d Unreachable %t TimedOut %t — want %q %d %t %t", res.Failure, res.HTTPStatus,
					res.Unreachable, res.TimedOut, c.kind, c.status, c.unreachable, c.timedOut)
			}
			if !strings.Contains(res.FailureDetail, c.detailHas) {
				t.Fatalf("FailureDetail %q lacks %q", res.FailureDetail, c.detailHas)
			}
			if c.transportIs == "" && res.TransportErr != "" || !strings.HasPrefix(res.TransportErr, c.transportIs) {
				t.Fatalf("TransportErr %q — want it unchanged (prefix %q)", res.TransportErr, c.transportIs)
			}
		})
	}
}

func TestCallFailure_AnAnswerIsNoFailure(t *testing.T) {
	ts := rpcServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"no such tool"}}`)
	})
	res := (&Client{ServerURL: ts.URL, Transport: Streamable}).Call(CallInput{Tool: "x"})
	if res.Failure != "" || res.HTTPStatus != 0 || res.FailureDetail != "" || res.JSONRPCError == nil {
		t.Fatalf("a JSON-RPC answer was classified as a failure: %+v", res)
	}
}

// CapForLog never cuts inside a UTF-8 character, and says how long the text was.
func TestCapForLog(t *testing.T) {
	for _, c := range []struct {
		s    string
		max  int
		want string
	}{
		{"abc", 3, "abc"},                    // within the bound: as it is
		{"abcd", 3, "abc… (4 bytes in all)"}, // cut at the bound
		{"aé", 2, "a… (3 bytes in all)"},     // the bound falls inside é (2 bytes): cut before it
		{"a€b", 3, "a… (5 bytes in all)"},    // inside € (3 bytes)
		{"a€b", 4, "a€… (5 bytes in all)"},   // right after €
		{"éé", 0, "… (4 bytes in all)"},      // nothing fits
	} {
		got := CapForLog(c.s, c.max)
		if got != c.want || !utf8.ValidString(got) {
			t.Errorf("CapForLog(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
	}
}

// The logged list failure is bounded: a server's JSON-RPC message (up to 16 MiB in one read) is cut to 1 KiB.
func TestListFailure_ErrorCapsALongRPCMessage(t *testing.T) {
	long := strings.Repeat("é", 4000) + "THE-END"
	got := (&ListFailure{Kind: ListRPCError, Stage: "tools/list", RPCCode: -32603, RPCMessage: long}).Error()
	if len(got) > 1200 || strings.Contains(got, "THE-END") || !utf8.ValidString(got) || !strings.Contains(got, fmt.Sprintf("(%d bytes in all)", len(long))) {
		t.Fatalf("not capped (%d bytes): %.200q…", len(got), got)
	}
	if !strings.HasPrefix(got, `tools/list: JSON-RPC error -32603 "éé`) {
		t.Fatalf("the start changed: %.80q", got)
	}
	// A short message is exactly as before.
	if got := (&ListFailure{Kind: ListRPCError, Stage: "tools/list", RPCCode: -32603, RPCMessage: "internal"}).Error(); got != `tools/list: JSON-RPC error -32603 "internal"` {
		t.Fatalf("a short message changed: %q", got)
	}
	exact := strings.Repeat("a", maxLoggedRPCMessage)
	if got := (&ListFailure{Kind: ListRPCError, Stage: "s", RPCCode: 1, RPCMessage: exact}).Error(); strings.Contains(got, "bytes in all") {
		t.Fatalf("a message of exactly the bound was cut")
	}
	// An HTTP status whose body carried a JSON-RPC error: the same bound.
	if got := (&ListFailure{Kind: ListHTTPStatus, Stage: "s", HTTPStatus: 401, Detail: long}).Error(); len(got) > 1200 {
		t.Fatalf("a long Detail is not capped (%d bytes)", len(got))
	}
}

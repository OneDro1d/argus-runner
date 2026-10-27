package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync/atomic"
	"testing"
)

// The client does not follow redirects when told not to, and AnswerMatches reads both places an
// "unavailable" answer can be.

func okMCP(hits *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			atomic.AddInt32(hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "ok"}}}})
		}
	}
}

func TestClient_NoRedirects(t *testing.T) {
	var hits int32
	other := httptest.NewServer(okMCP(&hits))
	defer other.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer front.Close()

	follow := &Client{ServerURL: front.URL + "/mcp", Transport: Streamable}
	if r := follow.Call(CallInput{Tool: "t", Args: map[string]any{}, RequestID: "r1"}); r.TransportErr != "" || r.Unreachable || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("control, today's behaviour: the redirect is followed: %+v / %d", r, atomic.LoadInt32(&hits))
	}

	atomic.StoreInt32(&hits, 0)
	for name, cl := range map[string]*Client{
		"default client":  {ServerURL: front.URL + "/mcp", Transport: Streamable, NoRedirects: true},
		"explicit client": {ServerURL: front.URL + "/mcp", Transport: Streamable, NoRedirects: true, HTTP: &http.Client{}},
	} {
		r := cl.Call(CallInput{Tool: "t", Args: map[string]any{}, RequestID: "r2"})
		if r.TransportErr == "" && !r.Unreachable {
			t.Errorf("%s: a 3xx reads as the transport failure it is: %+v", name, r)
		}
		if v := Judge(r, Expect{ErrorPlane: PlaneNone}); v.Pass {
			t.Errorf("%s: a redirect is never a pass: %+v", name, v)
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("nothing reached the other host: %d", n)
	}
}

func TestAnswerMatches(t *testing.T) {
	re := regexp.MustCompile(`database_unavailable|internal server error`)
	for name, tc := range map[string]struct {
		r    CallResult
		want bool
	}{
		"content text":         {CallResult{Content: []ContentBlock{{Text: "x database_unavailable y"}}}, true},
		"second content block": {CallResult{Content: []ContentBlock{{Text: "fine"}, {Text: "internal server error"}}}, true},
		"json-rpc message":     {CallResult{JSONRPCError: &RPCError{Code: -32000, Message: "internal server error"}}, true},
		"json-rpc data":        {CallResult{JSONRPCError: &RPCError{Code: -32000, Message: "bad", Data: json.RawMessage(`"database_unavailable"`)}}, true},
		"neither":              {CallResult{Content: []ContentBlock{{Text: "rows"}}, JSONRPCError: &RPCError{Message: "nope"}}, false},
		"empty":                {CallResult{}, false},
	} {
		if got := AnswerMatches(tc.r, re); got != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
	if AnswerMatches(CallResult{Content: []ContentBlock{{Text: "database_unavailable"}}}, nil) {
		t.Error("no pattern, no match")
	}
}

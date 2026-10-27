package mcp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// AC-D69 (B3): ListTools reads a server's tool list — the same handshake as Call, then tools/list — so the local
// router can pass on the tools a control plane (and, in part 2, an executor) defines instead of keeping its own list.
// These tests drive the suite's OWN tool server wherever they can: the list the router passes on must be the list that
// server actually sends, not what a stub thinks it sends.

const (
	listRunnerTok = "list-runner-token-111"
	listAuthorTok = "list-author-token-222"
)

// listFixtureTools is a small but realistic set: a closed schema with a nested array of closed objects and numeric
// bounds (the shapes author_create_comparison uses), plus a tool only the test hat may see.
func listFixtureTools() []mcpserver.Tool {
	nested := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"instance_id", "members"},
		"properties": map[string]any{
			"instance_id": map[string]any{"type": "string", "description": "the instance"},
			"members": map[string]any{
				"type": "array", "minItems": 2, "maxItems": 12,
				"items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"name", "role"},
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
						"role": map[string]any{"type": "string", "description": "<reference> & \"candidate\""},
					},
				},
			},
		},
	}
	ok := func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome { return mcpserver.Ok(map[string]any{}) }
	return []mcpserver.Tool{
		{Name: "author_create_thing", Namespace: mcpserver.NSAuthor, Description: "Create a thing — with a long, specific text.",
			InputSchema: nested, Handler: ok, Aliases: []string{"author__create_thing"}},
		{Name: "author_list_things", Namespace: mcpserver.NSAuthor, Description: "List things.",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"instance_id"},
				"properties": map[string]any{"instance_id": map[string]any{"type": "string"}}}, Handler: ok},
		{Name: "author_hidden_thing", Namespace: mcpserver.NSAuthor, Hidden: true, Description: "never listed",
			InputSchema: map[string]any{"type": "object"}, Handler: ok},
	}
}

func listServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, err := mcpserver.NewServer(auth.Config{RunnerToken: listRunnerTok, AuthorToken: listAuthorTok}, listFixtureTools()...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpserver.NewHTTPHandler(srv))
	t.Cleanup(ts.Close)
	return ts
}

// directToolsList asks the same server for its tools/list over a hand-written handshake, so the comparison below is
// against the server's own bytes — not against ListTools' idea of them.
func directToolsList(t *testing.T, base, token string) []map[string]json.RawMessage {
	t.Helper()
	post := func(sid string, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	sid := r1.Header.Get("Mcp-Session-Id")
	r1.Body.Close()
	post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`).Body.Close()
	r3 := post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	defer r3.Body.Close()
	var out struct {
		Result struct {
			Tools []map[string]json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(r3.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Result.Tools
}

func assertSameAsDirect(t *testing.T, got mcp.ListResult, direct []map[string]json.RawMessage) {
	t.Helper()
	if got.Failure != nil {
		t.Fatalf("ListTools failed: %+v", got.Failure)
	}
	if len(got.Tools) != len(direct) {
		t.Fatalf("ListTools returned %d tools, the server lists %d", len(got.Tools), len(direct))
	}
	for i, d := range direct {
		var name, desc string
		_ = json.Unmarshal(d["name"], &name)
		_ = json.Unmarshal(d["description"], &desc)
		g := got.Tools[i]
		if g.Name != name || g.Description != desc {
			t.Errorf("tool %d: got (%q, %q), the server lists (%q, %q)", i, g.Name, g.Description, name, desc)
		}
		if !bytes.Equal(g.InputSchema, d["inputSchema"]) {
			t.Errorf("tool %q: inputSchema differs from the server's own bytes\n got: %s\nwant: %s", name, g.InputSchema, d["inputSchema"])
		}
	}
}

func TestListTools_Streamable_IsTheServersOwnList(t *testing.T) {
	ts := listServer(t)
	cl := &mcp.Client{ServerURL: ts.URL + "/mcp", Transport: mcp.Streamable, Token: listAuthorTok}
	got := cl.ListTools()
	assertSameAsDirect(t, got, directToolsList(t, ts.URL, listAuthorTok))
	if len(got.Tools) != 2 {
		t.Fatalf("want the 2 listed tools (the hidden one and the alias are never listed), got %d", len(got.Tools))
	}
	if got.Server.Name != "argus" || got.Server.Version != "M2.5" {
		t.Errorf("serverInfo = %+v, want the server's own initialize answer {argus M2.5}", got.Server)
	}
}

func TestListTools_LegacySSE_IsTheServersOwnList(t *testing.T) {
	ts := listServer(t)
	cl := &mcp.Client{ServerURL: ts.URL, Transport: mcp.LegacySSE, Token: listAuthorTok}
	got := cl.ListTools()
	assertSameAsDirect(t, got, directToolsList(t, ts.URL, listAuthorTok))
	if got.Server.Name != "argus" {
		t.Errorf("serverInfo = %+v, want the server's own initialize answer", got.Server)
	}
}

// The list a caller gets depends on its token: the product hat's list has no author tool, and ListTools must report
// that list as it is — an empty list is an answer, not a failure.
func TestListTools_AnEmptyListIsAnAnswerNotAFailure(t *testing.T) {
	ts := listServer(t)
	got := (&mcp.Client{ServerURL: ts.URL + "/mcp", Transport: mcp.Streamable, Token: listRunnerTok}).ListTools()
	if got.Failure != nil {
		t.Fatalf("an empty list was reported as a failure: %+v", got.Failure)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("the product hat's list should be empty here, got %d tools", len(got.Tools))
	}
}

// pagedServer serves tools/list in pages through nextCursor (MCP's paging), and records each cursor it received.
type pagedServer struct {
	mu      sync.Mutex
	cursors []string
	pages   map[string]string // cursor -> result JSON ("" = first page)
}

func (p *pagedServer) handler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "paged")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"paged","version":"1.2.3"}}}`, req.ID)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		p.mu.Lock()
		p.cursors = append(p.cursors, req.Params.Cursor)
		page := p.pages[req.Params.Cursor]
		p.mu.Unlock()
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, page)
	}
}

func TestListTools_FollowsNextCursorAcrossPages(t *testing.T) {
	ps := &pagedServer{pages: map[string]string{
		"":   `{"tools":[{"name":"a_one","description":"1","inputSchema":{"type":"object"}}],"nextCursor":"p2"}`,
		"p2": `{"tools":[{"name":"a_two","description":"2","inputSchema":{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993}}}}]}`,
	}}
	ts := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer ts.Close()
	got := (&mcp.Client{ServerURL: ts.URL, Transport: mcp.Streamable}).ListTools()
	if got.Failure != nil {
		t.Fatalf("paged list failed: %+v", got.Failure)
	}
	if len(got.Tools) != 2 || got.Tools[0].Name != "a_one" || got.Tools[1].Name != "a_two" {
		t.Fatalf("want both pages in order, got %+v", got.Tools)
	}
	if len(ps.cursors) != 2 || ps.cursors[0] != "" || ps.cursors[1] != "p2" {
		t.Fatalf("cursors sent = %q, want [\"\" \"p2\"]", ps.cursors)
	}
	// A number beyond float64 precision must survive: the schema is kept as raw bytes, never decoded.
	if !strings.Contains(string(got.Tools[1].InputSchema), "9007199254740993") {
		t.Errorf("the schema's number was altered: %s", got.Tools[1].InputSchema)
	}
	if got.Server.Name != "paged" || got.Server.Version != "1.2.3" {
		t.Errorf("serverInfo = %+v", got.Server)
	}
}

// A server that hands back the same cursor forever would make the list endless; that is a bad answer, not a list.
func TestListTools_ARepeatedCursorIsABadAnswer(t *testing.T) {
	ps := &pagedServer{pages: map[string]string{
		"":     `{"tools":[],"nextCursor":"loop"}`,
		"loop": `{"tools":[{"name":"x","description":"","inputSchema":{"type":"object"}}],"nextCursor":"loop"}`,
	}}
	ts := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer ts.Close()
	got := (&mcp.Client{ServerURL: ts.URL, Transport: mcp.Streamable}).ListTools()
	if got.Failure == nil || got.Failure.Kind != mcp.ListBadAnswer {
		t.Fatalf("want a bad-answer failure, got %+v", got.Failure)
	}
	if len(got.Tools) != 0 {
		t.Errorf("a failed read must return no partial list, got %d tools", len(got.Tools))
	}
}

func TestListTools_UnreachableIsClassified(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens there now
	for _, tr := range []mcp.Transport{mcp.Streamable, mcp.LegacySSE} {
		got := (&mcp.Client{ServerURL: "http://" + addr + "/mcp", Transport: tr, Timeout: 2 * time.Second}).ListTools()
		if got.Failure == nil || got.Failure.Kind != mcp.ListUnreachable {
			t.Fatalf("%s: want unreachable, got %+v", tr, got.Failure)
		}
	}
}

// The deadline covers the WHOLE read: a server that answers initialize at once and then never answers tools/list must
// still time out, as must one that never answers at all.
func TestListTools_TimedOutIsClassified(t *testing.T) {
	// block is closed BEFORE the servers are: httptest.Server.Close waits for every handler, and these handlers wait
	// on block. (A handler that has not read the request body never sees the client hang up either, so each one reads
	// it first.)
	block := make(chan struct{})
	slowList := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case bytes.Contains(body, []byte(`"initialize"`)):
			w.Header().Set("Mcp-Session-Id", "s")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"s","version":"1"}}}`)
		case bytes.Contains(body, []byte(`"tools/list"`)):
			// Hang until the CLIENT gives up (or the test ends) — watching the request's context keeps
			// httptest.Server.Close from waiting forever on this handler.
			select {
			case <-block:
			case <-r.Context().Done():
			}
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(block)
		slowList.Close()
		silent.Close()
	}()

	for name, url := range map[string]string{"slow tools/list": slowList.URL, "silent": silent.URL} {
		start := time.Now()
		got := (&mcp.Client{ServerURL: url, Transport: mcp.Streamable, Timeout: 300 * time.Millisecond}).ListTools()
		if got.Failure == nil || got.Failure.Kind != mcp.ListTimedOut {
			t.Fatalf("%s: want timed-out, got %+v", name, got.Failure)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("%s: the read took %v — the deadline is not applied to the whole read", name, took)
		}
	}
}

// The deadline is ONE deadline for the whole read, not one per request: a server whose every answer comes within the
// timeout, but whose handshake plus pages together do not, must time out. (A per-request timeout alone would let a slow
// control plane hold a session start for several timeouts — past the MCP client's own connect timeout.)
func TestListTools_TheDeadlineCoversTheWholeReadNotEachRequest(t *testing.T) {
	const step = 150 * time.Millisecond // every single answer is well inside the 300 ms timeout…
	answer := func(method string, id json.RawMessage, cursor string) string {
		switch method {
		case "initialize":
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"s","version":"1"}}}`, id)
		case "tools/list":
			if cursor == "" {
				return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"a","description":"","inputSchema":{"type":"object"}}],"nextCursor":"p2"}}`, id)
			}
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"b","description":"","inputSchema":{"type":"object"}}]}}`, id)
		}
		return ""
	}
	type rpcIn struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}

	// Streamable: each POST answers after `step`.
	streamable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in rpcIn
		_ = json.NewDecoder(r.Body).Decode(&in)
		select {
		case <-time.After(step):
		case <-r.Context().Done():
			return
		}
		if a := answer(in.Method, in.ID, in.Params.Cursor); a != "" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, a)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer streamable.Close()

	// Legacy SSE: the stream answers each POST `step` after it arrives.
	events := make(chan string, 8)
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "event: endpoint\ndata: /message?sessionId=x\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case ev := <-events:
					io.WriteString(w, "event: message\ndata: "+ev+"\n\n")
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case r.Method == http.MethodPost:
			var in rpcIn
			_ = json.NewDecoder(r.Body).Decode(&in)
			select {
			case <-time.After(step):
			case <-r.Context().Done():
				return
			}
			if a := answer(in.Method, in.ID, in.Params.Cursor); a != "" {
				events <- a
			}
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer legacy.Close()

	for name, cl := range map[string]*mcp.Client{
		"streamable": {ServerURL: streamable.URL, Transport: mcp.Streamable, Timeout: 300 * time.Millisecond},
		"legacy-sse": {ServerURL: legacy.URL, Transport: mcp.LegacySSE, Timeout: 300 * time.Millisecond},
	} {
		start := time.Now()
		got := cl.ListTools()
		took := time.Since(start)
		if got.Failure == nil || got.Failure.Kind != mcp.ListTimedOut {
			t.Fatalf("%s: …but the four together are not; want timed-out after about 300 ms, got %+v (%d tools) after %v", name, got.Failure, len(got.Tools), took)
		}
		if took > 2*time.Second {
			t.Fatalf("%s: the read took %v", name, took)
		}
	}
}

func TestListTools_RefusedTokenIsAnAuthRefusal(t *testing.T) {
	ts := listServer(t)
	for _, tr := range []mcp.Transport{mcp.Streamable, mcp.LegacySSE} {
		url := ts.URL
		if tr == mcp.Streamable {
			url += "/mcp"
		}
		got := (&mcp.Client{ServerURL: url, Transport: tr, Token: "not-a-token", Timeout: 3 * time.Second}).ListTools()
		if got.Failure == nil || got.Failure.Kind != mcp.ListHTTPStatus || got.Failure.HTTPStatus != http.StatusUnauthorized {
			t.Fatalf("%s: want http-status 401, got %+v", tr, got.Failure)
		}
		if !got.Failure.AuthRefused() {
			t.Fatalf("%s: a 401 must read as the token being refused", tr)
		}
		if strings.Contains(got.Failure.Detail, "not-a-token") || strings.Contains(got.Failure.Error(), "not-a-token") {
			t.Fatalf("%s: the failure text carries the token: %q", tr, got.Failure.Error())
		}
		// The 401 body is the only place that says WHICH authentication failure it was; it must survive the read.
		if got.Failure.RPCCode != -32001 || !strings.Contains(got.Failure.RPCMessage, "unauthorized") {
			t.Fatalf("%s: the 401's JSON-RPC body was not kept: code %d message %q", tr, got.Failure.RPCCode, got.Failure.RPCMessage)
		}
	}
}

// One read is bounded: a server that lists more tools than any real one would is a bad answer, not a list to hold.
func TestListTools_AnOversizedListIsABadAnswer(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"jsonrpc":"2.0","id":2,"result":{"tools":[`)
	for i := 0; i < 1001; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"t%d","description":"","inputSchema":{"type":"object"}}`, i)
	}
	b.WriteString(`]}}`)
	ts := rpcServer(okInit, b.String())
	defer ts.Close()
	got := (&mcp.Client{ServerURL: ts.URL, Transport: mcp.Streamable}).ListTools()
	if got.Failure == nil || got.Failure.Kind != mcp.ListBadAnswer {
		t.Fatalf("1001 tools: want bad-answer, got %+v (%d tools)", got.Failure, len(got.Tools))
	}
}

func statusServer(code int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
}

func TestListTools_HTTPStatusesAreClassified(t *testing.T) {
	cases := []struct {
		code int
		auth bool
	}{{http.StatusForbidden, true}, {http.StatusServiceUnavailable, false}, {http.StatusNotFound, false}, {http.StatusInternalServerError, false}}
	for _, c := range cases {
		ts := statusServer(c.code, "<html>proxy page</html>")
		got := (&mcp.Client{ServerURL: ts.URL, Transport: mcp.Streamable}).ListTools()
		ts.Close()
		if got.Failure == nil || got.Failure.Kind != mcp.ListHTTPStatus || got.Failure.HTTPStatus != c.code {
			t.Fatalf("HTTP %d: want http-status %d, got %+v", c.code, c.code, got.Failure)
		}
		if got.Failure.AuthRefused() != c.auth {
			t.Errorf("HTTP %d: AuthRefused = %v, want %v", c.code, got.Failure.AuthRefused(), c.auth)
		}
	}
}

func rpcServer(initResult, listAnswer string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case bytes.Contains(body, []byte(`"initialize"`)):
			io.WriteString(w, initResult)
		case bytes.Contains(body, []byte(`"tools/list"`)):
			io.WriteString(w, listAnswer)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
}

const okInit = `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"s","version":"1"}}}`

func TestListTools_JSONRPCErrorsAreClassified(t *testing.T) {
	cases := []struct {
		name       string
		init, list string
		code       int
		auth       bool
	}{
		{"list refused with -32603", okInit, `{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"internal"}}`, -32603, false},
		{"list refused with -32001", okInit, `{"jsonrpc":"2.0","id":2,"error":{"code":-32001,"message":"unauthorized: no valid credential"}}`, -32001, true},
		{"initialize refused", `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"bad"}}`, `{}`, -32600, false},
	}
	for _, c := range cases {
		ts := rpcServer(c.init, c.list)
		got := (&mcp.Client{ServerURL: ts.URL, Transport: mcp.Streamable}).ListTools()
		ts.Close()
		if got.Failure == nil || got.Failure.Kind != mcp.ListRPCError || got.Failure.RPCCode != c.code {
			t.Fatalf("%s: want jsonrpc %d, got %+v", c.name, c.code, got.Failure)
		}
		if got.Failure.AuthRefused() != c.auth {
			t.Errorf("%s: AuthRefused = %v, want %v", c.name, got.Failure.AuthRefused(), c.auth)
		}
	}
}

func TestListTools_UnreadableAnswersAreBadAnswers(t *testing.T) {
	for name, list := range map[string]string{
		"not JSON":            `<html>oops</html>`,
		"no result, no error": `{"jsonrpc":"2.0","id":2}`,
		"tools is not a list": `{"jsonrpc":"2.0","id":2,"result":{"tools":{"a":1}}}`,
		"tools missing":       `{"jsonrpc":"2.0","id":2,"result":{}}`,
	} {
		ts := rpcServer(okInit, list)
		got := (&mcp.Client{ServerURL: ts.URL, Transport: mcp.Streamable}).ListTools()
		ts.Close()
		if got.Failure == nil || got.Failure.Kind != mcp.ListBadAnswer {
			t.Fatalf("%s: want bad-answer, got %+v", name, got.Failure)
		}
		if got.Failure.AuthRefused() {
			t.Errorf("%s: a bad answer is not a token refusal", name)
		}
	}
}

func TestListTools_UndeclaredTransportIsAFailureNotAGuess(t *testing.T) {
	got := (&mcp.Client{ServerURL: "http://127.0.0.1:1/mcp"}).ListTools()
	if got.Failure == nil {
		t.Fatal("an undeclared transport produced a list")
	}
}

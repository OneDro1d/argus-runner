package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// ListTools (AC-D69, design B3) reads a server's tool list: the same handshake as Call (initialize, then
// notifications/initialized), then tools/list, following nextCursor when the server pages the list.
//
// It exists for the local router, which passes on the tools a control plane (and, in part 2, each executor) DEFINES
// instead of keeping its own list of them. So it returns each tool as the server sent it — the schema as raw JSON, never
// decoded and re-encoded here — and the server's serverInfo, which the router needs to tell executor versions apart.
//
// A failure is CLASSIFIED rather than flattened into a string, because the router must tell the agent which of several
// different things happened (A6): the server could not be reached or did not answer in time; it refused the token; it
// answered with an error; or it answered with something that is not a tool list. One deadline covers the whole read —
// every request of the handshake and every page — so a server that answers initialize and then hangs cannot stall the
// caller past it.
//
// All or nothing: a failure on any page returns no tools at all. A partial list would look like a complete one.
func (c *Client) ListTools() ListResult {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch c.Transport {
	case Streamable:
		return c.listStreamable(ctx)
	case LegacySSE:
		return c.listLegacySSE(ctx)
	default:
		return ListResult{Failure: &ListFailure{Kind: ListBadAnswer, Stage: "transport",
			Detail: "unknown/undeclared transport " + string(c.Transport)}}
	}
}

// ListedTool is one entry of a tools/list answer, kept as the server sent it.
type ListedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ServerInfo is the serverInfo a server returns from initialize.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ListResult is what ListTools read. Exactly one of (Tools, Server) and Failure is meaningful: on a failure Tools is nil.
type ListResult struct {
	Tools   []ListedTool
	Server  ServerInfo
	Failure *ListFailure
}

// ListFailureKind says why a list could not be read.
type ListFailureKind string

const (
	// ListUnreachable: no HTTP answer at all — refused connection, DNS failure, reset. The server was never heard from.
	ListUnreachable ListFailureKind = "unreachable"
	// ListTimedOut: the deadline elapsed before the whole list was read.
	ListTimedOut ListFailureKind = "timed-out"
	// ListHTTPStatus: the server answered with an HTTP status outside 2xx (HTTPStatus).
	ListHTTPStatus ListFailureKind = "http-status"
	// ListRPCError: the server answered with a JSON-RPC error (RPCCode, RPCMessage).
	ListRPCError ListFailureKind = "jsonrpc"
	// ListBadAnswer: an answer that is not a readable tools/list result (not JSON-RPC, no result, tools not a list,
	// a cursor that never ends, an undeclared transport).
	ListBadAnswer ListFailureKind = "bad-answer"
)

// ListFailure classifies a failed ListTools. Detail is a short description for logs; it NEVER contains the token.
type ListFailure struct {
	Kind       ListFailureKind
	Stage      string // "initialize" | "tools/list" | "GET /sse" | "transport"
	HTTPStatus int    // set when Kind is ListHTTPStatus
	// RPCCode and RPCMessage are the JSON-RPC error the server sent: always set when Kind is ListRPCError, and ALSO set
	// for a ListHTTPStatus whose body carried one. The second case matters: Argus's own server answers every failed
	// authentication with HTTP 401 plus a -32001 body, and the body is the only place that says WHICH failure it was
	// (a token it does not know, or a token store it could not reach).
	RPCCode    int
	RPCMessage string
	Detail     string
}

// maxLoggedRPCMessage bounds each free-text part of Error() — the server's JSON-RPC message and Detail (AC-D70.2): one
// read may hold up to maxListBytes, and Error() is what the router writes to its log.
const maxLoggedRPCMessage = 1024

func (f *ListFailure) Error() string {
	switch f.Kind {
	case ListHTTPStatus:
		return fmt.Sprintf("%s: HTTP %d: %s", f.Stage, f.HTTPStatus, CapForLog(f.Detail, maxLoggedRPCMessage))
	case ListRPCError:
		return fmt.Sprintf("%s: JSON-RPC error %d %q", f.Stage, f.RPCCode, CapForLog(f.RPCMessage, maxLoggedRPCMessage))
	default:
		return fmt.Sprintf("%s: %s: %s", f.Stage, f.Kind, CapForLog(f.Detail, maxLoggedRPCMessage))
	}
}

// CapForLog cuts s to at most max bytes for a log line, never inside a UTF-8 character, and says how long it was:
// "<first bytes>… (<n> bytes in all)". A text within the bound is returned as it is.
func CapForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… (%d bytes in all)", s[:cut], len(s))
}

// AuthRefused reports whether the server refused the CREDENTIAL: HTTP 401 or 403, or the MCP Unauthorized code -32001
// — the same rule Preflight applies (classifyHTTPAuth / isAuthRejection), minus the message keywords: a list read is
// classified by status and code only, so a server's free-text message cannot move it between causes.
func (f *ListFailure) AuthRefused() bool {
	if f == nil {
		return false
	}
	switch f.Kind {
	case ListHTTPStatus:
		return f.HTTPStatus == http.StatusUnauthorized || f.HTTPStatus == http.StatusForbidden
	case ListRPCError:
		return f.RPCCode == -32001
	}
	return false
}

// The bounds of ONE read, across every page. Argus's own servers never page and send well under 1 MiB; the bounds only
// stop a server that keeps handing out cursors, or keeps sending, from making one session start endless or unbounded in
// memory. Going over a bound is a bad answer, never a truncated list.
const (
	maxListPages = 100
	maxListBytes = 16 << 20
	maxListTools = 1000
)

// transportFailure classifies an error from sending a request or reading its answer.
func transportFailure(ctx context.Context, stage string, err error) *ListFailure {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || isTimeoutErr(err) {
		return &ListFailure{Kind: ListTimedOut, Stage: stage, Detail: "no complete answer before the deadline"}
	}
	return &ListFailure{Kind: ListUnreachable, Stage: stage, Detail: err.Error()}
}

// statusFailure classifies a non-2xx answer. It reads at most 4 KiB of the body: enough for a JSON-RPC error object, and
// the first line of it becomes Detail (a proxy's HTML page is thousands of bytes of no interest — httpStatusErr's rule).
func statusFailure(resp *http.Response, stage string) *ListFailure {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	f := &ListFailure{Kind: ListHTTPStatus, Stage: stage, HTTPStatus: resp.StatusCode, Detail: excerpt(body)}
	var a rpcAnswer
	if json.Unmarshal(body, &a) == nil && a.Error != nil {
		f.RPCCode, f.RPCMessage = a.Error.Code, a.Error.Message
	}
	return f
}

// excerpt is the first line of a body, at most 100 bytes — the line that names the problem.
func excerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// boundedBody caps what one answer may make this read hold in memory. The SSE branch of readJSONRPC has no cap of its own.
func boundedBody(resp *http.Response) {
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, maxListBytes), resp.Body}
}

// rpcAnswer is one JSON-RPC answer as ListTools reads it.
type rpcAnswer struct {
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// decodeAnswer reads one answer body; a JSON-RPC error and a missing result are classified here.
func decodeAnswer(raw []byte, stage string) (json.RawMessage, *ListFailure) {
	var a rpcAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, &ListFailure{Kind: ListBadAnswer, Stage: stage, Detail: "not a JSON-RPC answer"}
	}
	if a.Error != nil {
		return nil, &ListFailure{Kind: ListRPCError, Stage: stage, RPCCode: a.Error.Code, RPCMessage: a.Error.Message}
	}
	if len(a.Result) == 0 || bytes.Equal(bytes.TrimSpace(a.Result), []byte("null")) {
		return nil, &ListFailure{Kind: ListBadAnswer, Stage: stage, Detail: "neither a result nor an error"}
	}
	return a.Result, nil
}

func decodeServerInfo(result json.RawMessage) ServerInfo {
	var r struct {
		ServerInfo ServerInfo `json:"serverInfo"`
	}
	_ = json.Unmarshal(result, &r)
	return r.ServerInfo
}

// pageOf reads one tools/list result: its tools (raw schemas kept) and its nextCursor.
func pageOf(result json.RawMessage) ([]ListedTool, string, *ListFailure) {
	var r struct {
		Tools      *json.RawMessage `json:"tools"`
		NextCursor string           `json:"nextCursor"`
	}
	if err := json.Unmarshal(result, &r); err != nil || r.Tools == nil {
		return nil, "", &ListFailure{Kind: ListBadAnswer, Stage: "tools/list", Detail: "the result holds no tools list"}
	}
	var tools []ListedTool
	if err := json.Unmarshal(*r.Tools, &tools); err != nil {
		return nil, "", &ListFailure{Kind: ListBadAnswer, Stage: "tools/list", Detail: "tools is not a list of tools"}
	}
	return tools, r.NextCursor, nil
}

// collectPages drives the tools/list loop over one transport's request function. ask sends one JSON-RPC request and
// returns its answer body, or a classified failure.
func collectPages(ask func(id int, method string, params any) ([]byte, *ListFailure)) ([]ListedTool, *ListFailure) {
	var all []ListedTool
	cursor := ""
	seen := map[string]bool{}
	total := 0
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, &ListFailure{Kind: ListBadAnswer, Stage: "tools/list",
				Detail: fmt.Sprintf("the list did not end within %d pages", maxListPages)}
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, f := ask(2+page, "tools/list", params)
		if f != nil {
			return nil, f
		}
		if total += len(raw); total > maxListBytes {
			return nil, &ListFailure{Kind: ListBadAnswer, Stage: "tools/list",
				Detail: fmt.Sprintf("the list is larger than %d bytes", maxListBytes)}
		}
		result, f := decodeAnswer(raw, "tools/list")
		if f != nil {
			return nil, f
		}
		tools, next, f := pageOf(result)
		if f != nil {
			return nil, f
		}
		if all = append(all, tools...); len(all) > maxListTools {
			return nil, &ListFailure{Kind: ListBadAnswer, Stage: "tools/list",
				Detail: fmt.Sprintf("the list has more than %d tools", maxListTools)}
		}
		if next == "" {
			return all, nil
		}
		if seen[next] {
			return nil, &ListFailure{Kind: ListBadAnswer, Stage: "tools/list",
				Detail: "the server handed out the same cursor twice; the list would never end"}
		}
		seen[next] = true
		cursor = next
	}
}

func (c *Client) postCtx(ctx context.Context, url, sessionID string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	c.applyExtra(req.Header)
	return c.http().Do(req)
}

// --- streamable HTTP ---

func (c *Client) listStreamable(ctx context.Context) ListResult {
	sid := ""
	ask := func(id int, method string, params any) ([]byte, *ListFailure) {
		resp, err := c.postCtx(ctx, c.ServerURL, sid, rpc(id, method, params))
		if err != nil {
			return nil, transportFailure(ctx, method, err)
		}
		defer resp.Body.Close()
		if f := statusFailure(resp, method); f != nil {
			return nil, f
		}
		if method == "initialize" {
			sid = resp.Header.Get("Mcp-Session-Id")
		}
		boundedBody(resp)
		raw, err := readJSONRPC(resp)
		if err != nil {
			if ctx.Err() != nil {
				return nil, transportFailure(ctx, method, err)
			}
			return nil, &ListFailure{Kind: ListBadAnswer, Stage: method, Detail: "unreadable answer: " + err.Error()}
		}
		return raw, nil
	}

	raw, f := ask(1, "initialize", initParams())
	if f != nil {
		return ListResult{Failure: f}
	}
	initResult, f := decodeAnswer(raw, "initialize")
	if f != nil {
		return ListResult{Failure: f}
	}
	// Best effort, exactly as Call: a server that needs the notification refuses tools/list without it, and that
	// refusal is then the answer this read reports.
	if ni, err := c.postCtx(ctx, c.ServerURL, sid, rpcNotif("notifications/initialized")); err == nil {
		ni.Body.Close()
	}
	tools, f := collectPages(ask)
	if f != nil {
		return ListResult{Failure: f}
	}
	return ListResult{Tools: tools, Server: decodeServerInfo(initResult)}
}

// --- legacy HTTP+SSE ---

func (c *Client) listLegacySSE(ctx context.Context) ListResult {
	base := strings.TrimSuffix(c.ServerURL, "/")
	sseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sse", nil)
	if err != nil {
		return ListResult{Failure: &ListFailure{Kind: ListBadAnswer, Stage: "GET /sse", Detail: "bad server URL"}}
	}
	if c.Token != "" {
		sseReq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	c.applyExtra(sseReq.Header)
	resp, err := c.http().Do(sseReq)
	if err != nil {
		return ListResult{Failure: transportFailure(ctx, "GET /sse", err)}
	}
	defer resp.Body.Close()
	if f := statusFailure(resp, "GET /sse"); f != nil {
		return ListResult{Failure: f}
	}
	boundedBody(resp)
	br := bufio.NewReader(resp.Body)
	ev, data, err := readSSE(br)
	if err != nil || ev != "endpoint" || data == "" {
		if err != nil && ctx.Err() != nil {
			return ListResult{Failure: transportFailure(ctx, "GET /sse", err)}
		}
		return ListResult{Failure: &ListFailure{Kind: ListBadAnswer, Stage: "GET /sse",
			Detail: "no endpoint event on GET /sse (the server is not legacy HTTP+SSE)"}}
	}
	msgURL, err := resolveEndpoint(sseReq.URL, data)
	if err != nil {
		return ListResult{Failure: &ListFailure{Kind: ListBadAnswer, Stage: "GET /sse", Detail: "bad endpoint event URL"}}
	}

	// The POST's own status is checked here, unlike Call: Argus's server answers a refused token 401 INLINE on the POST
	// (transport.go handleMessage) and never on the stream, so a reader that only watched the stream would wait for an
	// answer that is not coming. 200 with a body is an inline answer too (no stream bound to the session).
	ask := func(id int, method string, params any) ([]byte, *ListFailure) {
		pr, err := c.postCtx(ctx, msgURL, "", rpc(id, method, params))
		if err != nil {
			return nil, transportFailure(ctx, method, err)
		}
		defer pr.Body.Close()
		if f := statusFailure(pr, method); f != nil {
			return nil, f
		}
		if pr.StatusCode == http.StatusOK {
			inline, rerr := io.ReadAll(io.LimitReader(pr.Body, 4<<20))
			if rerr == nil && len(bytes.TrimSpace(inline)) > 0 {
				return inline, nil
			}
		}
		d, err := readSSEResponse(br)
		if err != nil {
			if ctx.Err() != nil {
				return nil, transportFailure(ctx, method, err)
			}
			return nil, &ListFailure{Kind: ListBadAnswer, Stage: method, Detail: "unreadable answer: " + err.Error()}
		}
		return []byte(d), nil
	}

	raw, f := ask(1, "initialize", initParams())
	if f != nil {
		return ListResult{Failure: f}
	}
	initResult, f := decodeAnswer(raw, "initialize")
	if f != nil {
		return ListResult{Failure: f}
	}
	if ni, err := c.postCtx(ctx, msgURL, "", rpcNotif("notifications/initialized")); err == nil {
		ni.Body.Close()
	}
	tools, f := collectPages(ask)
	if f != nil {
		return ListResult{Failure: f}
	}
	return ListResult{Tools: tools, Server: decodeServerInfo(initResult)}
}

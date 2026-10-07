package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// isTimeoutErr reports whether err is the client's OWN deadline elapsing (net.Error.Timeout()) —
// http.Client.Timeout aborts the whole round trip (dial, write, read) the same way, so this is one
// check for all three (VR12-TO-RUN). It is deliberately not "was this a connection refusal": Go
// reports both through the same net.Error, and telling them apart from the caller's side is no
// more possible here than it is for a hung TCP handshake on the JMeter path (argus.local_runner.go
// carries the identical caveat for exec.CommandContext's ctx.Err()).
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Transport is the SUT-DECLARED MCP transport. The client speaks the matching
// handshake — it never guesses (VR-J5). stdio is out of scope.
type Transport string

const (
	Streamable Transport = "streamable-http" // single POST /mcp; Mcp-Session-Id response header
	LegacySSE  Transport = "http-sse"        // 2-endpoint: GET /sse + POST message; correlated by session id
)

// ClientProtocolVersion is what the client OFFERS at initialize. The client is
// version-TOLERANT: it accepts whatever protocolVersion the server declares back
// (2024-11-05 or 2025-03-26) and does not hard-gate on the string (VR-J6).
const ClientProtocolVersion = "2025-03-26"

// Client is the both-transport MCP test client.
type Client struct {
	ServerURL string    // streamable: the POST /mcp URL; legacy-sse: the base (the client GETs base+"/sse")
	Transport Transport // declared, never guessed
	Token     string    // bearer (from ${VAR}); "" = none
	// Timeout is the per-call deadline (GAP-3) — the SUT's declared targets.mcp.timeout_seconds.
	// Zero keeps the 30s default. An explicit HTTP client still wins over both.
	Timeout time.Duration
	HTTP    *http.Client
	// RateLimit (VR10-R1) is the SUT's DECLARED throttle signature, carried here because it is a
	// property of THIS endpoint — the same reason Token and Timeout are. Callers pass it to
	// JudgeWithRateLimit; nil (the default) leaves every verdict exactly as it was.
	RateLimit *RateLimitSpec
	// ExtraHeaders (VR12-T3 / V29-018) are the scenario author's own TRIGGER headers, already
	// resolved. They are applied to every request this client makes AFTER the protocol's own, so an
	// author header REPLACES the runner's for the same canonical name — which is the point: the
	// parser has always populated `Trigger.Headers` for every `Name: value` line, and everything
	// but `Authorization` was dropped between the parser and the wire. SYN-MCP-002 and SYN-MCP-004
	// each declare `Accept: application/json, text/event-stream` and neither has ever sent it.
	//
	// ⛔ TWO NAMES ARE NEVER HERE, and they are REFUSED at validate time rather than dropped:
	// `X-Correlation-Id` (the runner's alone — the report, the logs, the dashboard and the SUT's own
	// records are stitched together by it) and `Mcp-Session-Id` (the protocol's, minted by the
	// handshake). Silently ignoring an author's header is the defect this row is about; refusing it
	// by name is the fix.
	//
	// ⛔ NEVER LOGGED, NEVER IN report.json. A header value is the likeliest place in a scenario to
	// hold a credential — log header NAMES only.
	ExtraHeaders map[string]string
}

// applyExtra sets the author's headers LAST, so they win over the protocol's defaults.
func (c *Client) applyExtra(h http.Header) {
	for k, v := range c.ExtraHeaders {
		h.Set(k, v)
	}
}

// CallInput is one tools/call.
type CallInput struct {
	Tool      string
	Args      any    // becomes `arguments`
	RequestID string // injected as _meta.request_id: one per call (PerCallRequestID), prefixed by the correlation id
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// GAP-3 (2026-07-22): the deadline is the SUT's declared targets.mcp.timeout_seconds when the
	// caller threads it, else 30s. It used to be an unconditional hardcoded 30s, which scored a
	// slow-but-HEALTHY SUT red: Social's PASSING calls measured 16.1s and 19.1s, and four of its
	// eight failures were transport timeouts at exactly 30.00s on calls that passed elsewhere.
	if c.Timeout > 0 {
		return &http.Client{Timeout: c.Timeout}
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Call performs the handshake (per declared transport) + tools/call and returns the
// parsed envelope. Reachability / transport failures are surfaced distinctly on the
// CallResult so the judge can classify them (unreachable / transport ≠ tool-failed).
func (c *Client) Call(in CallInput) CallResult {
	switch c.Transport {
	case LegacySSE:
		return c.callLegacySSE(in)
	case Streamable:
		return c.callStreamable(in)
	default:
		return CallResult{TransportErr: "unknown/undeclared transport " + string(c.Transport)}
	}
}

// --- streamable HTTP (single POST /mcp; session via the Mcp-Session-Id header) ---

func (c *Client) callStreamable(in CallInput) CallResult {
	resp, err := c.postMCP(c.ServerURL, "", rpc(1, "initialize", initParams()))
	if err != nil {
		return CallResult{Unreachable: true, TimedOut: isTimeoutErr(err)} // first contact failed => unreachable (gate-infra)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	// VR2-12 (V15-006): a non-2xx is reported AS a non-2xx, before anything tries to parse it.
	//
	// An HTTP 404 is not a transport failure — `postMCP` returns nil error and the body reads fine —
	// so it used to flow all the way down to json.Unmarshal, fail there, and surface as
	// "unparseable JSON-RPC response". That sentence is true about the BODY'S SHAPE and silent about
	// the CAUSE, and it sent a live diagnosis looking for a protocol bug when the real answer was
	// "you asked for a URL this server does not serve" (measured 2026-08-12: POST <tunnel>/sse ->
	// 404 text/plain, while POST <tunnel>/mcp -> 200 JSON-RPC).
	//
	// The LEGACY-SSE branch of this same file already got this right (see the GET /sse status check
	// below), so this is the streamable path being brought up to a shape the file already contains.
	if st := httpStatusErr(resp, "initialize"); st != "" {
		resp.Body.Close()
		return CallResult{TransportErr: st}
	}
	if _, rerr := readJSONRPC(resp); rerr != nil {
		resp.Body.Close()
		return CallResult{TransportErr: "initialize: " + rerr.Error()}
	}
	resp.Body.Close()

	if ni, nerr := c.postMCP(c.ServerURL, sid, rpcNotif("notifications/initialized")); nerr == nil {
		ni.Body.Close()
	}

	callResp, err := c.postMCP(c.ServerURL, sid, rpc(2, "tools/call", callParams(in)))
	if err != nil {
		return CallResult{TransportErr: "tools/call: " + err.Error(), TimedOut: isTimeoutErr(err)}
	}
	defer callResp.Body.Close()
	if st := httpStatusErr(callResp, "tools/call"); st != "" {
		return CallResult{TransportErr: st}
	}
	raw, rerr := readJSONRPC(callResp)
	if rerr != nil {
		return CallResult{TransportErr: "tools/call read: " + rerr.Error()}
	}
	return parseCallResult(raw)
}

// httpStatusErr returns a describing message for a non-2xx response, or "" when the status is fine.
//
// It reports the STATUS FIRST and a short body excerpt second, because the status is the fact that
// identifies the problem and the body is the evidence for it. The excerpt is bounded: an HTML error
// page from a proxy is thousands of bytes of no interest, and the first line is where the reason
// lives ("404 page not found", "401 Unauthorized").
//
// The body is CONSUMED here. That is safe at both call sites — each returns immediately on a non-2xx
// and never reads the body again — and it is what lets the excerpt exist at all.
func httpStatusErr(resp *http.Response, stage string) string {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ""
	}
	excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 100))
	s := strings.TrimSpace(string(excerpt))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return fmt.Sprintf("%s: HTTP %d from the upstream (empty body)", stage, resp.StatusCode)
	}
	return fmt.Sprintf("%s: HTTP %d from the upstream: %s", stage, resp.StatusCode, s)
}

func (c *Client) postMCP(url, sessionID string, body []byte) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	c.applyExtra(req.Header) // VR12-T3: last, so an author header replaces the runner's
	return c.http().Do(req)
}

// --- legacy HTTP+SSE (GET /sse for the stream + POST message; correlated by session id) ---

func (c *Client) callLegacySSE(in CallInput) CallResult {
	base := strings.TrimSuffix(c.ServerURL, "/")
	sseReq, _ := http.NewRequest(http.MethodGet, base+"/sse", nil)
	if c.Token != "" {
		sseReq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	c.applyExtra(sseReq.Header) // VR12-T3
	resp, err := c.http().Do(sseReq)
	if err != nil {
		return CallResult{Unreachable: true, TimedOut: isTimeoutErr(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CallResult{TransportErr: fmt.Sprintf("GET /sse status %d (declared legacy-sse?)", resp.StatusCode)}
	}
	br := bufio.NewReader(resp.Body)
	ev, data, err := readSSE(br)
	if err != nil || ev != "endpoint" || data == "" {
		return CallResult{TransportErr: "no endpoint event on GET /sse (server is not legacy-HTTP+SSE)"}
	}
	msgURL, perr := resolveEndpoint(sseReq.URL, data)
	if perr != nil {
		return CallResult{TransportErr: "bad endpoint event url: " + perr.Error()}
	}

	post := func(body []byte) error {
		req, _ := http.NewRequest(http.MethodPost, msgURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if c.Token != "" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
		c.applyExtra(req.Header) // VR12-T3
		r, err := c.http().Do(req)
		if err != nil {
			return err
		}
		r.Body.Close()
		return nil
	}

	if err := post(rpc(1, "initialize", initParams())); err != nil {
		return CallResult{TransportErr: "initialize POST: " + err.Error(), TimedOut: isTimeoutErr(err)}
	}
	if _, err := readSSEResponse(br); err != nil { // the initialize response (version-tolerant; consumed)
		return CallResult{TransportErr: "initialize response: " + err.Error()}
	}
	_ = post(rpcNotif("notifications/initialized")) // notification: no SSE reply

	if err := post(rpc(2, "tools/call", callParams(in))); err != nil {
		return CallResult{TransportErr: "tools/call POST: " + err.Error(), TimedOut: isTimeoutErr(err)}
	}
	d, err := readSSEResponse(br)
	if err != nil {
		return CallResult{TransportErr: "tools/call response: " + err.Error()}
	}
	return parseCallResult([]byte(d))
}

// --- shared helpers ---

func initParams() map[string]any {
	return map[string]any{
		"protocolVersion": ClientProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "argus-mcp-client", "version": "M2.5"},
	}
}

func callParams(in CallInput) map[string]any {
	p := map[string]any{"name": in.Tool, "arguments": orEmpty(in.Args)}
	if in.RequestID != "" {
		p["_meta"] = map[string]any{"request_id": in.RequestID}
	}
	return p
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// resolveEndpoint resolves the legacy-SSE `endpoint` event (often a root-relative path
// like /mcp/message?sessionId=...) against the SSE request URL per RFC 3986 — NOT
// base+data, which double-prefixes when the base carries a path (.../mcp + /mcp/message).
func resolveEndpoint(sseURL *url.URL, data string) (string, error) {
	ref, err := url.Parse(strings.TrimSpace(data))
	if err != nil {
		return "", err
	}
	return sseURL.ResolveReference(ref).String(), nil
}

func rpc(id int, method string, params any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return b
}

func rpcNotif(method string) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	return b
}

// readJSONRPC reads one JSON-RPC response from a streamable-HTTP POST response,
// branching on Content-Type: application/json (single body) vs text/event-stream
// (parse data: events to the first complete message). Do NOT default-assume SSE.
func readJSONRPC(resp *http.Response) ([]byte, error) {
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, err := readSSEResponse(bufio.NewReader(resp.Body))
		if err != nil {
			return nil, err
		}
		return []byte(data), nil
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func parseCallResult(raw []byte) CallResult {
	cr := CallResult{Raw: append(json.RawMessage(nil), raw...)}
	var resp struct {
		Error  *RPCError `json:"error"`
		Result *struct {
			Content []ContentBlock `json:"content"`
			IsError bool           `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		cr.TransportErr = "unparseable JSON-RPC response"
		return cr
	}
	cr.JSONRPCError = resp.Error
	if resp.Result != nil {
		cr.IsError = resp.Result.IsError
		cr.Content = resp.Result.Content
	}
	return cr
}

// maxSSEEventsBeforeResponse bounds how many events are read while looking for the JSON-RPC response.
const maxSSEEventsBeforeResponse = 256

// readSSEResponse reads SSE events until one is a JSON-RPC RESPONSE — a message with an
// `id` and no `method`. The server may send notifications (notifications/message: no `id`) BEFORE the result
// on the same stream; the first event is not necessarily the answer. A data event that is not JSON is
// returned as it is (the caller reports it as unparseable), as before.
func readSSEResponse(br *bufio.Reader) (string, error) {
	skipped := 0
	for skipped < maxSSEEventsBeforeResponse {
		_, data, err := readSSE(br)
		if err != nil {
			if skipped > 0 && errors.Is(err, io.EOF) {
				return "", fmt.Errorf("the event stream ended after %d notification(s) without a JSON-RPC response (no event with an id)", skipped)
			}
			return "", err
		}
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &m) != nil {
			return data, nil
		}
		id, hasID := m["id"]
		_, hasMethod := m["method"]
		if hasID && string(id) != "null" && !hasMethod {
			return data, nil
		}
		skipped++
	}
	return "", fmt.Errorf("no JSON-RPC response (an event with an id) within %d events of the stream", maxSSEEventsBeforeResponse)
}

// readSSE reads one complete SSE event (event:/data: until the blank line).
func readSSE(br *bufio.Reader) (event, data string, err error) {
	for {
		line, e := br.ReadString('\n')
		if e != nil {
			return event, data, e
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event != "" || data != "" {
				return event, data, nil
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[len("event:"):])
		}
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(line[len("data:"):])
		}
	}
}

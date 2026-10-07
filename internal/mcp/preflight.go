package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// PreflightResult classifies a handshake-only auth probe: did the SUT ACCEPT the bearer token?
// It exists because validate-config only checks that the ${VAR} is SET — never that the SUT
// accepts its value. Without this, a stale/unseeded token surfaces as EVERY scenario failing
// Unauthorized (e.g. the Social gateway's JSON-RPC -32001) instead of one clear early error.
type PreflightResult struct {
	OK           bool // initialize (+ tools/list) succeeded — the SUT accepted the token
	Unreachable  bool // first contact failed (endpoint down / wrong network) — not an auth verdict
	AuthRejected bool // the SUT rejected the CREDENTIAL (HTTP 401 or JSON-RPC -32001 / auth-keyword)
	Forbidden    bool // HTTP 403 — ambiguous: token scope OR Origin/proxy/policy (the probe sends no
	// Origin header, exactly like the runner). Kept distinct from AuthRejected so the message does not
	// wrongly tell the user to re-seed the token when it may be an Origin/allowlist block.
	HTTPStatus int    // the failing step's HTTP status (0 if no response)
	RPCCode    int    // top-level JSON-RPC error code, if any
	Detail     string // human one-liner for the failure message (NEVER contains the token)
}

// Preflight performs ONLY the MCP handshake — initialize, then the read-only tools/list — and
// classifies whether the bearer token was accepted. It NEVER issues tools/call, so it is safe
// against effectful SUTs. It speaks the DECLARED transport and never guesses (VR-J5), exactly
// like Call.
func (c *Client) Preflight() PreflightResult {
	switch c.Transport {
	case LegacySSE:
		return c.preflightLegacySSE()
	case Streamable:
		return c.preflightStreamable()
	default:
		return PreflightResult{Detail: "unknown/undeclared transport " + string(c.Transport)}
	}
}

func (c *Client) preflightStreamable() PreflightResult {
	// 1) initialize — auth is enforced here on gateways that wrap /mcp in middleware (e.g. Social).
	resp, err := c.postMCP(c.ServerURL, "", rpc(1, "initialize", initParams()))
	if err != nil {
		return PreflightResult{Unreachable: true, Detail: "initialize POST failed: " + err.Error()}
	}
	if pr, done := classifyHTTPAuth(resp.StatusCode); done {
		resp.Body.Close()
		return pr
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	status := resp.StatusCode
	raw, rerr := readJSONRPC(resp)
	resp.Body.Close()
	if rerr != nil {
		return PreflightResult{Detail: "initialize response unreadable: " + rerr.Error(), HTTPStatus: status}
	}
	if pr, done := classifyRPCBody(raw, status, "initialize"); done {
		return pr
	}

	// 2) notifications/initialized (best-effort; some servers require it before tools/list).
	if ni, nerr := c.postMCP(c.ServerURL, sid, rpcNotif("notifications/initialized")); nerr == nil {
		ni.Body.Close()
	}

	// 3) tools/list — a READ-ONLY method (lists tools; never invokes one, so it is safe against
	// effectful SUTs). A token accepted at initialize but rejected here (a SUT that gates only
	// past-handshake) is still caught. This is the true parity with what a scenario needs.
	lr, lerr := c.postMCP(c.ServerURL, sid, rpc(2, "tools/list", map[string]any{}))
	if lerr != nil {
		return PreflightResult{Detail: "tools/list POST failed: " + lerr.Error()}
	}
	if pr, done := classifyHTTPAuth(lr.StatusCode); done {
		lr.Body.Close()
		return pr
	}
	lstatus := lr.StatusCode
	lraw, lrerr := readJSONRPC(lr)
	lr.Body.Close()
	if lrerr != nil {
		return PreflightResult{Detail: "tools/list response unreadable: " + lrerr.Error(), HTTPStatus: lstatus}
	}
	if pr, done := classifyRPCBody(lraw, lstatus, "tools/list"); done {
		return pr
	}
	return PreflightResult{OK: true, HTTPStatus: 200}
}

// preflightLegacySSE checks auth at the GET /sse stream open (the bearer is carried there). A 401/403
// is an auth rejection; a 200 stream open is acceptance. (Legacy SUTs that gate ONLY the message POST
// are not covered here — the streamable path gets the full initialize+tools/list; Social is streamable.)
func (c *Client) preflightLegacySSE() PreflightResult {
	base := strings.TrimSuffix(c.ServerURL, "/")
	req, _ := http.NewRequest(http.MethodGet, base+"/sse", nil)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return PreflightResult{Unreachable: true, Detail: "GET /sse failed: " + err.Error()}
	}
	defer resp.Body.Close()
	if pr, done := classifyHTTPAuth(resp.StatusCode); done {
		return pr
	}
	if resp.StatusCode != http.StatusOK {
		return PreflightResult{HTTPStatus: resp.StatusCode,
			Detail: fmt.Sprintf("GET /sse status %d (declared legacy-sse?)", resp.StatusCode)}
	}
	return PreflightResult{OK: true, HTTPStatus: resp.StatusCode}
}

// classifyHTTPAuth maps an HTTP status to a verdict; done=true when the status alone decides it.
// 401 is an unambiguous credential rejection. 403 is DISTINCT (Forbidden): it may be token scope,
// but the MCP spec also advises servers to 403 on Origin-header validation (DNS-rebinding defense),
// and the probe sends no Origin — so a valid token can 403. Conflating them would wrongly tell the
// user to re-seed the token. Both are still fatal in onboarding (a run would hit the same status),
// but with accurate remediation.
func classifyHTTPAuth(status int) (PreflightResult, bool) {
	switch status {
	case http.StatusUnauthorized:
		return PreflightResult{AuthRejected: true, HTTPStatus: status,
			Detail: "SUT returned HTTP 401 Unauthorized"}, true
	case http.StatusForbidden:
		return PreflightResult{Forbidden: true, HTTPStatus: status,
			Detail: "SUT returned HTTP 403 Forbidden"}, true
	}
	return PreflightResult{}, false
}

// classifyRPCBody inspects a JSON-RPC body. done=true means it carried an ERROR that terminates the
// preflight (auth-reject or other protocol failure). A body with a result (no error) returns
// done=false so the caller proceeds to the next handshake step.
func classifyRPCBody(raw []byte, status int, step string) (PreflightResult, bool) {
	var resp struct {
		Error *RPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return PreflightResult{Detail: step + " returned a non-JSON-RPC body", HTTPStatus: status}, true
	}
	if resp.Error != nil {
		if isAuthRejection(resp.Error) {
			return PreflightResult{AuthRejected: true, HTTPStatus: status, RPCCode: resp.Error.Code,
				Detail: fmt.Sprintf("%s: SUT rejected the token (JSON-RPC %d %q)", step, resp.Error.Code, resp.Error.Message)}, true
		}
		return PreflightResult{HTTPStatus: status, RPCCode: resp.Error.Code,
			Detail: fmt.Sprintf("%s returned JSON-RPC error %d %q", step, resp.Error.Code, resp.Error.Message)}, true
	}
	return PreflightResult{}, false
}

// isAuthRejection recognizes an MCP auth-reject: the MCP-spec Unauthorized code -32001, or a message
// that clearly signals an auth failure. A non-auth error (e.g. -32603 internal) is NOT a rejection.
func isAuthRejection(e *RPCError) bool {
	if e.Code == -32001 {
		return true
	}
	m := strings.ToLower(e.Message)
	for _, kw := range []string{"unauthor", "invalid_token", "invalid token", "forbidden", "authentication", "not authenticated", "auth failed"} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}

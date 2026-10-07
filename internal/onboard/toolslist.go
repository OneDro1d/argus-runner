package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SignInChallengeError is what /mcp answered when it wanted a sign-in instead of tools: HTTP 401 (with the
// WWW-Authenticate challenge that starts an OAuth flow in an MCP client). For a BUILDER this is never
// something to follow — the sign-in would issue an author-scope login (the holdout risk) — so callers
// refuse on it rather than treating it as "no tools".
type SignInChallengeError struct {
	Status          int
	WWWAuthenticate string
}

func (e *SignInChallengeError) Error() string {
	return fmt.Sprintf("the control plane answered HTTP %d with a sign-in challenge instead of tools", e.Status)
}

// ToolsList runs the streamable-HTTP handshake (initialize, initialized, tools/list) against /mcp with the
// given bearer and returns the tool names the token can see. Unlike VerifyToolsList it never reads a 401
// as an empty list: a 401/403 (or any response carrying WWW-Authenticate) is a *SignInChallengeError /
// *HTTPStatusError, and a JSON-RPC error is an error. The token goes only into the Authorization header.
func (c *CloudClient) ToolsList(ctx context.Context, token string) ([]string, error) {
	post := func(sid, body string) (int, http.Header, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/mcp", strings.NewReader(body))
		if err != nil {
			return 0, nil, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := c.HC.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, resp.Header, b, nil
	}
	refusal := func(status int, h http.Header, what string) error {
		if status == http.StatusUnauthorized || h.Get("WWW-Authenticate") != "" {
			return &SignInChallengeError{Status: status, WWWAuthenticate: h.Get("WWW-Authenticate")}
		}
		return &HTTPStatusError{Status: status, Msg: fmt.Sprintf("%s: HTTP %d", what, status)}
	}

	status, h, _, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"argus-init","version":"1"}}}`)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, refusal(status, h, "mcp initialize")
	}
	sid := h.Get("Mcp-Session-Id")
	if sid == "" {
		return nil, fmt.Errorf("mcp initialize: no session id")
	}
	_, _, _, _ = post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	status, h, body, err := post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, refusal(status, h, "mcp tools/list")
	}
	var out struct {
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &out) != nil {
		return nil, fmt.Errorf("tools/list: the answer was not JSON-RPC")
	}
	if out.Error != nil {
		return nil, fmt.Errorf("tools/list: JSON-RPC error %d: %s", out.Error.Code, out.Error.Message)
	}
	if out.Result == nil {
		return nil, fmt.Errorf("tools/list: no result")
	}
	names := make([]string, 0, len(out.Result.Tools))
	for _, t := range out.Result.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}

package toolcore

import (
	"encoding/json"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// MCPCall performs one MCP tool call against a SUT (the manual/dogfood `mcp-call`
// path; the same internal/mcp client + two-plane judge the runner-native scenario
// type uses). transport is declared (streamable-http | http-sse). If expectPlane is
// set, the verdict is included; the two planes are the binding pass/fail.
// timeout is the SUT's declared per-call deadline (GAP-3); zero keeps the 30s default.
func MCPCall(serverURL, transport, tool string, args any, requestID, expectPlane string, expectCode int, mcpToken string, timeout time.Duration) (any, error) {
	cl := &mcp.Client{ServerURL: serverURL, Transport: mcp.Transport(transport), Token: mcpToken, Timeout: timeout}
	res := cl.Call(mcp.CallInput{Tool: tool, Args: args, RequestID: requestID})

	out := map[string]any{
		"tool": tool, "server_url": serverURL, "transport": transport,
		"is_error": res.IsError, "unreachable": res.Unreachable,
	}
	if len(res.Raw) > 0 {
		out["envelope"] = json.RawMessage(res.Raw)
	}
	if res.JSONRPCError != nil {
		out["jsonrpc_error"] = res.JSONRPCError
	}
	if res.TransportErr != "" {
		out["transport_error"] = res.TransportErr
	}
	if len(res.Content) > 0 {
		out["content"] = res.Content
	}
	if expectPlane != "" {
		out["verdict"] = mcp.Judge(res, mcp.Expect{ErrorPlane: mcp.ErrPlane(expectPlane), ErrorCode: expectCode})
	}
	return out, nil
}

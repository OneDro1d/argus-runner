package mcpserver

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/auth"
)

const (
	rtok = "runner-token-AAA-1111"
	atok = "author-token-BBB-2222"
)

const holdoutSecret = "HOLDOUT-SECRET-XYZ"

func fakeTools() []Tool {
	return []Tool{
		{Name: "runner__ping", Namespace: NSRunner, Description: "ping", InputSchema: sch(),
			Handler: func(_ json.RawMessage, p Principal) Outcome {
				return Ok(map[string]any{"pong": true, "hat": string(p.Hat)})
			}},
		{Name: "author__secret", Namespace: NSAuthor, Description: "holdout", InputSchema: sch(),
			Handler: func(_ json.RawMessage, _ Principal) Outcome {
				return Ok(map[string]any{"secret": holdoutSecret})
			}},
		{Name: "runner__boom", Namespace: NSRunner, Description: "tool-plane error", InputSchema: sch(),
			Handler: func(_ json.RawMessage, _ Principal) Outcome {
				return ToolErr(map[string]any{"observed": "boom"})
			}},
	}
}

func sch() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"instance_id": map[string]any{"type": "string"}}}
}

func newSrv(t *testing.T, tools ...Tool) *Server {
	t.Helper()
	if len(tools) == 0 {
		tools = fakeTools()
	}
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, tools...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func reqBytes(id int, method string, params any) []byte {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return b
}

func notifBytes(method string, params any) []byte {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return b
}

func callParamsFor(name string) map[string]any {
	return map[string]any{"name": name, "arguments": map[string]any{"instance_id": "local"}}
}

// handshake drives the full bootup ritual for a session+token.
func handshake(t *testing.T, s *Server, sid, token string) {
	t.Helper()
	s.NewSession(sid)
	if _, ok := s.Dispatch(sid, token, reqBytes(1, "initialize", map[string]any{})); !ok {
		t.Fatalf("initialize returned no response")
	}
	s.Dispatch(sid, token, notifBytes("notifications/initialized", nil))
}

func decode(t *testing.T, raw []byte) Response {
	t.Helper()
	var r Response
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode response: %v (%s)", err, raw)
	}
	return r
}

func TestNewServer_RefusesBadAuthConfig(t *testing.T) {
	if _, err := NewServer(auth.Config{RunnerToken: "same", AuthorToken: "same"}, fakeTools()...); err == nil {
		t.Fatal("NewServer accepted colliding tokens; want refuse-to-start (DF-DEC-M25-05)")
	}
}

func TestInitialize_Handshake(t *testing.T) {
	s := newSrv(t)
	s.NewSession("s1")
	raw, _ := s.Dispatch("s1", rtok, reqBytes(1, "initialize", map[string]any{}))
	r := decode(t, raw)
	res, _ := json.Marshal(r.Result)
	if !strings.Contains(string(res), ProtocolVersion) {
		t.Fatalf("initialize missing protocolVersion %q: %s", ProtocolVersion, res)
	}
}

// VR-H8 / UC-65: tools/call before initialize -> top-level protocol error.
func TestToolsCall_BeforeInitialize_ProtocolError(t *testing.T) {
	s := newSrv(t)
	s.NewSession("s1")
	raw, _ := s.Dispatch("s1", rtok, reqBytes(2, "tools/call", callParamsFor("runner__ping")))
	r := decode(t, raw)
	if r.Error == nil || r.Error.Code != CodeNoSession {
		t.Fatalf("want protocol error CodeNoSession before initialize, got %+v", r)
	}
	if r.Result != nil {
		t.Fatal("a protocol error must carry no result")
	}
}

// VR-H8: tools/call on an unknown/expired session -> protocol error.
func TestToolsCall_UnknownSession(t *testing.T) {
	s := newSrv(t)
	raw, _ := s.Dispatch("never-created", rtok, reqBytes(2, "tools/call", callParamsFor("runner__ping")))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeNoSession {
		t.Fatalf("want CodeNoSession on unknown session, got %+v", r)
	}
}

// VR-H10 / DF-DEC-M25-02: tools/list is scope-filtered per token.
func TestToolsList_ScopeFiltered(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	handshake(t, s, "tt", atok)

	rawP, _ := s.Dispatch("p", rtok, reqBytes(3, "tools/list", nil))
	if names := toolNames(t, rawP); contains(names, "author__secret") || !contains(names, "runner__ping") {
		t.Fatalf("product tools/list must show runner__ only, got %v", names)
	}
	rawT, _ := s.Dispatch("tt", atok, reqBytes(3, "tools/list", nil))
	if names := toolNames(t, rawT); !contains(names, "author__secret") || !contains(names, "runner__ping") {
		t.Fatalf("test tools/list must show all, got %v", names)
	}
}

// VR-H4: runner tools are callable by BOTH hats.
func TestToolsCall_RunnerToolBothHats(t *testing.T) {
	s := newSrv(t)
	for _, tok := range []string{rtok, atok} {
		sid := "sess-" + tok
		handshake(t, s, sid, tok)
		raw, _ := s.Dispatch(sid, tok, reqBytes(4, "tools/call", callParamsFor("runner__ping")))
		if r := decode(t, raw); r.Error != nil {
			t.Fatalf("runner__ping denied for token %s: %+v", tok, r.Error)
		}
	}
}

// VR-AUTH9 / VR-I3 + VR-C8: the product hat is denied author tools AND no holdout content leaks.
func TestToolsCall_AuthorToolDeniedToProduct(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	raw, _ := s.Dispatch("p", rtok, reqBytes(5, "tools/call", callParamsFor("author__secret")))
	r := decode(t, raw)
	if r.Error == nil || r.Error.Code != CodeUnauthorized {
		t.Fatalf("product hat must be DENIED author__secret, got %+v", r)
	}
	if bytes.Contains(raw, []byte(holdoutSecret)) {
		t.Fatalf("SECURITY: denied response leaked holdout content: %s", raw)
	}
}

// VR-I1 / VR-I10: no token -> top-level unauthorized error, no side effect.
func TestToolsCall_NoToken_Unauthorized(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	raw, _ := s.Dispatch("p", "", reqBytes(6, "tools/call", callParamsFor("runner__ping")))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeUnauthorized {
		t.Fatalf("want CodeUnauthorized with no token, got %+v", r)
	}
}

// VR-H6: unknown tool -> -32601.
func TestToolsCall_UnknownTool(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	raw, _ := s.Dispatch("p", rtok, reqBytes(7, "tools/call", callParamsFor("runner__nope")))
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeMethodNotFound {
		t.Fatalf("want -32601 for unknown tool, got %+v", r)
	}
}

// VR-H6: malformed tools/call params -> -32602.
func TestToolsCall_BadParams(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	raw, _ := s.Dispatch("p", rtok, reqBytes(8, "tools/call", map[string]any{"arguments": map[string]any{}})) // no name
	if r := decode(t, raw); r.Error == nil || r.Error.Code != CodeInvalidParams {
		t.Fatalf("want -32602 for missing name, got %+v", r)
	}
}

// VR-H6 boundary: a tool-plane failure is a RESULT with isError:true, NOT a top-level error.
func TestToolsCall_ToolErrorPlane(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	raw, _ := s.Dispatch("p", rtok, reqBytes(9, "tools/call", callParamsFor("runner__boom")))
	r := decode(t, raw)
	if r.Error != nil {
		t.Fatalf("tool failure must NOT be a top-level error: %+v", r.Error)
	}
	res, _ := json.Marshal(r.Result)
	if !strings.Contains(string(res), `"isError":true`) {
		t.Fatalf("tool failure must set result.isError:true, got %s", res)
	}
}

// VR-H3: success -> result.content[0].text is the JSON payload, isError:false.
func TestToolsCall_SuccessEnvelope(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	raw, _ := s.Dispatch("p", rtok, reqBytes(10, "tools/call", callParamsFor("runner__ping")))
	r := decode(t, raw)
	var tr ToolResult
	b, _ := json.Marshal(r.Result)
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("result not a ToolResult: %v", err)
	}
	if tr.IsError || len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, `"pong":true`) {
		t.Fatalf("unexpected success envelope: %+v", tr)
	}
}

// DF-06: runner__run is ASYNC — it returns {status:"running", run_id} immediately
// (no MCP-layer timeout for a real run); the caller then polls runner__get_report{run_id}.
func TestRunnerRun_AsyncReturnsRunningHandle(t *testing.T) {
	ran := make(chan struct{}, 1)
	runTool := Tool{Name: "runner__run", Namespace: NSRunner, Async: true, Description: "run", InputSchema: sch(),
		Handler: func(_ json.RawMessage, _ Principal) Outcome { ran <- struct{}{}; return Ok(map[string]any{"ok": true}) }}
	s := newSrv(t, runTool)
	handshake(t, s, "a", rtok)
	raw, _ := s.Dispatch("a", rtok, reqBytes(1, "tools/call", callParamsFor("runner__run")))
	r := decode(t, raw)
	if r.Error != nil {
		t.Fatalf("async run must not error at dispatch: %+v", r.Error)
	}
	res, _ := json.Marshal(r.Result)
	// the async handle returns {status:running, run_id} where run_id is the shortest UTC
	// date-time (YYYYMMDDThhmmss), uniform with the CLI. (res nests JSON, so quotes are escaped;
	// match the datetime pattern + the run_id key without relying on quote literals.)
	if !strings.Contains(string(res), "running") || !strings.Contains(string(res), "run_id") ||
		!regexp.MustCompile(`\d{8}T\d{6}`).MatchString(string(res)) {
		t.Fatalf("runner__run must return {status:running, run_id:<datetime>}: %s", res)
	}
	select {
	case <-ran: // the background handler executed
	case <-time.After(2 * time.Second):
		t.Fatal("background run handler did not execute")
	}
}

// VR-H17 / DF-DEC-M25-01: a second runner__run while one is in flight is REJECTED.
func TestRunStateGuard_RejectsConcurrent(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	runTool := Tool{Name: "runner__run", Namespace: NSRunner, Async: true, Description: "run", InputSchema: sch(),
		Handler: func(_ json.RawMessage, _ Principal) Outcome {
			close(entered)
			<-release
			return Ok(map[string]any{"ran": true})
		}}
	s := newSrv(t, runTool)
	handshake(t, s, "a", rtok)
	handshake(t, s, "b", rtok)

	go s.Dispatch("a", rtok, reqBytes(1, "tools/call", callParamsFor("runner__run")))
	<-entered // first run holds the run lock
	raw, _ := s.Dispatch("b", rtok, reqBytes(2, "tools/call", callParamsFor("runner__run")))
	r := decode(t, raw)
	if r.Error == nil || r.Error.Code != CodeRunInProgress {
		t.Fatalf("second concurrent run must be rejected (CodeRunInProgress), got %+v", r)
	}
	close(release)
}

// VR-H11: tool calls emit argus_mcp_tool_calls_total on /metrics.
func TestMetrics_Increment(t *testing.T) {
	s := newSrv(t)
	handshake(t, s, "p", rtok)
	s.Dispatch("p", rtok, reqBytes(11, "tools/call", callParamsFor("runner__ping")))
	m := s.Metrics()
	if !strings.Contains(m, "argus_mcp_tool_calls_total") || !strings.Contains(m, `tool="runner__ping"`) {
		t.Fatalf("metrics missing the tool-call counter:\n%s", m)
	}
}

func TestNotification_NoResponse(t *testing.T) {
	s := newSrv(t)
	s.NewSession("p")
	s.Dispatch("p", rtok, reqBytes(1, "initialize", map[string]any{}))
	if _, ok := s.Dispatch("p", rtok, notifBytes("notifications/initialized", nil)); ok {
		t.Fatal("notifications/initialized must not produce a response")
	}
	if !s.sessionReady("p") {
		t.Fatal("session must be ready after notifications/initialized")
	}
}

// --- helpers ---

func toolNames(t *testing.T, raw []byte) []string {
	t.Helper()
	r := decode(t, raw)
	b, _ := json.Marshal(r.Result)
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(b, &res)
	var names []string
	for _, x := range res.Tools {
		names = append(names, x.Name)
	}
	return names
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

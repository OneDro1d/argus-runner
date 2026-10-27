package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// AC-D69 (design #678 B4 part 1, B7 part 1): the router passes the control plane's author tools on unchanged.
//
// The control plane here is a REAL mcpserver behind its real HTTP handler — the same protocol code the control plane
// runs — with a recorder in front (every request: method, tool, bearer, arguments) and switchable failure modes. The
// router side is the real server router.NewServer builds, driven through Dispatch exactly as an agent's client drives it.
// passthrough_cp_test.go repeats the equality checks against the control plane's own tool set.

const (
	cpTokA = "odts_machine_token_AAAA"
	cpTokB = "odts_machine_token_BBBB" // the token after a rotation
)

type stubCall struct {
	Method, Tool, Bearer, Host string
	Args                       json.RawMessage
}

type stubCP struct {
	t        *testing.T
	ts       *httptest.Server
	mu       sync.Mutex
	srv      *mcpserver.Server
	h        http.Handler
	accepted map[string]bool
	mode     string // "" | "hang" | "status:<n>" | "couldnotverify" | "rpcerr" | "badanswer" | "rawlist" | "slowlist"
	rawList  string // the tools/list result for mode "rawlist"
	calls    []stubCall
}

// stubAuth accepts the tokens in the stub's accepted set, as the test hat; anything else is refused (HTTP 401).
type stubAuth struct{ c *stubCP }

func (a stubAuth) Authenticate(tok string) (mcpserver.Principal, error) {
	a.c.mu.Lock()
	defer a.c.mu.Unlock()
	if a.c.accepted[tok] {
		return mcpserver.Principal{Hat: role.Test}, nil
	}
	return mcpserver.Principal{}, fmt.Errorf("no valid credential")
}

func newStubCP(t *testing.T, tools ...mcpserver.Tool) *stubCP {
	t.Helper()
	c := &stubCP{t: t, accepted: map[string]bool{cpTokA: true}}
	c.setTools(tools...)
	c.ts = httptest.NewServer(c)
	t.Cleanup(c.ts.Close)
	return c
}

func (c *stubCP) setTools(tools ...mcpserver.Tool) {
	srv, err := mcpserver.NewServerWithAuth(stubAuth{c}, tools...)
	if err != nil {
		c.t.Fatal(err)
	}
	c.mu.Lock()
	c.srv, c.h = srv, mcpserver.NewHTTPHandler(srv)
	c.mu.Unlock()
}

func (c *stubCP) setMode(mode string) {
	c.mu.Lock()
	c.mode = mode
	c.mu.Unlock()
}

func (c *stubCP) URL() string { return c.ts.URL + "/mcp" }

func (c *stubCP) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, x := range c.calls {
		if x.Method == method {
			n++
		}
	}
	return n
}

func (c *stubCP) lastCall() stubCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.calls) - 1; i >= 0; i-- {
		if c.calls[i].Method == "tools/call" {
			return c.calls[i]
		}
	}
	return stubCall{}
}

func (c *stubCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	c.mu.Lock()
	c.calls = append(c.calls, stubCall{Method: req.Method, Tool: req.Params.Name, Bearer: r.Header.Get("Authorization"),
		Host: r.Host, Args: req.Params.Arguments})
	mode, raw, h := c.mode, c.rawList, c.h
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case mode == "hang":
		<-r.Context().Done()
		return
	case strings.HasPrefix(mode, "status:"):
		var code int
		fmt.Sscanf(strings.TrimPrefix(mode, "status:"), "%d", &code)
		w.WriteHeader(code)
		io.WriteString(w, "<html>upstream proxy page</html>")
		return
	case mode == "couldnotverify":
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"unauthorized: could not verify the credential (the control plane could not reach its token store): db down","data":{"instance_id":"local"}}}`)
		return
	case mode == "rpcerr" && req.Method == "tools/list":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"internal"}}`, req.ID)
		return
	case mode == "badanswer" && req.Method == "tools/list":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, req.ID)
		return
	case mode == "rawlist" && req.Method == "tools/list":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, raw)
		return
	case mode == "slowlist" && req.Method == "tools/list":
		time.Sleep(200 * time.Millisecond)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.ServeHTTP(w, r)
}

// direct drives the stub's own tool server, as an agent connected STRAIGHT to the control plane would: the reference.
func (c *stubCP) direct(t *testing.T, sid string, id int, method string, params any) []byte {
	t.Helper()
	c.mu.Lock()
	srv := c.srv
	c.mu.Unlock()
	if method != "tools/list" && method != "tools/call" {
		t.Fatalf("direct: %s", method)
	}
	srv.AdoptSession(sid)
	raw, _ := srv.Dispatch(sid, cpTokA, rpcReq(id, method, params))
	return raw
}

func okTool(payload any) mcpserver.Handler {
	return func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome { return mcpserver.Ok(payload) }
}

// cpTools is a realistic author list: closed schemas, a nested array of closed objects with numeric bounds, long texts
// with characters the encoder escapes, a tool that answers -32602 for a missing instance_id (as the control plane's
// resolveWorkspace does), a tool error, and a JSON-RPC error that carries data.
func cpTools() []mcpserver.Tool {
	closed := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	return []mcpserver.Tool{
		{Name: "author_write_scenario", Namespace: mcpserver.NSAuthor, Aliases: []string{"author__write_scenario"},
			Description: "Validate, then write a scenario <file> into the catalog — refused when invalid & never partial.",
			InputSchema: closed([]string{"instance_id", "content"}, map[string]any{"instance_id": str("the instance (required)"), "content": str("markdown")}),
			Handler: func(args json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
				return mcpserver.Ok(map[string]any{"written": true, "echo": json.RawMessage(args)})
			}},
		{Name: "author_list_runs", Namespace: mcpserver.NSAuthor, Aliases: []string{"author__list_runs"},
			Description: "WHICH RUNS EXIST for an instance, newest first.",
			InputSchema: closed([]string{"instance_id"}, map[string]any{"instance_id": str("required"), "limit": str("optional"), "since": str("optional")}),
			Handler: func(args json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
				var a struct {
					InstanceID string `json:"instance_id"`
				}
				_ = json.Unmarshal(args, &a)
				if a.InstanceID == "" {
					return mcpserver.RPC(mcpserver.CodeInvalidParams, "instance_id is required")
				}
				return mcpserver.Ok(map[string]any{"runs": []any{}})
			}},
		{Name: "author_create_comparison", Namespace: mcpserver.NSAuthor, Aliases: []string{"author__create_comparison"},
			Description: "Create a comparison of several instances.",
			InputSchema: closed([]string{"instance_id", "members"}, map[string]any{"instance_id": str("required"),
				"members": map[string]any{"type": "array", "minItems": 2, "maxItems": 12, "items": closed([]string{"name", "instance_id", "role"},
					map[string]any{"name": str("n"), "instance_id": str("i"), "role": str("reference|candidate")})}}),
			Handler: okTool(map[string]any{"comparison_id": "cmp_1"})},
		{Name: "author_get_report", Namespace: mcpserver.NSAuthor, Aliases: []string{"author__get_report"},
			Description: "Read a report.", InputSchema: closed([]string{"instance_id"}, map[string]any{"instance_id": str("required")}),
			Handler: func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome {
				return mcpserver.ToolErr(map[string]any{"error": "no report yet", "status": "running"})
			}},
		{Name: "author_get_reveal", Namespace: mcpserver.NSAuthor, Aliases: []string{"author__get_reveal"},
			Description: "Reveal.", InputSchema: closed([]string{"instance_id"}, map[string]any{"instance_id": str("required")}),
			Handler: func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome {
				return mcpserver.Outcome{RPCErr: &mcpserver.RPCError{Code: mcpserver.CodeUnauthorized, Message: "not yet", Data: map[string]any{"instance_id": "suta", "reason": "sealed"}}}
			}},
	}
}

// harness is the router under test: three folders, the stub control plane, and one stub EXECUTOR per instance
// (stubexec_test.go) — since AC-D70 every session start reads each instance's executor, so no folder here names an
// address nothing answers on, or one a real executor on the build machine might answer on.
type harness struct {
	t    *testing.T
	cp   *stubCP
	ex   map[string]*stubExec // by instance id: suta, sutb (product + test folders), sutz (the other folder)
	tbl  *Table
	srv  *mcpserver.Server
	mu   sync.Mutex
	logs []string
}

type folderOpt func(*Folder)

// harnessExecVersion is the version every harness executor reports.
const harnessExecVersion = "0.3.73"

func newHarness(t *testing.T, opts Options, testFolder ...folderOpt) *harness {
	t.Helper()
	h := &harness{t: t, cp: newStubCP(t, cpTools()...), ex: map[string]*stubExec{}}
	for _, id := range []string{"suta", "sutb", "sutz"} {
		h.ex[id] = newStubExec(t, harnessExecVersion, "runner-"+id, "author-"+id)
	}
	h.tbl = h.table(testFolder...)
	opts.Logf = func(format string, a ...any) {
		h.mu.Lock()
		h.logs = append(h.logs, fmt.Sprintf(format, a...))
		h.mu.Unlock()
	}
	if opts.ListTimeout == 0 {
		opts.ListTimeout = 3 * time.Second
	}
	srv, err := NewServer(h.tbl, opts)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	return h
}

// table builds prodTok (product), testTok (test, the stub control plane) and otherTok (test, another SUT).
func (h *harness) table(testFolder ...folderOpt) *Table {
	tbl := New()
	must := func(f Folder) {
		if err := tbl.AddFolder(f); err != nil {
			h.t.Fatal(err)
		}
	}
	up := func(id, tok string) Upstream { return Upstream{URL: h.ex[id].URL(), Token: tok} }
	must(Folder{Path: "C:/work/product-agent", Hat: role.Product, Token: prodTok,
		Upstreams: map[string]Upstream{"suta": up("suta", "runner-suta"), "sutb": up("sutb", "runner-sutb")}})
	tf := Folder{Path: "C:/work/test-agent", Hat: role.Test, Token: testTok,
		Upstreams: map[string]Upstream{"suta": up("suta", "author-suta"), "sutb": up("sutb", "author-sutb")},
		Cloud:     &CloudRef{URL: h.cp.URL(), User: "u"}, record: &CloudRecord{URL: h.cp.URL(), User: "u", Token: cpTokA}}
	for _, o := range testFolder {
		o(&tf)
	}
	must(tf)
	must(Folder{Path: "C:/elsewhere/test-agent", Hat: role.Test, Token: otherTok,
		Upstreams: map[string]Upstream{"sutz": up("sutz", "author-sutz")},
		Cloud:     &CloudRef{URL: h.cp.URL(), User: "u"}, record: &CloudRecord{URL: h.cp.URL(), User: "u", Token: cpTokA}})
	return tbl
}

// execToolCalls counts the tools/call requests every harness executor received.
func (h *harness) execToolCalls() int {
	n := 0
	for _, e := range h.ex {
		n += e.count("tools/call")
	}
	return n
}

func rpcReq(id int, method string, params any) []byte {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != 0 {
		m["id"] = id
	}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	return b
}

func (h *harness) initialize(sid, tok string) map[string]any {
	h.t.Helper()
	raw, _ := h.srv.Dispatch(sid, tok, rpcReq(1, "initialize", map[string]any{}))
	var r struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Result == nil {
		h.t.Fatalf("initialize: %s", raw)
	}
	h.srv.Dispatch(sid, tok, rpcReq(0, "notifications/initialized", nil))
	return r.Result
}

// listed returns the router's tools/list entries, raw, by name — and the names in order.
func (h *harness) listed(sid, tok string) (map[string]json.RawMessage, []string) {
	h.t.Helper()
	raw, _ := h.srv.Dispatch(sid, tok, rpcReq(2, "tools/list", map[string]any{}))
	return parseList(h.t, raw)
}

func parseList(t *testing.T, raw []byte) (map[string]json.RawMessage, []string) {
	t.Helper()
	var r struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
		Error *mcpserver.RPCError `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Error != nil {
		t.Fatalf("tools/list: %s", raw)
	}
	byName := map[string]json.RawMessage{}
	var names []string
	for _, tl := range r.Result.Tools {
		var n struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(tl, &n)
		byName[n.Name] = tl
		names = append(names, n.Name)
	}
	return byName, names
}

func (h *harness) call(sid, tok string, id int, name string, args any) []byte {
	h.t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	raw, _ := h.srv.Dispatch(sid, tok, rpcReq(id, "tools/call", params))
	return raw
}

func authorNames(names []string) []string {
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, "author_") {
			out = append(out, n)
		}
	}
	return out
}

// runnerNames is what every harness session lists first: the runner tools each instance's executor lists — the
// executor's real tools since AC-D70, passed on unchanged.
const runnerNames = "runner_validate_config,runner_run,runner_get_report,runner_get_sagas,runner_get_tail_logs,runner_get_dashboard_url"

// ── B7 1 / A9 #1: the router's author list IS the control plane's ─────────────────────────────────────────────────

func TestPassThrough_TheAuthorListEqualsTheControlPlanesOwn(t *testing.T) {
	h := newHarness(t, Options{})
	if res := h.initialize("s", testTok); res["instructions"] != nil {
		t.Fatalf("a healthy session start carries instructions: %v", res["instructions"])
	}
	routed, names := h.listed("s", testTok)
	direct, directNames := parseList(t, h.cp.direct(t, "d", 2, "tools/list", map[string]any{}))
	if len(directNames) != len(cpTools()) {
		t.Fatalf("precondition: the control plane lists %d tools, want %d — two empty lists would compare equal", len(directNames), len(cpTools()))
	}
	if got, want := strings.Join(authorNames(names), ","), strings.Join(directNames, ","); got != want {
		t.Fatalf("author tools through the router %q, straight from the control plane %q", got, want)
	}
	for name, d := range direct {
		if !bytes.Equal(routed[name], d) {
			t.Errorf("%s differs (name, description or schema)\nrouter: %s\ndirect: %s", name, routed[name], d)
		}
	}
	// …and the runner tools the executors list come before them.
	if got := strings.Join(names[:strings.Count(runnerNames, ",")+1], ","); got != runnerNames {
		t.Fatalf("the executors' runner tools are not listed first: %q", got)
	}
	if strings.Contains(strings.Join(names, ","), "author__") {
		t.Fatalf("an old double-underscore name is listed: %v", names)
	}
}

// ── B7 3 / A9 #8: the product folder ───────────────────────────────────────────────────────────────────────────────

func TestPassThrough_AProductFolderGetsNoAuthorToolAndNoControlPlaneCall(t *testing.T) {
	h := newHarness(t, Options{})
	res := h.initialize("p", prodTok)
	if _, has := res["instructions"]; has {
		t.Fatalf("a product folder's session start mentions something: %v", res["instructions"])
	}
	_, names := h.listed("p", prodTok)
	if got := strings.Join(names, ","); got != runnerNames {
		t.Fatalf("the product folder lists %q, want exactly the 6 runner tools", got)
	}
	for _, name := range []string{"author_write_scenario", "author__write_scenario", "author_anything_new"} {
		raw := h.call("p", prodTok, 3, name, map[string]any{"instance_id": "suta"})
		want := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"error":{"code":-32001,"message":"not permitted for the product scope: %s","data":{"instance_id":"suta"}}}`, name)
		if string(raw) != want {
			t.Fatalf("product call %s:\n got %s\nwant %s (today's answer, unchanged)", name, raw, want)
		}
	}
	if n := len(h.cp.calls); n != 0 {
		t.Fatalf("the product folder caused %d requests to the control plane — it must cause none", n)
	}
	// Positive control: the same router, the test folder, does see author tools.
	h.initialize("t", testTok)
	if _, names := h.listed("t", testTok); len(authorNames(names)) == 0 {
		t.Fatal("positive control failed: the test folder sees no author tool either, so the product check proves nothing")
	}
}

// ── B7 4 (author half): a product token presenting a test session's id ────────────────────────────────────────────

func TestPassThrough_AProductTokenOnATestSessionIDSeesNoAuthorTool(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("shared", testTok)
	before := len(h.cp.calls)
	_, names := h.listed("shared", prodTok)
	if got := strings.Join(names, ","); got != runnerNames {
		t.Fatalf("a product token on a test session's id lists %q", got)
	}
	for _, name := range []string{"author_write_scenario", "author__write_scenario"} {
		raw := h.call("shared", prodTok, 4, name, map[string]any{"instance_id": "suta", "content": "x"})
		if !strings.Contains(string(raw), `"code":-32001`) {
			t.Fatalf("product token calling %s on a test session id: %s", name, raw)
		}
	}
	if len(h.cp.calls) != before {
		t.Fatalf("the product token caused %d control-plane requests", len(h.cp.calls)-before)
	}
	// Its runner call is forwarded with the PRODUCT folder's executor token (VR-R4), so the executor redacts.
	raw := h.call("shared", prodTok, 5, "runner__get_report", map[string]any{"instance_id": "suta"})
	if strings.Contains(string(raw), `"isError":true`) || h.ex["suta"].lastCall().Bearer != "Bearer runner-suta" {
		t.Fatalf("runner call: %s, forwarded with %q (want the product executor token)", raw, h.ex["suta"].lastCall().Bearer)
	}
	if strings.Contains(string(raw), sentinelExpected) {
		t.Fatalf("the product token's report carries the expected value: %s", raw)
	}
}

// ── B7 5: the folder rule and the one-instance rule on the author path ───────────────────────────────────────────

func TestPassThrough_TheFolderRuleRefusesBeforeAnyForward(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	cases := map[string]string{
		"another folder's instance (VR-R5)": `{"instance_id":"sutz"}`,
		"a joined list (VR-R13)":            `{"instance_id":"suta,sutb"}`,
		"a number":                          `{"instance_id":5}`,
		"null":                              `{"instance_id":null}`,
		"an empty string":                   `{"instance_id":""}`,
		"a key that folds to instance_id":   `{"INSTANCE_ID":"sutz"}`,
		"a folded key even for an in-folder instance": `{"Instance_Id":"suta"}`,
		"a long-s key Go's decoder folds":             `{"inſtance_id":"sutz"}`,
		"instance_id twice (last would win upstream)": `{"instance_id":"suta","instance_id":"sutz"}`,
		"instance_id twice with the same value":       `{"instance_id":"suta","instance_id":"suta"}`,
	}
	for name, args := range cases {
		raw := h.call("s", testTok, 6, "author_list_runs", json.RawMessage(args))
		if !strings.Contains(string(raw), `"isError":true`) || !strings.Contains(string(raw), "router:") {
			t.Errorf("%s: not refused by the router: %s", name, raw)
		}
	}
	if n := h.cp.count("tools/call"); n != 0 {
		t.Fatalf("%d refused calls reached the control plane", n)
	}
	// In-folder, by the old name too: forwarded.
	for _, name := range []string{"author_list_runs", "author__list_runs"} {
		raw := h.call("s", testTok, 7, name, map[string]any{"instance_id": "suta"})
		if strings.Contains(string(raw), `"error"`) || strings.Contains(string(raw), `"isError":true`) {
			t.Fatalf("%s for an in-folder instance was refused: %s", name, raw)
		}
	}
	if n := h.cp.count("tools/call"); n != 2 {
		t.Fatalf("%d calls reached the control plane, want 2", n)
	}
}

// A9 #7 / A3 "accepted on purpose": the router checks only the instance named in `instance_id`. A tool that also acts on
// other instances — here a comparison whose members include an instance of ANOTHER folder — is not refused by the router;
// the control plane answers as it does for a Full token.
func TestPassThrough_AToolActingOnOtherInstancesIsNotRefusedByTheRouter(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	args := map[string]any{"instance_id": "suta", "members": []any{
		map[string]any{"name": "ref", "instance_id": "suta", "role": "reference"},
		map[string]any{"name": "other", "instance_id": "sutz", "role": "candidate"}, // sutz is onboarded into otherTok's folder
	}}
	raw := h.call("s", testTok, 30, "author_create_comparison", args)
	if strings.Contains(string(raw), "router:") || strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("the router refused a comparison over another folder's member: %s", raw)
	}
	got := h.cp.lastCall()
	if got.Tool != "author_create_comparison" || !strings.Contains(string(got.Args), `"sutz"`) {
		t.Fatalf("the call did not reach the control plane with its members: %+v %s", got, got.Args)
	}
}

// ── B7 14 (author half) / A9 #9: no instance_id passes through, and the answer is the control plane's own ─────────

func TestPassThrough_NoInstanceIDPassesThroughAndTheErrorIsTheControlPlanesOwn(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	routed := h.call("s", testTok, 8, "author_list_runs", map[string]any{})
	direct := h.cp.direct(t, "d", 8, "tools/call", map[string]any{"name": "author_list_runs", "arguments": map[string]any{}})
	if string(routed) != string(direct) {
		t.Fatalf("the same bad call answers differently\nrouter: %s\ndirect: %s", routed, direct)
	}
	if !strings.Contains(string(routed), `"code":-32602`) || strings.Contains(string(routed), `"data"`) {
		t.Fatalf("want the control plane's -32602 with NO data field: %s", routed)
	}
	// Arguments that are not an object pass through unchanged too — the control plane answers.
	if n := h.cp.count("tools/call"); n != 1 {
		t.Fatalf("%d forwards, want 1", n)
	}
}

// ── B7 15: answers back unchanged ─────────────────────────────────────────────────────────────────────────────────

func TestPassThrough_AnswersComeBackUnchanged(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"author_write_scenario", map[string]any{"instance_id": "suta", "content": "<b>&\u2028x</b>"}}, // a result
		{"author_get_report", map[string]any{"instance_id": "suta"}},                                   // a tool error
		{"author_get_reveal", map[string]any{"instance_id": "suta"}},                                   // a JSON-RPC error with data
		{"author_write_scenario", map[string]any{"instance_id": "suta", "content": "x", "junk": 1}},    // refused by the CP's closed schema
	} {
		routed := h.call("s", testTok, 9, c.tool, c.args)
		direct := h.cp.direct(t, "d", 9, "tools/call", map[string]any{"name": c.tool, "arguments": c.args})
		if string(routed) != string(direct) {
			t.Errorf("%s: answers differ\nrouter: %s\ndirect: %s", c.tool, routed, direct)
		}
	}
}

// The arguments reach the control plane as the agent sent them — byte for byte, the real instance_id included.
func TestPassThrough_ArgumentsReachTheControlPlaneUnchanged(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	args := json.RawMessage(`{"content":"line1\nline2","instance_id":"suta","n":1.50,"x":[1,{"y":null}]}`)
	h.call("s", testTok, 10, "author__write_scenario", args)
	got := h.cp.lastCall()
	if got.Tool != "author_write_scenario" {
		t.Fatalf("the old name was forwarded as %q — the router must use the name the control plane listed", got.Tool)
	}
	if string(got.Args) != string(args) {
		t.Fatalf("arguments changed on the way\n sent: %s\n  got: %s", args, got.Args)
	}
	if got.Bearer != "Bearer "+cpTokA {
		t.Fatalf("forwarded with %q, want the machine's author token", got.Bearer)
	}
}

// ── B7 9 / A9 #12: every control-plane cause ─────────────────────────────────────────────────────────────────────

func TestPassThrough_EveryControlPlaneCauseHasItsTextAtStartAndOnACall(t *testing.T) {
	closedPort := func() string {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		a := ln.Addr().String()
		ln.Close()
		return "http://" + a + "/mcp"
	}()
	cases := []struct {
		name   string
		mode   string
		folder folderOpt
		want   string
	}{
		{"not reachable", "", func(f *Folder) { f.record.URL = closedPort; f.Cloud.URL = closedPort }, textCPUnreachable},
		{"timed out", "hang", nil, textCPUnreachable},
		{"401 — token refused", "", func(f *Folder) { f.record.Token = "odts_revoked" }, textCPRefused},
		{"403", "status:403", nil, textCPRefused},
		{"503", "status:503", nil, fmt.Sprintf(textCPErrorFmt, "HTTP 503")},
		{"401 because the token store is down", "couldnotverify", nil, fmt.Sprintf(textCPErrorFmt, "HTTP 401: it could not check the token just now")},
		{"JSON-RPC error", "rpcerr", nil, fmt.Sprintf(textCPErrorFmt, "JSON-RPC error -32603")},
		{"not a tool list", "badanswer", nil, fmt.Sprintf(textCPErrorFmt, "an answer that is not a tool list")},
		{"no record", "", func(f *Folder) { f.Cloud, f.record = nil, nil }, textNoRecord},
		{"record gone", "", func(f *Folder) { f.record = nil }, textRecordGone},
		{"no token", "", func(f *Folder) { f.record.Token = "" }, textNoToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var opts []folderOpt
			if c.folder != nil {
				opts = append(opts, func(f *Folder) {
					r := *f.record // never mutate a record another folder shares
					f.record = &r
					cr := *f.Cloud
					f.Cloud = &cr
					c.folder(f)
				})
			}
			h := newHarness(t, Options{ListTimeout: 300 * time.Millisecond}, opts...)
			h.cp.setMode(c.mode)
			res := h.initialize("s", testTok)
			if res["instructions"] != c.want {
				t.Fatalf("session start text\n got: %v\nwant: %s", res["instructions"], c.want)
			}
			_, names := h.listed("s", testTok)
			if got := strings.Join(names, ","); got != runnerNames {
				t.Fatalf("lists %q, want the 6 runner tools and no author tool", got)
			}
			for _, name := range []string{"author_write_scenario", "author__write_scenario"} {
				raw := h.call("s", testTok, 11, name, map[string]any{"instance_id": "suta"})
				var r struct {
					Result mcpserver.ToolResult `json:"result"`
				}
				_ = json.Unmarshal(raw, &r)
				if !r.Result.IsError || len(r.Result.Content) != 1 || !strings.Contains(r.Result.Content[0].Text, jsonText(c.want)) {
					t.Fatalf("%s: the call did not get the same text: %s", name, raw)
				}
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			for _, l := range h.logs {
				for _, secret := range []string{testTok, prodTok, otherTok, cpTokA, "odts_revoked"} {
					if strings.Contains(l, secret) {
						t.Fatalf("a log line carries a token: %q", l)
					}
				}
			}
		})
	}
}

// jsonText is s as it appears inside a JSON string (the tool error's text is JSON).
func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// A9 #12: down at session start, up again → a NEW session start has the tools back, with no text.
func TestPassThrough_AfterAReconnectWithTheControlPlaneUpTheToolsAreBack(t *testing.T) {
	h := newHarness(t, Options{ListTimeout: 300 * time.Millisecond})
	h.cp.setMode("hang")
	if res := h.initialize("s1", testTok); res["instructions"] != textCPUnreachable {
		t.Fatalf("down: %v", res["instructions"])
	}
	h.cp.setMode("")
	if res := h.initialize("s2", testTok); res["instructions"] != nil {
		t.Fatalf("up again, a session start still says: %v", res["instructions"])
	}
	if _, names := h.listed("s2", testTok); len(authorNames(names)) != len(cpTools()) {
		t.Fatalf("after the reconnect the author tools are not back: %v", names)
	}
}

// A9 #5 / decision 3: a tool the control plane adds appears at the NEXT session start, with no router change — and not
// in a session already open.
func TestPassThrough_ANewControlPlaneToolAppearsAtTheNextSessionStart(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("old", testTok)
	h.cp.setTools(append(cpTools(), mcpserver.Tool{Name: "author_get_report_limited", Namespace: mcpserver.NSAuthor,
		Description: "new", InputSchema: map[string]any{"type": "object", "required": []string{"instance_id"},
			"properties": map[string]any{"instance_id": map[string]any{"type": "string"}}}, Handler: okTool("limited")})...)
	if _, names := h.listed("old", testTok); strings.Contains(strings.Join(names, ","), "author_get_report_limited") {
		t.Fatal("the open session's list changed — decision 3 says it does not need to")
	}
	h.initialize("new", testTok)
	if _, names := h.listed("new", testTok); !strings.Contains(strings.Join(names, ","), "author_get_report_limited") {
		t.Fatalf("the new tool did not appear at the next session start: %v", names)
	}
	if raw := h.call("new", testTok, 12, "author_get_report_limited", map[string]any{"instance_id": "suta"}); !strings.Contains(string(raw), "limited") {
		t.Fatalf("the new tool is listed but not callable: %s", raw)
	}
}

// ── B7 10 (control-plane half): decision 13 ──────────────────────────────────────────────────────────────────────

func TestPassThrough_ARunnerToolFromTheControlPlaneHidesEveryRunnerTool(t *testing.T) {
	for name, rawList := range map[string]string{
		"a runner_ tool":                      `{"tools":[{"name":"author_x","description":"d","inputSchema":{"type":"object"}},{"name":"runner_x","description":"d","inputSchema":{"type":"object"}}]}`,
		"a runner_ tool with a broken schema": `{"tools":[{"name":"author_x","description":"d","inputSchema":{"type":"object"}},{"name":"runner__run","description":"d","inputSchema":"oops"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Options{})
			h.cp.mu.Lock()
			h.cp.mode, h.cp.rawList = "rawlist", rawList
			h.cp.mu.Unlock()
			res := h.initialize("s", testTok)
			if text, _ := res["instructions"].(string); !strings.Contains(text, textRunnerBreach) {
				t.Fatalf("the session start does not say so: %v", res["instructions"])
			}
			_, names := h.listed("s", testTok)
			if got := strings.Join(names, ","); got != "author_x" {
				t.Fatalf("lists %q — want NO runner tool at all (neither plane's), the author tool kept", got)
			}
			for _, call := range []string{"runner__run", "runner_run", "runner__get_report", "runner_x"} {
				raw := h.call("s", testTok, 13, call, map[string]any{"instance_id": "suta"})
				if !strings.Contains(string(raw), jsonText(textRunnerBreach)) {
					t.Fatalf("%s: %s", call, raw)
				}
			}
			if n := h.execToolCalls(); n != 0 {
				t.Fatalf("%d runner calls reached an executor during a decision-13 breach", n)
			}
		})
	}
}

// A listed tool whose schema a strict client would reject is not passed on — it would make the
// client reject the WHOLE list, runner tools included — and the session start names it.
func TestPassThrough_ASchemaAStrictClientRejectsIsNotPassedOn(t *testing.T) {
	h := newHarness(t, Options{})
	h.cp.mu.Lock()
	h.cp.mode, h.cp.rawList = "rawlist", `{"tools":[
		{"name":"author_good","description":"d","inputSchema":{"type":"object","required":["instance_id"],"properties":{"instance_id":{"type":"string"}}}},
		{"name":"author_required_null","description":"d","inputSchema":{"type":"object","required":null}},
		{"name":"author_required_numbers","description":"d","inputSchema":{"type":"object","required":[1]}},
		{"name":"author_type_string","description":"d","inputSchema":{"type":"string"}},
		{"name":"author_properties_list","description":"d","inputSchema":{"type":"object","properties":[]}},
		{"name":"author_no_schema","description":"d"},
		{"name":"","description":"d","inputSchema":{"type":"object"}}]}`
	h.cp.mu.Unlock()
	res := h.initialize("s", testTok)
	_, names := h.listed("s", testTok)
	if got := strings.Join(names, ","); got != runnerNames+",author_good" {
		t.Fatalf("lists %q", got)
	}
	text, _ := res["instructions"].(string)
	for _, n := range []string{"author_required_null", "author_required_numbers", "author_type_string", "author_properties_list", "author_no_schema", "a tool with no name"} {
		if !strings.Contains(text, n) {
			t.Errorf("the session start does not name %s:\n%s", n, text)
		}
	}
	raw, _ := h.srv.Dispatch("s", testTok, rpcReq(2, "tools/list", map[string]any{}))
	if bytes.Contains(raw, []byte("null")) {
		t.Fatalf("the list the client gets still holds a null: %s", raw)
	}
}

// ── B7 11 (author__X) ─────────────────────────────────────────────────────────────────────────────────────────────

func TestPassThrough_OldNamesWorkAndAreNotListed(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	_, names := h.listed("s", testTok)
	if strings.Contains(strings.Join(names, ","), "author__") {
		t.Fatalf("an old name is listed: %v", names)
	}
	for _, tl := range cpTools() {
		old := strings.Replace(tl.Name, "author_", "author__", 1)
		raw := h.call("s", testTok, 14, old, map[string]any{"instance_id": "suta", "content": "c"})
		if strings.Contains(string(raw), `"code":-32601`) {
			t.Fatalf("the old name %s is not accepted: %s", old, raw)
		}
		if got := h.cp.lastCall().Tool; got != tl.Name {
			t.Fatalf("%s was forwarded as %q, want %q", old, got, tl.Name)
		}
	}
}

// ── B7 13: a session adopted after a router restart ──────────────────────────────────────────────────────────────

func TestPassThrough_AnAdoptedSessionBuildsOnFirstUse_ParallelFirstUsesReadTheListOnce(t *testing.T) {
	h := newHarness(t, Options{})
	h.cp.setMode("slowlist")
	h.srv.AdoptSession("restarted") // the router process restarted; the agent's client kept its session id
	var wg sync.WaitGroup
	got := make([][]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, got[i] = h.listed("restarted", testTok)
		}(i)
	}
	wg.Wait()
	if n := h.cp.count("tools/list"); n != 1 {
		t.Fatalf("two parallel first uses read the control plane's list %d times, want once", n)
	}
	for i, names := range got {
		if len(authorNames(names)) != len(cpTools()) {
			t.Fatalf("caller %d: %v", i, names)
		}
	}
}

// ── the runner tools: passed on from the executor (AC-D70) ────────────────────────────────────────────────────────

// The runner CALL path: the answer is the executor's own, byte for byte (decision 14); the call carries the folder's own
// executor token and instance_id "local"; and the folder rule still refuses, with today's text, before any forward.
func TestPassThrough_ARunnerCallIsTheExecutorsOwnAnswer(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	routed := h.call("s", testTok, 15, "runner_get_report", map[string]any{"instance_id": "suta", "run_id": "last-run"})
	direct := h.ex["suta"].direct(t, "author-suta", 15, "tools/call", map[string]any{"name": "runner_get_report",
		"arguments": map[string]any{"instance_id": "local", "run_id": "last-run"}})
	if string(routed) != string(direct) {
		t.Fatalf("the router's answer is not the executor's own\nrouter:   %s\nexecutor: %s", routed, direct)
	}
	if !strings.Contains(string(routed), sentinelExpected) {
		t.Fatalf("the test folder's report lost its expected value: %s", routed)
	}
	got := h.ex["suta"].lastCall()
	if got.Tool != "runner_get_report" || got.Bearer != "Bearer author-suta" || !strings.Contains(string(got.Args), `"instance_id":"local"`) {
		t.Fatalf("runner forward: %+v %s", got, got.Args)
	}
	before := h.execToolCalls()
	raw := h.call("s", testTok, 16, "runner_run", map[string]any{"instance_id": "sutz"})
	if !strings.Contains(string(raw), "not onboarded into this folder") || h.execToolCalls() != before {
		t.Fatalf("the runner folder rule changed: %s (%d forwards)", raw, h.execToolCalls()-before)
	}
}

// ── the holdout invariants ───────────────────────────────────────────────────────────────────────────────────────

// The session set holds no token and no URL: a token rotated after the set was built is the token the next call carries.
func TestPassThrough_ARotatedTokenIsUsedWithoutAReconnect(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	h.cp.mu.Lock()
	h.cp.accepted[cpTokB] = true
	h.cp.mu.Unlock()
	next := h.table(func(f *Folder) {
		r := *f.record
		r.Token = cpTokB
		f.record = &r
	})
	h.tbl.Replace(next)
	h.call("s", testTok, 17, "author_list_runs", map[string]any{"instance_id": "suta"})
	if got := h.cp.lastCall().Bearer; got != "Bearer "+cpTokB {
		t.Fatalf("the call carried %q — the set kept the old token", got)
	}
}

// The session set holds no token and no URL — the control plane's or any executor's — for a usable part and for every
// kind of unusable one. The walk is DEEP: it follows pointers (the runner snapshot is reached through one) and every map,
// slice and string the stored state and the tools hold, so a field added later is walked too.
func TestPassThrough_TheStoredStateHoldsNoTokenAndNoURL(t *testing.T) {
	for _, c := range []struct {
		name  string
		opt   folderOpt
		setup func(h *harness)
	}{
		{"usable", nil, nil},
		{"record gone", func(f *Folder) { f.record = nil }, nil},
		{"no token", func(f *Folder) { r := *f.record; r.Token = ""; f.record = &r }, nil},
		{"an executor unreachable", nil, func(h *harness) { h.ex["suta"].setMode("drop") }},
		{"an executor refusing the token", nil, func(h *harness) { h.ex["suta"].setMode("status:401") }},
		{"an executor breaching decision 13", nil, func(h *harness) {
			h.ex["sutb"].setRawList(`{"tools":[{"name":"author_x","description":"d","inputSchema":{"type":"object"}}]}`)
		}},
		{"an executor tool not passed on", nil, func(h *harness) {
			h.ex["sutb"].setRawList(`{"tools":[{"name":"runner___x","description":"d","inputSchema":{"type":"object"}},{"name":"runner_y","description":"d","inputSchema":"no"}]}`)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Options{})
			if c.opt != nil {
				h.tbl.Replace(h.table(c.opt))
			}
			if c.setup != nil {
				c.setup(h)
			}
			s := newSessions(h.tbl, Options{ListTimeout: time.Second})
			set := s.sessionTools("x", mcpserver.Principal{Hat: role.Test, Subject: testTok})
			var strs []string
			collectStrings(reflect.ValueOf(set.State), map[uintptr]bool{}, &strs)
			collectStrings(reflect.ValueOf(set.Instructions), map[uintptr]bool{}, &strs)
			for _, tl := range set.Tools.Tools() {
				collectStrings(reflect.ValueOf(tl.InputSchema), map[uintptr]bool{}, &strs)
				strs = append(strs, tl.Name, tl.Description, strings.Join(tl.Aliases, ","))
				if tl.Async {
					t.Fatalf("%s is Async — the router must never mint its own run id (INT-037)", tl.Name)
				}
			}
			if len(strs) < 10 {
				t.Fatalf("the walk found %d strings — it is not looking inside the state", len(strs))
			}
			secrets := []string{cpTokA, testTok, prodTok, otherTok, h.cp.URL(), strings.TrimPrefix(h.cp.ts.URL, "http://")}
			for id, e := range h.ex {
				secrets = append(secrets, "author-"+id, "runner-"+id, e.URL(), strings.TrimPrefix(e.ts.URL, "http://"))
			}
			for _, s := range strs {
				for _, secret := range secrets {
					if strings.Contains(s, secret) {
						t.Fatalf("the session set holds %q (in %q)", secret, s)
					}
				}
			}
		})
	}
}

// collectStrings appends every string reachable from v — through pointers, interfaces, structs (unexported fields too),
// maps (keys and values) and slices. seen stops a cycle.
func collectStrings(v reflect.Value, seen map[uintptr]bool, out *[]string) {
	switch v.Kind() {
	case reflect.String:
		*out = append(*out, v.String())
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return
		}
		seen[v.Pointer()] = true
		collectStrings(v.Elem(), seen, out)
	case reflect.Interface:
		if !v.IsNil() {
			collectStrings(v.Elem(), seen, out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			collectStrings(v.Field(i), seen, out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			collectStrings(k, seen, out)
			collectStrings(v.MapIndex(k), seen, out)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			collectStrings(v.Index(i), seen, out)
		}
	}
}

// A reload that turns the folder into a product folder while a TEST set is stored: nothing author is listed or callable,
// and the author handler called directly forwards nothing.
func TestPassThrough_AHatFlipUnderAStoredSetExposesNothing(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	flipped := New()
	if err := flipped.AddFolder(Folder{Path: "C:/work/test-agent", Hat: role.Product, Token: testTok,
		Upstreams: map[string]Upstream{"suta": {URL: h.ex["suta"].URL(), Token: "runner-suta"}}}); err != nil {
		t.Fatal(err)
	}
	h.tbl.Replace(flipped)
	_, names := h.listed("s", testTok)
	if len(authorNames(names)) != 0 {
		t.Fatalf("after the flip the session still lists author tools: %v", names)
	}
	before := h.cp.count("tools/call")
	if raw := h.call("s", testTok, 18, "author_list_runs", map[string]any{"instance_id": "suta"}); !strings.Contains(string(raw), `"code":-32001`) {
		t.Fatalf("after the flip an author call was not refused: %s", raw)
	}
	s := newSessions(h.tbl, Options{})
	out := s.authorHandler("author_list_runs")(json.RawMessage(`{"instance_id":"suta"}`), mcpserver.Principal{Hat: role.Test, Subject: testTok})
	if !out.IsError || h.cp.count("tools/call") != before {
		t.Fatalf("the handler forwarded for a folder that is now a product folder: %+v", out)
	}
}

// A state cause fixed while the session is open (onboarding minted the token): the call says so, truthfully.
func TestPassThrough_ACauseFixedMidSessionSaysReconnect(t *testing.T) {
	h := newHarness(t, Options{}, func(f *Folder) { r := *f.record; r.Token = ""; f.record = &r })
	if res := h.initialize("s", testTok); res["instructions"] != textNoToken {
		t.Fatalf("start: %v", res["instructions"])
	}
	h.tbl.Replace(h.table()) // the token arrives
	raw := h.call("s", testTok, 19, "author_list_runs", map[string]any{"instance_id": "suta"})
	if !strings.Contains(string(raw), jsonText(textBecameAvailable)) {
		t.Fatalf("got %s", raw)
	}
}

func TestPassThrough_AFailedPartAsksForARetryAUsableOneDoesNot(t *testing.T) {
	h := newHarness(t, Options{})
	s := newSessions(h.tbl, Options{ListTimeout: time.Second})
	if set := s.sessionTools("x", mcpserver.Principal{Hat: role.Test, Subject: testTok}); set.RetryAfter != 0 {
		t.Fatalf("a usable set asks for a retry: %v", set.RetryAfter)
	}
	if set := s.sessionTools("x", mcpserver.Principal{Hat: role.Product, Subject: prodTok}); set.RetryAfter != 0 {
		t.Fatalf("a product set asks for a retry: %v", set.RetryAfter)
	}
	h.cp.setMode("status:503")
	if set := s.sessionTools("x", mcpserver.Principal{Hat: role.Test, Subject: testTok}); set.RetryAfter != retryFailedFirstUse {
		t.Fatalf("a failed read does not ask for a retry: %v", set.RetryAfter)
	}
}

// Many sessions of one folder share one copy of the definitions (R1). Since AC-D70 the key is the author list AND the
// runner snapshot — instance ids included — so folders with the same instances and lists share a set, and folders whose
// instances differ do not.
func TestPassThrough_SessionsOfOneFolderShareOneToolSet(t *testing.T) {
	h := newHarness(t, Options{})
	s := newSessions(h.tbl, Options{ListTimeout: time.Second})
	a1 := s.sessionTools("1", mcpserver.Principal{Hat: role.Test, Subject: testTok}).Tools
	a2 := s.sessionTools("2", mcpserver.Principal{Hat: role.Test, Subject: testTok}).Tools
	z := s.sessionTools("3", mcpserver.Principal{Hat: role.Test, Subject: otherTok}).Tools
	p1 := s.sessionTools("4", mcpserver.Principal{Hat: role.Product, Subject: prodTok}).Tools
	p2 := s.sessionTools("5", mcpserver.Principal{Hat: role.Product, Subject: prodTok}).Tools
	if a1 != a2 || p1 != p2 {
		t.Fatal("two sessions of one folder got two copies of its set")
	}
	if a1 == z {
		t.Fatal("two folders with different instances share one set — its runner tools would answer for the wrong instances")
	}
	if a1 == p1 {
		t.Fatal("a test folder with author tools and a product folder share one set")
	}
}

// The author plane uses the same vantage rewrite as the forwarder (B4: "the same address rewrite the forwarder uses").
func TestPassThrough_TheVantageRewriteAppliesToTheAuthorPlane(t *testing.T) {
	h := newHarness(t, Options{HostAlias: "localhost"})
	port := h.cp.ts.URL[strings.LastIndex(h.cp.ts.URL, ":")+1:]
	h.tbl.Replace(h.table(func(f *Folder) {
		u := "http://127.0.0.1:" + port + "/mcp"
		f.Cloud = &CloudRef{URL: u, User: "u"}
		f.record = &CloudRecord{URL: u, User: "u", Token: cpTokA}
	}))
	h.initialize("s", testTok)
	h.call("s", testTok, 20, "author_list_runs", map[string]any{"instance_id": "suta"})
	h.cp.mu.Lock()
	defer h.cp.mu.Unlock()
	for _, c := range h.cp.calls {
		if !strings.HasPrefix(c.Host, "localhost:") {
			t.Fatalf("a %s went to %q — the alias was not applied", c.Method, c.Host)
		}
	}
}

func TestListTimeoutFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
		warn bool
	}{{"", DefaultListTimeout, false}, {"15s", 15 * time.Second, false}, {"0s", DefaultListTimeout, true},
		{"-3s", DefaultListTimeout, true}, {"ten", DefaultListTimeout, true}, {"5m", MaxListTimeout, true}} {
		t.Setenv(ListTimeoutEnv, c.env)
		got, warn := ListTimeoutFromEnv()
		if got != c.want || (warn != "") != c.warn {
			t.Errorf("%q: got %v %q", c.env, got, warn)
		}
	}
}

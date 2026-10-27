package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// AC-D69: the author call path after the session started, shared tool sets, and the pins the behaviour depends on —
// the runner tools as the router listed them before AC-D69, the A6 sentences as the design writes them, and the two
// timeouts. The harness is session_test.go's: a real mcpserver as the control plane, the real router server.

const cpTokC = "odts_machine_token_CCCC" // a second control plane's machine token

// twoControlPlanes points the test folder (testTok) at h.cp and the other test folder (otherTok) at a second stub control
// plane that lists the SAME tools. Both folders hold ONE instance with the SAME id, "suta", but on two DIFFERENT
// executors (folder A: the suta executor, folder B: the sutz executor, each with its own token) that list the same tools —
// so the two folders build byte-identical author lists AND runner snapshots, and share one cached ToolSet, whose handlers
// were made by whichever folder built it first (since AC-D70 the snapshot, instance ids included, is part of the key).
func twoControlPlanes(t *testing.T, h *harness) *stubCP {
	t.Helper()
	cp2 := newStubCP(t, cpTools()...)
	cp2.mu.Lock()
	cp2.accepted = map[string]bool{cpTokC: true}
	cp2.mu.Unlock()
	tbl := New()
	for _, f := range []Folder{
		{Path: "C:/work/test-agent", Hat: role.Test, Token: testTok,
			Upstreams: map[string]Upstream{"suta": {URL: h.ex["suta"].URL(), Token: "author-suta"}},
			Cloud:     &CloudRef{URL: h.cp.URL(), User: "u"}, record: &CloudRecord{URL: h.cp.URL(), User: "u", Token: cpTokA}},
		{Path: "C:/elsewhere/test-agent", Hat: role.Test, Token: otherTok,
			Upstreams: map[string]Upstream{"suta": {URL: h.ex["sutz"].URL(), Token: "author-sutz"}},
			Cloud:     &CloudRef{URL: cp2.URL(), User: "v"}, record: &CloudRecord{URL: cp2.URL(), User: "v", Token: cpTokC}},
	} {
		if err := tbl.AddFolder(f); err != nil {
			t.Fatal(err)
		}
	}
	h.tbl.Replace(tbl)
	return cp2
}

// toolErrPayload decodes a router tool error's JSON text.
func toolErrPayload(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var r struct {
		Result mcpserver.ToolResult `json:"result"`
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &r); err != nil || !r.Result.IsError || len(r.Result.Content) != 1 ||
		json.Unmarshal([]byte(r.Result.Content[0].Text), &p) != nil {
		t.Fatalf("not a router tool error: %s", raw)
	}
	return p
}

// ── A3 last bullet / B4 step 1: a shared ToolSet never carries one folder's address or token into another's call ─────

func TestPassThrough_TwoFoldersSharingOneToolSetEachReachTheirOwnControlPlane(t *testing.T) {
	h := newHarness(t, Options{})
	cp2 := twoControlPlanes(t, h)
	// What makes this test mean something: the two folders' sets ARE one shared ToolSet.
	s := newSessions(h.tbl, Options{ListTimeout: 3 * time.Second})
	a := s.sessionTools("1", mcpserver.Principal{Hat: role.Test, Subject: testTok}).Tools
	b := s.sessionTools("2", mcpserver.Principal{Hat: role.Test, Subject: otherTok}).Tools
	if a == nil || a != b {
		t.Fatal("precondition: two folders whose control planes and executors list the same tools do not share one ToolSet")
	}

	h.initialize("a", testTok) // folder A builds first, so the shared set's handlers come from A's build
	h.initialize("b", otherTok)
	cp1Calls, cp2Calls := h.cp.count("tools/call"), cp2.count("tools/call")

	// B names its own instance: forwarded to B's control plane, with B's token.
	if raw := h.call("b", otherTok, 40, "author_list_runs", map[string]any{"instance_id": "suta"}); bytes.Contains(raw, []byte(`"isError":true`)) || bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("folder B's call for its own instance failed: %s", raw)
	}
	if got := cp2.lastCall(); got.Bearer != "Bearer "+cpTokC || cp2.count("tools/call") != cp2Calls+1 || h.cp.count("tools/call") != cp1Calls {
		t.Fatalf("folder B's call: bearer %q, B's control plane +%d, A's +%d — want B's token on B's control plane only",
			got.Bearer, cp2.count("tools/call")-cp2Calls, h.cp.count("tools/call")-cp1Calls)
	}
	// B names an instance outside its folder: refused by the router, nothing forwarded anywhere.
	raw := h.call("b", otherTok, 41, "author_list_runs", map[string]any{"instance_id": "sutb"})
	if p := toolErrPayload(t, raw); !strings.HasPrefix(fmt.Sprint(p["error"]), "router:") {
		t.Fatalf("folder B naming an instance outside its folder was not refused by the router: %s", raw)
	}
	if cp2.count("tools/call") != cp2Calls+1 || h.cp.count("tools/call") != cp1Calls {
		t.Fatal("a refused call reached a control plane")
	}
	// A names its own instance: A's control plane, A's token.
	h.call("a", testTok, 42, "author_list_runs", map[string]any{"instance_id": "suta"})
	if got := h.cp.lastCall(); got.Bearer != "Bearer "+cpTokA || h.cp.count("tools/call") != cp1Calls+1 {
		t.Fatalf("folder A's call: bearer %q, A's control plane +%d", got.Bearer, h.cp.count("tools/call")-cp1Calls)
	}

	// The runner plane (AC-D70): the same instance id in both folders, two executors. Each folder's runner call reaches
	// ITS executor with ITS token, whichever folder built the shared set.
	h.call("b", otherTok, 43, "runner_get_report", map[string]any{"instance_id": "suta"})
	h.call("a", testTok, 44, "runner_get_report", map[string]any{"instance_id": "suta"})
	if got := h.ex["sutz"].toolCalls(); len(got) != 1 || got[0].Bearer != "Bearer author-sutz" {
		t.Fatalf("folder B's runner call: %+v — want one call on B's executor, with B's token", got)
	}
	if got := h.ex["suta"].toolCalls(); len(got) != 1 || got[0].Bearer != "Bearer author-suta" {
		t.Fatalf("folder A's runner call: %+v — want one call on A's executor, with A's token", got)
	}
}

// B4 step 1: the address comes from the live record on every call — a reload that moves the folder to another control
// plane takes effect on the next call, with no reconnect.
func TestPassThrough_AReloadedControlPlaneAddressIsUsedWithoutAReconnect(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	if raw := h.call("s", testTok, 39, "author_list_runs", map[string]any{"instance_id": "suta"}); bytes.Contains(raw, []byte(`"error"`)) || h.cp.count("tools/call") != 1 {
		t.Fatalf("the call before the reload: %s", raw)
	}
	cp2 := newStubCP(t, cpTools()...) // accepts cpTokA
	h.tbl.Replace(h.table(func(f *Folder) {
		f.Cloud = &CloudRef{URL: cp2.URL(), User: "u"}
		f.record = &CloudRecord{URL: cp2.URL(), User: "u", Token: cpTokA}
	}))
	before := h.cp.count("tools/call")
	if raw := h.call("s", testTok, 43, "author_list_runs", map[string]any{"instance_id": "suta"}); bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("the call after the reload failed: %s", raw)
	}
	if cp2.count("tools/call") != 1 || h.cp.count("tools/call") != before {
		t.Fatalf("after the reload: new control plane %d calls, old one +%d — want 1 and 0", cp2.count("tools/call"), h.cp.count("tools/call")-before)
	}
}

// ── A6 "a call made after a part goes down fails as today" / B4 step 8 ───────────────────────────────────────────────

func TestPassThrough_AControlPlaneThatGoesDownMidSessionFailsAsToday(t *testing.T) {
	t.Run("not reachable", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		h.cp.ts.Close()
		raw := h.call("s", testTok, 44, "author_list_runs", map[string]any{"instance_id": "suta"})
		// A router tool error, byte for byte; since AC-D70.2 the text is the router's own sentence (no address).
		want := `{"jsonrpc":"2.0","id":44,"result":{"content":[{"type":"text","text":"{\"error\":\"router: the call got no usable answer from the control plane (instance suta): it is not reachable. The router's log has the details.\",\"instance\":\"suta\",\"plane\":\"author\",\"reached\":false}"}],"isError":true}}`
		if string(raw) != want {
			t.Fatalf("\n got: %s\nwant: %s", raw, want)
		}
	})
	for _, code := range []string{"401", "403", "503"} {
		t.Run("HTTP "+code, func(t *testing.T) {
			h := newHarness(t, Options{})
			h.initialize("s", testTok)
			h.cp.setMode("status:" + code)
			p := toolErrPayload(t, h.call("s", testTok, 45, "author_list_runs", map[string]any{"instance_id": "suta"}))
			want := fmt.Sprintf(textNoAnswerFmt, "the control plane (instance suta)", "it answered with HTTP "+code)
			if p["error"] != want || p["reached"] != false || p["plane"] != "author" || p["instance"] != "suta" {
				t.Fatalf("got %v — want %q with reached false", p, want)
			}
		})
	}
}

// ── B9 / B2 "no lock while building", across folders ─────────────────────────────────────────────────────────────

func TestPassThrough_ASlowControlPlaneDoesNotHoldBackAnotherFoldersSessionStart(t *testing.T) {
	h := newHarness(t, Options{ListTimeout: 2 * time.Second})
	twoControlPlanes(t, h)
	h.cp.setMode("hang") // folder A's control plane does not answer
	done := make(chan struct{})
	go func() { // raw Dispatch: no t in a goroutine
		h.srv.Dispatch("a", testTok, rpcReq(1, "initialize", map[string]any{}))
		close(done)
	}()
	waitFor(t, "folder A's list read to reach its control plane", func() bool { return h.cp.count("initialize") >= 1 })
	start := time.Now()
	res := h.initialize("b", otherTok)
	took := time.Since(start)
	select {
	case <-done:
		t.Fatal("precondition: folder A's session start ended before folder B's was tried")
	default:
	}
	if took > time.Second || res["instructions"] != nil {
		t.Fatalf("folder B's session start took %s (instructions %v) while folder A's control plane hung — a build held a lock", took, res["instructions"])
	}
	<-done
}

// ── A6: one text per cause, as the design writes it; only the first says "could not reach" ─────────────────────────

func TestPassThrough_TheA6TextsAreTheDesignsSentences(t *testing.T) {
	designs := []struct{ cause, got, want string }{
		{"not reachable or timed out", textCPUnreachable, "Argus could not reach the control plane when it read the tools for this session, so the author tools are not listed. Make sure the control plane is up, then reconnect (in Claude Code: /mcp)."},
		{"refused the token", textCPRefused, "The control plane refused this machine's author token, so the author tools are not listed. Onboard this folder again (onboarding step 8b mints a new token), then reconnect."},
		{"answered with an error", fmt.Sprintf(textCPErrorFmt, "HTTP 503"), "The control plane answered with an error (HTTP 503) when Argus read the tools for this session. Try again later, then reconnect."},
		{"no record", textNoRecord, "No control plane is configured for this folder, so it has no author tools."},
		{"record gone", textRecordGone, "This folder refers to a control-plane record this machine no longer holds — onboard again."},
		{"no token", textNoToken, "This machine holds no author token for this control plane yet — onboarding mints it at step 8b."},
	}
	seen := map[string]string{}
	for _, d := range designs {
		if d.got != d.want {
			t.Errorf("%s:\n got: %s\nwant: %s", d.cause, d.got, d.want)
		}
		if other, dup := seen[d.got]; dup {
			t.Errorf("%q and %q share one text", d.cause, other)
		}
		seen[d.got] = d.cause
	}
	for _, text := range []string{textCPUnreachable, textCPRefused, textCPErrorFmt, textNoRecord, textRecordGone, textNoToken,
		textRunnerBreach, textBecameAvailable, textNotPassedOnFmt} {
		if says := strings.Contains(text, "could not reach"); says != (text == textCPUnreachable) {
			t.Errorf("only the unreachable text may say \"could not reach\": %q", text)
		}
	}
}

// ── A3 / VR-R7: the product folder's refusal is the server's own, as before ─────────────────────────────────────────

// The answer and the metric a product folder's author call got before AC-D69, when the router's author tools were static:
// the server's scope refusal — its instance read the way the server reads it, a key that only folds to instance_id
// included — counted as "denied". The label is now one fixed value, so names a caller invents never grow the metrics.
func TestPassThrough_AProductFoldersAuthorCallIsTheServersOwnRefusal(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("p", prodTok)
	raw := h.call("p", prodTok, 51, "author__read_scenario", json.RawMessage(`{"INSTANCE_ID":"suta"}`))
	if want := `{"jsonrpc":"2.0","id":51,"error":{"code":-32001,"message":"not permitted for the product scope: author__read_scenario","data":{"instance_id":"suta"}}}`; string(raw) != want {
		t.Fatalf("\n got: %s\nwant: %s", raw, want)
	}
	if m := h.srv.Metrics(); !strings.Contains(m, `tool="(unlisted)",instance_id="suta",outcome="denied"} 1`) || strings.Contains(m, "author__read_scenario") {
		t.Fatalf("the refusal was not counted as denied under the fixed label:\n%s", m)
	}
	if n := len(h.cp.calls); n != 0 {
		t.Fatalf("the product folder caused %d control-plane requests", n)
	}
}

// ── A9 #9 / decision 14: the same call, the same answer ──────────────────────────────────────────────────────────

func TestPassThrough_AnUnlistedAuthorNameGetsTheControlPlanesOwnAnswer(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	for _, name := range []string{"author_nope", "author__nope"} {
		args := map[string]any{"instance_id": "suta"}
		routed := h.call("s", testTok, 46, name, args)
		direct := h.cp.direct(t, "d", 46, "tools/call", map[string]any{"name": name, "arguments": args})
		if string(routed) != string(direct) || !bytes.Contains(routed, []byte(`"code":-32601`)) {
			t.Fatalf("%s:\nrouter: %s\ndirect: %s", name, routed, direct)
		}
	}
}

func TestPassThrough_ArgumentsThatAreNotAnObjectPassThroughUnchanged(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	for _, c := range []struct {
		name string
		args json.RawMessage
	}{{"absent", nil}, {"null", json.RawMessage(`null`)}, {"a list", json.RawMessage(`[]`)}, {"a string", json.RawMessage(`"x"`)}} {
		params := map[string]any{"name": "author_list_runs"}
		if c.args != nil {
			params["arguments"] = c.args
		}
		before := h.cp.count("tools/call")
		routed, _ := h.srv.Dispatch("s", testTok, rpcReq(47, "tools/call", params))
		direct := h.cp.direct(t, "d", 47, "tools/call", params)
		if string(routed) != string(direct) {
			t.Errorf("%s: answers differ\nrouter: %s\ndirect: %s", c.name, routed, direct)
		}
		if h.cp.count("tools/call") != before+1 {
			t.Errorf("%s: the call was not forwarded", c.name)
		}
		if got := h.cp.lastCall().Args; c.args != nil && string(got) != string(c.args) {
			t.Errorf("%s: the control plane received %s", c.name, got)
		}
	}
}

func TestPassThrough_ARichResultComesBackByteForByte(t *testing.T) {
	h := newHarness(t, Options{})
	rich := `{"content":[{"type":"text","text":"a"},{"type":"image","data":"iVBORw0KGgo=","mimeType":"image/png"}],"structuredContent":{"n":1.50,"k":[true,null]},"isError":false,"_meta":{"x":"y"}}`
	h.cp.setTools(append(cpTools(), mcpserver.Tool{Name: "author_rich", Namespace: mcpserver.NSAuthor, Description: "rich",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"instance_id": map[string]any{"type": "string"}}},
		Handler: func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome {
			return mcpserver.Outcome{Result: json.RawMessage(rich)}
		}})...)
	h.initialize("s", testTok)
	args := map[string]any{"instance_id": "suta"}
	routed := h.call("s", testTok, 48, "author_rich", args)
	direct := h.cp.direct(t, "d", 48, "tools/call", map[string]any{"name": "author_rich", "arguments": args})
	if string(routed) != string(direct) {
		t.Fatalf("\nrouter: %s\ndirect: %s", routed, direct)
	}
	for _, part := range []string{`"structuredContent":{"n":1.50`, `"mimeType":"image/png"`, `"_meta":{"x":"y"}`} {
		if !bytes.Contains(routed, []byte(part)) {
			t.Fatalf("%s was lost on the way: %s", part, routed)
		}
	}
}

// ── the two timeouts ───────────────────────────────────────────────────────────────────────────────────────────────

func TestPassThrough_TheListTimeoutBoundsTheSessionStart(t *testing.T) {
	h := newHarness(t, Options{ListTimeout: 300 * time.Millisecond})
	h.cp.setMode("hang")
	start := time.Now()
	res := h.initialize("s", testTok)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("initialize took %s with a 300 ms list timeout", took)
	}
	if res["instructions"] != textCPUnreachable {
		t.Fatalf("instructions %v", res["instructions"])
	}
}

func TestPassThrough_TheAuthorCallTimeoutIsTheRoutersOwnBound(t *testing.T) {
	if DefaultAuthorCallTimeout < 120*time.Second {
		t.Fatalf("DefaultAuthorCallTimeout %s is shorter than the 120 s the control plane sizes its slowest calls to", DefaultAuthorCallTimeout)
	}
	if s := newSessions(New(), Options{}); s.opts.CallTimeout != DefaultAuthorCallTimeout {
		t.Fatalf("an unset CallTimeout became %s", s.opts.CallTimeout)
	}
	slow := mcpserver.Tool{Name: "author_slow", Namespace: mcpserver.NSAuthor, Description: "slow", InputSchema: map[string]any{"type": "object"},
		Handler: func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome {
			time.Sleep(600 * time.Millisecond)
			return mcpserver.Ok("done")
		}}
	args := map[string]any{"instance_id": "suta"}

	// Inside the bound: the control plane's own answer.
	h := newHarness(t, Options{CallTimeout: 5 * time.Second})
	h.cp.setTools(append(cpTools(), slow)...)
	h.initialize("s", testTok)
	routed := h.call("s", testTok, 49, "author_slow", args)
	if direct := h.cp.direct(t, "d", 49, "tools/call", map[string]any{"name": "author_slow", "arguments": args}); string(routed) != string(direct) {
		t.Fatalf("a slow call inside the bound\nrouter: %s\ndirect: %s", routed, direct)
	}

	// Past the bound: today's text, at the bound — not at the client's own 30 s default.
	h2 := newHarness(t, Options{CallTimeout: 300 * time.Millisecond})
	h2.cp.setTools(append(cpTools(), slow)...)
	h2.initialize("s", testTok)
	start := time.Now()
	p := toolErrPayload(t, h2.call("s", testTok, 50, "author_slow", args))
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the call took %s with a 300 ms bound", took)
	}
	if p["error"] != fmt.Sprintf(textNoAnswerFmt, "the control plane (instance suta)", kindTimedOut) || p["reached"] != false {
		t.Fatalf("past the bound: %v", p)
	}
}

func TestAuthorCallTimeoutFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
		warn bool
	}{{"", DefaultAuthorCallTimeout, false}, {"5m", 5 * time.Minute, false}, {"30s", 30 * time.Second, false},
		{"10s", MinAuthorCallTimeout, true}, {"0s", DefaultAuthorCallTimeout, true}, {"-1m", DefaultAuthorCallTimeout, true},
		{"slow", DefaultAuthorCallTimeout, true}, {"2h", MaxAuthorCallTimeout, true}} {
		t.Setenv(AuthorCallTimeoutEnv, c.env)
		got, warn := AuthorCallTimeoutFromEnv()
		if got != c.want || (warn != "") != c.warn {
			t.Errorf("%q: got %v %q", c.env, got, warn)
		}
	}
}

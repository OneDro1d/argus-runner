package router

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// AC-D70 (design #678 A6, A9 #12 P2, B7 9, B4 "fetch in parallel"): an executor that cannot be used. Its tools are
// missing for that session; the session start says why, one sentence per cause; a call naming that instance gets the
// same sentence — by the listed name, the old name, a name only another instance offers, or no instance at all — and
// nothing is sent to that executor after the session started. A NEW session start after the executor is back lists its
// tools again.

// The executor A6 texts, as the design writes them (A6: "the same texts with 'the executor of instance …' and 'its runner
// tools', and 'Make sure the instance is running'"). Only the first says "could not reach".
func TestRunnerCauses_TheExecutorTextsAreTheDesignsSentences(t *testing.T) {
	for _, d := range []struct{ got, want string }{
		{fmt.Sprintf(textExecUnreachableFmt, "memstore-compose"), "Argus could not reach the executor of instance memstore-compose when it read the tools for this session, so its runner tools are not listed. Make sure the instance is running, then reconnect (in Claude Code: /mcp)."},
		{fmt.Sprintf(textExecRefusedFmt, "memstore-compose", "memstore-compose"), "The executor of instance memstore-compose refused this folder's executor token, so its runner tools are not listed. Onboard instance memstore-compose again (onboarding step 7/8 writes this folder's executor token), then reconnect."},
		{fmt.Sprintf(textExecErrorFmt, "memstore-compose", "HTTP 503"), "The executor of instance memstore-compose answered with an error (HTTP 503) when Argus read the tools for this session. Try again later, then reconnect."},
	} {
		if d.got != d.want {
			t.Errorf("\n got: %s\nwant: %s", d.got, d.want)
		}
	}
	for _, text := range []string{textExecUnreachableFmt, textExecRefusedFmt, textExecErrorFmt, textExecNoAddressFmt, textExecNoTokenFmt,
		textExecBreachFmt, textExecNotPassedOnFmt, textExecUsableNowFmt, textNotOfferedUpdateFmt, textNotOfferedOlderFmt} {
		if says := strings.Contains(text, "could not reach"); says != (text == textExecUnreachableFmt) {
			t.Errorf("only the unreachable text may say \"could not reach\": %q", text)
		}
		for _, banned := range []string{"Bearer", "http://", "https://", "token="} {
			if strings.Contains(text, banned) {
				t.Errorf("a text carries %q: %q", banned, text)
			}
		}
	}
}

func TestRunnerCauses_EveryCauseHasItsTextAtStartAndTheSameTextOnACall(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *stubExec, up *Upstream)
		want  string
	}{
		{"not reachable", func(e *stubExec, up *Upstream) { up.URL = closedURL() }, fmt.Sprintf(textExecUnreachableFmt, "a")},
		{"connection dropped", func(e *stubExec, _ *Upstream) { e.setMode("drop") }, fmt.Sprintf(textExecUnreachableFmt, "a")},
		{"timed out", func(e *stubExec, _ *Upstream) { e.setMode("hang") }, fmt.Sprintf(textExecUnreachableFmt, "a")},
		{"401 — the token refused", func(e *stubExec, up *Upstream) { up.Token = "runner-revoked" }, fmt.Sprintf(textExecRefusedFmt, "a", "a")},
		{"403", func(e *stubExec, _ *Upstream) { e.setMode("status:403") }, fmt.Sprintf(textExecRefusedFmt, "a", "a")},
		{"404", func(e *stubExec, _ *Upstream) { e.setMode("status:404") }, fmt.Sprintf(textExecErrorFmt, "a", "HTTP 404")},
		{"503", func(e *stubExec, _ *Upstream) { e.setMode("status:503") }, fmt.Sprintf(textExecErrorFmt, "a", "HTTP 503")},
		{"JSON-RPC error", func(e *stubExec, _ *Upstream) { e.setMode("rpcerr") }, fmt.Sprintf(textExecErrorFmt, "a", "JSON-RPC error -32603")},
		{"not a tool list", func(e *stubExec, _ *Upstream) { e.setMode("badanswer") }, fmt.Sprintf(textExecErrorFmt, "a", "an answer that is not a tool list")},
		{"no address", func(e *stubExec, up *Upstream) { up.URL = "" }, fmt.Sprintf(textExecNoAddressFmt, "a", "a")},
		{"no token", func(e *stubExec, up *Upstream) { up.Token = " " }, fmt.Sprintf(textExecNoTokenFmt, "a", "a")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newStubExec(t, "0.3.73", "runner-a", "author-a")
			up := Upstream{URL: e.URL(), Token: "runner-a"}
			c.setup(e, &up)
			r := newRig(t, Options{ListTimeout: 300 * time.Millisecond}, productFolder(pTok, map[string]Upstream{"a": up}))
			res := r.initialize("s", pTok)
			if res["instructions"] != c.want {
				t.Fatalf("session start text\n got: %v\nwant: %s", res["instructions"], c.want)
			}
			if _, names := r.listed("s", pTok); len(names) != 0 {
				t.Fatalf("a one-instance folder whose executor cannot be used lists %v", names)
			}
			lists, calls := e.count("tools/list"), e.count("tools/call")
			for _, call := range []struct {
				name string
				args map[string]any
			}{
				{"runner_get_report", map[string]any{"instance_id": "a"}},  // a name the executor offers, not listed here
				{"runner__get_report", map[string]any{"instance_id": "a"}}, // its old name
				{"runner_run", map[string]any{"instance_id": " a "}},       // a spaced id
				{"runner_get_report", map[string]any{}},                    // no instance named
			} {
				if got := toolErrorText(t, r.call("s", pTok, 3, call.name, call.args)); got != c.want {
					t.Fatalf("%s %v:\n got: %s\nwant: %s", call.name, call.args, got, c.want)
				}
			}
			if e.count("tools/list") != lists || e.count("tools/call") != calls {
				t.Fatalf("the unusable executor was asked again: +%d tools/list, +%d tools/call", e.count("tools/list")-lists, e.count("tools/call")-calls)
			}
			for _, l := range strings.Split(r.logged(), "\n") {
				for _, secret := range []string{"runner-a", "author-a", "runner-revoked", pTok} {
					if strings.Contains(l, secret) {
						t.Fatalf("a log line carries a token: %q", l)
					}
				}
			}
		})
	}
}

// Two instances down for different reasons: a call gets ITS target's sentence, not both, and not the other's; and a
// name the healthy third instance offers still names the down target's cause.
func TestRunnerCauses_ACallGetsItsOwnTargetsSentence(t *testing.T) {
	down := newStubExec(t, "0.3.73", "runner-d", "author-d")
	down.setMode("drop")
	refusing := newStubExec(t, "0.3.73", "runner-r", "author-r")
	ok := newStubExec(t, "0.3.73", "runner-o", "author-o")
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{
		"d": {URL: down.URL(), Token: "runner-d"}, "r": {URL: refusing.URL(), Token: "wrong"}, "o": {URL: ok.URL(), Token: "runner-o"}}))
	res := r.initialize("s", pTok)
	if want := fmt.Sprintf(textExecUnreachableFmt, "d") + "\n" + fmt.Sprintf(textExecRefusedFmt, "r", "r"); res["instructions"] != want {
		t.Fatalf("session start:\n got: %v\nwant: %s", res["instructions"], want)
	}
	// Here runner_get_report IS listed (o offers it), so these calls take the listed tool's path, not the unlisted one —
	// and the sentence must come from the session start, with NO request to either unusable executor (A6, B7 9).
	asked := func() int {
		n := 0
		for _, e := range []*stubExec{down, refusing} {
			n += e.count("initialize") + e.count("tools/list") + e.count("tools/call")
		}
		return n
	}
	before := asked()
	if got := toolErrorText(t, r.call("s", pTok, 3, "runner_get_report", map[string]any{"instance_id": "d"})); got != fmt.Sprintf(textExecUnreachableFmt, "d") {
		t.Fatalf("d: %q", got)
	}
	if got := toolErrorText(t, r.call("s", pTok, 4, "runner_get_report", map[string]any{"instance_id": "r"})); got != fmt.Sprintf(textExecRefusedFmt, "r", "r") {
		t.Fatalf("r: %q", got)
	}
	if after := asked(); after != before {
		t.Fatalf("%d requests reached the two unusable executors after the session started", after-before)
	}
	if raw := r.call("s", pTok, 5, "runner_get_report", map[string]any{"instance_id": "o"}); bytes.Contains(raw, []byte(`"isError":true`)) {
		t.Fatalf("the healthy instance: %s", raw)
	}
}

// A9 #12 P2 / Done-when 8: stopped at session start → missing and explained; running again → a NEW session start on the
// same session id (the client's reconnect) lists the tools with no text, and a call works.
func TestRunnerCauses_AStoppedExecutorIsBackAfterAReconnect(t *testing.T) {
	e := newStubExec(t, "0.3.73", "runner-a", "author-a")
	e.setMode("drop")
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	if res := r.initialize("s", pTok); res["instructions"] != fmt.Sprintf(textExecUnreachableFmt, "a") {
		t.Fatalf("stopped: %v", res["instructions"])
	}
	e.setMode("")
	if res := r.initialize("s", pTok); res["instructions"] != nil {
		t.Fatalf("running again, the new session start still says: %v", res["instructions"])
	}
	if _, names := r.listed("s", pTok); strings.Join(names, ",") != runnerNames {
		t.Fatalf("after the reconnect: %v", names)
	}
	if raw := r.call("s", pTok, 3, "runner_get_report", map[string]any{"instance_id": "a"}); bytes.Contains(raw, []byte(`"isError":true`)) {
		t.Fatalf("after the reconnect a call fails: %s", raw)
	}
}

// A6 / B4 step 8: an executor that goes down AFTER the session started fails a call with a router tool error,
// reached:false — byte for byte; since AC-D70.2 the text is the router's own sentence naming the instance and the kind.
func TestRunnerCauses_AnExecutorThatGoesDownMidSessionFailsAsToday(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	h.ex["suta"].ts.Close()
	raw := h.call("s", testTok, 44, "runner_get_report", map[string]any{"instance_id": "suta"})
	want := `{"jsonrpc":"2.0","id":44,"result":{"content":[{"type":"text","text":"{\"error\":\"router: the call got no usable answer from the executor of instance suta: it is not reachable. The router's log has the details.\",\"instance\":\"suta\",\"plane\":\"runner\",\"reached\":false}"}],"isError":true}}`
	if string(raw) != want {
		t.Fatalf("\n got: %s\nwant: %s", raw, want)
	}
}

// A D-5 state cause (only a hand-edited state.json holds one) that the folder no longer has: the call says the truth
// NOW — reconnect — instead of repeating "onboard again" after the operator did.
func TestRunnerCauses_AStateCauseFixedMidSessionSaysReconnect(t *testing.T) {
	e := newStubExec(t, "0.3.73", "runner-a", "author-a")
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: ""}}))
	if res := r.initialize("s", pTok); res["instructions"] != fmt.Sprintf(textExecNoTokenFmt, "a", "a") {
		t.Fatalf("start: %v", res["instructions"])
	}
	r.replace(productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	if got := toolErrorText(t, r.call("s", pTok, 3, "runner_get_report", map[string]any{"instance_id": "a"})); got != fmt.Sprintf(textExecUsableNowFmt, "a") {
		t.Fatalf("got %q", got)
	}
}

// T1: the same two rules for a LISTED tool — the folder has a healthy instance, so runner_get_report is in the session's
// list and the call takes the listed tool's path. A state cause fixed since the session started says reconnect; an
// executor that broke decision 13 at session start stays unused even after it lists cleanly. Neither executor is asked.
func TestRunnerCauses_AnUnusableInstanceBesideAHealthyOneIsNotAsked(t *testing.T) {
	ok := newStubExec(t, "0.3.73", "runner-o", "author-o")
	t.Run("a state cause fixed mid-session", func(t *testing.T) {
		e := newStubExec(t, "0.3.73", "runner-a", "author-a")
		r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: ""}, "o": {URL: ok.URL(), Token: "runner-o"}}))
		if res := r.initialize("s", pTok); res["instructions"] != fmt.Sprintf(textExecNoTokenFmt, "a", "a") {
			t.Fatalf("start: %v", res["instructions"])
		}
		r.replace(productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}, "o": {URL: ok.URL(), Token: "runner-o"}}))
		if got := toolErrorText(t, r.call("s", pTok, 3, "runner_get_report", map[string]any{"instance_id": "a"})); got != fmt.Sprintf(textExecUsableNowFmt, "a") {
			t.Fatalf("got %q", got)
		}
		if e.count("initialize")+e.count("tools/list")+e.count("tools/call") != 0 {
			t.Fatalf("the executor was asked: %d initialize, %d tools/list, %d tools/call", e.count("initialize"), e.count("tools/list"), e.count("tools/call"))
		}
	})
	t.Run("decision 13 at session start, a clean list later", func(t *testing.T) {
		bad := newStubExec(t, "0.3.73", "runner-b", "author-b")
		bad.setRawList(`{"tools":[{"name":"author_x","description":"d","inputSchema":{"type":"object"}}]}`)
		r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"b": {URL: bad.URL(), Token: "runner-b"}, "o": {URL: ok.URL(), Token: "runner-o"}}))
		text, _ := r.initialize("s", pTok)["instructions"].(string)
		if !strings.HasPrefix(text, "The executor of instance b listed a tool that is not a runner tool") {
			t.Fatalf("start: %q", text)
		}
		bad.setMode("") // its real tools from now on
		lists := bad.count("tools/list")
		if got := toolErrorText(t, r.call("s", pTok, 4, "runner_get_report", map[string]any{"instance_id": "b"})); got != text {
			t.Fatalf("got %q, want its session-start sentence", got)
		}
		if bad.count("tools/list") != lists || bad.count("tools/call") != 0 {
			t.Fatalf("the breaching executor was asked: +%d tools/list, %d tools/call", bad.count("tools/list")-lists, bad.count("tools/call"))
		}
	})
}

// R10: a session adopted without an initialize (its client never received the instructions) is built again in the
// background after RetryAfter when an executor read FAILED — a product folder too — and not for a decision-13 breach.
func TestRunnerCauses_AFailedExecutorReadAsksForARetry(t *testing.T) {
	ok := newStubExec(t, "0.3.73", "runner-a", "author-a")
	down := newStubExec(t, "0.3.73", "runner-b", "author-b")
	down.setMode("drop")
	bad := newStubExec(t, "0.3.73", "runner-c", "author-c")
	bad.setRawList(`{"tools":[{"name":"author_x","description":"d","inputSchema":{"type":"object"}}]}`)
	tbl := New()
	for _, f := range []Folder{
		productFolder(pTok, map[string]Upstream{"a": {URL: ok.URL(), Token: "runner-a"}}),
		productFolder(p2Tok, map[string]Upstream{"b": {URL: down.URL(), Token: "runner-b"}}),
		productFolder("odtr_third_rig_folder_token_rrrrrrrrrrrr", map[string]Upstream{"c": {URL: bad.URL(), Token: "runner-c"}}),
		productFolder("odtr_fourth_rig_folder_token_ssssssssss", map[string]Upstream{"d": {URL: ok.URL(), Token: ""}}),
	} {
		if err := tbl.AddFolder(f); err != nil {
			t.Fatal(err)
		}
	}
	s := newSessions(tbl, Options{ListTimeout: time.Second})
	for tok, want := range map[string]time.Duration{pTok: 0, p2Tok: retryFailedFirstUse,
		"odtr_third_rig_folder_token_rrrrrrrrrrrr": 0, "odtr_fourth_rig_folder_token_ssssssssss": retryFailedFirstUse} {
		if got := s.sessionTools("x", mcpserver.Principal{Hat: role.Product, Subject: tok}).RetryAfter; got != want {
			t.Errorf("%s: RetryAfter %v, want %v", tok[:20], got, want)
		}
	}
}

// B4 "fetch in parallel": the control plane and every executor are read at once, so a session start waits for the
// slowest part, not for their sum — with 4 hanging parts and a 400 ms bound it ends near 400 ms, not 1.6 s.
func TestRunnerCauses_EveryPartIsReadInParallel(t *testing.T) {
	h := newHarness(t, Options{ListTimeout: 400 * time.Millisecond})
	h.cp.setMode("hang")
	for _, e := range h.ex {
		e.setMode("hang")
	}
	h.tbl.Replace(h.table(func(f *Folder) {
		f.Upstreams = map[string]Upstream{"suta": {URL: h.ex["suta"].URL(), Token: "author-suta"},
			"sutb": {URL: h.ex["sutb"].URL(), Token: "author-sutb"}, "sutz": {URL: h.ex["sutz"].URL(), Token: "author-sutz"}}
	}))
	start := time.Now()
	res := h.initialize("s", testTok)
	if took := time.Since(start); took > 800*time.Millisecond {
		t.Fatalf("the session start took %s with 4 parts hanging and a 400 ms bound — the reads are not parallel", took)
	}
	text, _ := res["instructions"].(string)
	want := []string{textCPUnreachable, fmt.Sprintf(textExecUnreachableFmt, "suta"), fmt.Sprintf(textExecUnreachableFmt, "sutb"), fmt.Sprintf(textExecUnreachableFmt, "sutz")}
	if text != strings.Join(want, "\n") {
		t.Fatalf("instructions:\n got: %s\nwant: %s", text, strings.Join(want, "\n"))
	}
}

// R11: a read that panics ends as the server's "could not build" set — the router keeps serving every other session.
func TestRunnerCauses_APanickingReadDoesNotTakeTheRouterDown(t *testing.T) {
	boom := newStubExec(t, "0.3.73", "runner-a", "author-a")
	fine := newStubExec(t, "0.3.73", "runner-b", "author-b")
	orig := listTools
	t.Cleanup(func() { listTools = orig })
	listTools = func(c *mcp.Client) mcp.ListResult {
		if strings.HasPrefix(c.ServerURL, boom.ts.URL) {
			panic("a read went wrong")
		}
		return orig(c)
	}
	r := newRig(t, Options{},
		productFolder(pTok, map[string]Upstream{"a": {URL: boom.URL(), Token: "runner-a"}}),
		productFolder(p2Tok, map[string]Upstream{"b": {URL: fine.URL(), Token: "runner-b"}}))
	if res := r.initialize("s1", pTok); !strings.Contains(fmt.Sprint(res["instructions"]), "could not build this session's tool list") {
		t.Fatalf("the panicking session's start: %v", res["instructions"])
	}
	if res := r.initialize("s2", p2Tok); res["instructions"] != nil {
		t.Fatalf("another folder's session start after the panic: %v", res["instructions"])
	}
	if _, names := r.listed("s2", p2Tok); strings.Join(names, ",") != runnerNames {
		t.Fatalf("another folder's tools after the panic: %v", names)
	}
}

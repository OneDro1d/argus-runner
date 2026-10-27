package router

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// AC-D70 — the holdout (VR-R4, VR-P7) on the runner plane, and the run identity end to end (INT-037).
//
// The router shares one ToolSet between sessions whose lists are the same (R1). A product folder and a test folder on the
// SAME instance read the same runner list (the executor lists its runner tools to both hats), so when the test folder has
// no author tools — no control-plane record yet — the two share ONE set. That is safe only because no runner handler
// holds a folder's address or token: each call resolves its own folder again. These tests make the sharing happen and
// then check the one thing that matters: the product folder's calls carry the PRODUCT token, so the executor redacts.

func sharedRunnerFolders(t *testing.T, e *stubExec) (product, test Folder) {
	t.Helper()
	product = productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}})
	test = Folder{Path: "C:/work/test-rig", Hat: role.Test, Token: tTok, Upstreams: map[string]Upstream{"a": {URL: e.URL(), Token: "author-a"}}}
	return product, test
}

func TestRunnerHoldout_AProductAndATestFolderShareOneSetAndEachCarriesItsOwnToken(t *testing.T) {
	for _, testFirst := range []bool{true, false} {
		name := "product first"
		if testFirst {
			name = "test first"
		}
		t.Run(name, func(t *testing.T) {
			e := newStubExec(t, "0.3.73", "runner-a", "author-a")
			product, test := sharedRunnerFolders(t, e)
			r := newRig(t, Options{}, product, test)
			s := newSessions(r.tbl, Options{ListTimeout: time.Second})
			order := []mcpserver.Principal{{Hat: role.Product, Subject: pTok}, {Hat: role.Test, Subject: tTok}}
			if testFirst {
				order[0], order[1] = order[1], order[0]
			}
			x := s.sessionTools("1", order[0]).Tools
			y := s.sessionTools("2", order[1]).Tools
			if x == nil || x != y {
				t.Fatal("precondition: the product folder and the test folder (no author tools) do not share one ToolSet — the test proves nothing")
			}
			// Now through the real server, building in the same order.
			if testFirst {
				r.initialize("t", tTok)
				r.initialize("p", pTok)
			} else {
				r.initialize("p", pTok)
				r.initialize("t", tTok)
			}
			pRaw := r.call("p", pTok, 3, "runner_get_report", map[string]any{"instance_id": "a"})
			if got := e.lastCall().Bearer; got != "Bearer runner-a" {
				t.Fatalf("the product folder's call carried %q — want the product executor token", got)
			}
			if bytes.Contains(pRaw, []byte(sentinelExpected)) {
				t.Fatalf("SECURITY: the product folder's report carries the expected value: %s", pRaw)
			}
			tRaw := r.call("t", tTok, 4, "runner_get_report", map[string]any{"instance_id": "a"})
			if got := e.lastCall().Bearer; got != "Bearer author-a" {
				t.Fatalf("the test folder's call carried %q — want the test executor token", got)
			}
			if !bytes.Contains(tRaw, []byte(sentinelExpected)) {
				t.Fatalf("positive control: the test folder's report lost its expected value: %s", tRaw)
			}
		})
	}
}

// The set holds no address and no token, so a reload that moves an instance to another executor and rotates its token
// takes effect on the next call, with no reconnect.
func TestRunnerHoldout_AReloadedAddressAndTokenAreUsedWithoutAReconnect(t *testing.T) {
	e := newStubExec(t, "0.3.73", "runner-a", "author-a")
	moved := newStubExec(t, "0.3.73", "runner-a2", "author-a2")
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	r.replace(productFolder(pTok, map[string]Upstream{"a": {URL: moved.URL(), Token: "runner-a2"}}))
	if raw := r.call("s", pTok, 5, "runner_get_report", map[string]any{"instance_id": "a"}); bytes.Contains(raw, []byte(`"isError":true`)) {
		t.Fatalf("after the reload: %s", raw)
	}
	if got := moved.lastCall(); got.Bearer != "Bearer runner-a2" || e.count("tools/call") != 0 {
		t.Fatalf("the call went to %+v (old executor %d calls) — want the new address with the new token", got, e.count("tools/call"))
	}
}

// ── INT-037 / B7 16 / A9 #10: the run identity, end to end ───────────────────────────────────────────────────────

// runner_run through the router is the EXECUTOR's own async run: its dispatcher mints the id (recorded by RunBeginner),
// the router hands that id back unchanged, and runner_get_report with it reaches the same executor and returns THAT run's
// report — not the last run's, which is planted as a decoy.
func TestRunnerHoldout_ARunThroughTheRouterIsTheExecutorsOwnEndToEnd(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "listed at session start"
		if late {
			name = "an instance onboarded after the session started"
		}
		t.Run(name, func(t *testing.T) {
			e := newStubExec(t, "0.3.73", "runner-a", "author-a")
			other := newStubExec(t, "0.3.73", "runner-o", "author-o")
			testF := func(ups map[string]Upstream) Folder {
				return Folder{Path: "C:/work/test-rig", Hat: role.Test, Token: tTok, Upstreams: ups}
			}
			if late {
				r := newRig(t, Options{}, testF(map[string]Upstream{"o": {URL: other.URL(), Token: "author-o"}}))
				r.initialize("s", tTok)
				r.replace(testF(map[string]Upstream{"o": {URL: other.URL(), Token: "author-o"}, "a": {URL: e.URL(), Token: "author-a"}}))
				runEndToEnd(t, r, e)
				return
			}
			r := newRig(t, Options{}, testF(map[string]Upstream{"a": {URL: e.URL(), Token: "author-a"}}))
			r.initialize("s", tTok)
			runEndToEnd(t, r, e)
		})
	}
}

func runEndToEnd(t *testing.T, r *rig, e *stubExec) {
	t.Helper()
	raw := r.call("s", tTok, 6, "runner_run", map[string]any{"instance_id": "a"})
	e.mu.Lock()
	minted := append([]string(nil), e.minted...)
	e.mu.Unlock()
	if len(minted) != 1 {
		t.Fatalf("the executor minted %d run ids (want 1): %s", len(minted), raw)
	}
	id := minted[0]
	var res mcpserver.ToolResult
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Result, &res) != nil || len(res.Content) != 1 {
		t.Fatalf("runner_run answered %s", raw)
	}
	var started map[string]any
	_ = json.Unmarshal([]byte(res.Content[0].Text), &started)
	if started["status"] != "running" || started["run_id"] != id {
		t.Fatalf("the router handed back %v — the executor minted %q (INT-037: the router must never mint its own)", started, id)
	}
	if !waitUntil(2*time.Second, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.ran) == 1 && e.ran[0] == id
	}) {
		t.Fatalf("the executor's run tool did not receive run %q", id)
	}
	// The executor writes the run's own report; report.json is a decoy — the last run, another id.
	writeReport(t, filepath.Join(e.env.ResultsRoot, "local", "runs", id+".json"), id)
	rep := r.call("s", tTok, 7, "runner_get_report", map[string]any{"instance_id": "a", "run_id": id})
	if got := e.lastCall(); got.Tool != "runner_get_report" || !bytes.Contains(got.Args, []byte(`"run_id":"`+id+`"`)) {
		t.Fatalf("get_report reached the executor as %+v %s", got, got.Args)
	}
	if !bytes.Contains(rep, []byte(`\"run_id\":\"`+id+`\"`)) || bytes.Contains(rep, []byte(`last-run`)) {
		t.Fatalf("runner_get_report(%s) did not return that run's report: %s", id, rep)
	}
	// An out-of-folder run is refused synchronously, before any executor hears of it.
	calls := e.count("tools/call")
	if got := toolErrorText(t, r.call("s", tTok, 8, "runner_run", map[string]any{"instance_id": "elsewhere"})); !strings.Contains(got, "not onboarded into this folder") || e.count("tools/call") != calls {
		t.Fatalf("an out-of-folder run: %q", got)
	}
}

// INT-037's guard on every tool the router can dispatch: a built set's tools, and the tool UnknownTool hands back for an
// instance onboarded mid-session. None is Async — Async would make the server mint its own run id and run the router's
// checks in the background where nobody sees their refusal.
func TestRunnerHoldout_NoRouterToolIsAsync(t *testing.T) {
	h := newHarness(t, Options{})
	s := newSessions(h.tbl, Options{ListTimeout: time.Second})
	for _, prin := range []mcpserver.Principal{{Hat: role.Test, Subject: testTok}, {Hat: role.Product, Subject: prodTok}, {Hat: role.Test, Subject: otherTok}} {
		set := s.sessionTools("x", prin)
		if len(set.Tools.Tools()) == 0 {
			t.Fatalf("%v: an empty set — the guard would check nothing", prin.Hat)
		}
		for _, tl := range set.Tools.Tools() {
			if tl.Async || tl.PreCheck != nil {
				t.Fatalf("%v: %s is Async=%v PreCheck=%v — the router must never background a call (INT-037)", prin.Hat, tl.Name, tl.Async, tl.PreCheck != nil)
			}
		}
	}
	// The session starts BEFORE the late instance is onboarded, so its tool comes through UnknownTool.
	set := s.sessionTools("y", mcpserver.Principal{Hat: role.Test, Subject: testTok})
	late := newStubExec(t, "0.3.73", "runner-l", "author-l")
	late.serve("0.3.73", "runner-l", "author-l", append(late.newTools(), execTool("runner_lateonly", "d")))
	h.tbl.Replace(h.table(func(f *Folder) {
		f.Upstreams = map[string]Upstream{"suta": {URL: h.ex["suta"].URL(), Token: "author-suta"}, "late": {URL: late.URL(), Token: "author-l"}}
	}))
	tool, out := s.unknownTool("y", "runner_lateonly", json.RawMessage(`{"instance_id":"late"}`), mcpserver.Principal{Hat: role.Test, Subject: testTok}, set.State)
	if out != nil || tool == nil {
		t.Fatalf("precondition: UnknownTool did not hand back a tool for the late instance (%+v)", out)
	}
	if tool.Async || tool.PreCheck != nil || tool.Namespace != mcpserver.NSRunner {
		t.Fatalf("the late instance's tool: Async=%v PreCheck=%v Namespace=%q", tool.Async, tool.PreCheck != nil, tool.Namespace)
	}
}

// ── B2 / A7: the router reports its own version ─────────────────────────────────────────────────────────────────

func TestRunnerHoldout_TheRouterReportsItsVersion(t *testing.T) {
	for _, c := range []struct{ set, want string }{{"0.3.73", "0.3.73"}, {"", "M2.5"}} {
		e := newStubExec(t, "0.3.73", "runner-a", "author-a")
		r2 := newRig(t, Options{Version: c.set}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
		res := r2.initialize("v", pTok)
		info, _ := res["serverInfo"].(map[string]any)
		if info["version"] != c.want {
			t.Fatalf("Options.Version %q: serverInfo %v, want version %q", c.set, info, c.want)
		}
	}
}

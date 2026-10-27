package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// AC-D70 (design #678 B4 call path steps 1-8, A5, B7 5, 6, 7, 8, 12, 14, 15): what happens on a runner CALL.

// ── B4 step 4 / B7 5, 14: the folder rules, before any request to an executor ───────────────────────────────────────

func TestRunnerCall_TheFolderRulesRefuseBeforeAnyRequest(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	lists := func() int {
		return h.ex["suta"].count("tools/list") + h.ex["sutb"].count("tools/list") + h.ex["sutz"].count("tools/list")
	}
	beforeLists, beforeCalls := lists(), h.execToolCalls()
	for _, name := range []string{"runner_get_report", "runner__get_report"} {
		for c, args := range map[string]string{
			"another folder's instance (VR-R5)":           `{"instance_id":"sutz"}`,
			"a joined list (VR-R13)":                      `{"instance_id":"suta,sutb"}`,
			"a key that folds to instance_id":             `{"INSTANCE_ID":"sutz"}`,
			"a folded key even for an in-folder instance": `{"Instance_Id":"suta"}`,
			"a long-s key Go's decoder folds":             `{"inſtance_id":"sutz"}`,
			"instance_id twice":                           `{"instance_id":"suta","instance_id":"sutz"}`,
			"instance_id twice with the same value":       `{"instance_id":"suta","instance_id":"suta"}`,
		} {
			if got := toolErrorText(t, h.call("s", testTok, 6, name, json.RawMessage(args))); !strings.HasPrefix(got, "router:") {
				t.Errorf("%s %s: not refused by the router: %q", name, c, got)
			}
		}
	}
	if lists() != beforeLists || h.execToolCalls() != beforeCalls {
		t.Fatalf("refused calls reached an executor: +%d tools/list, +%d tools/call", lists()-beforeLists, h.execToolCalls()-beforeCalls)
	}
}

// R1: a runner call whose instance_id is missing, empty or not a string — or whose arguments are not an object — gets
// EXACTLY the executor's own answer (the same bad call, the same answer: A9 #9), and nothing is forwarded.
func TestRunnerCall_NoInstanceIDGetsTheExecutorsOwnAnswer(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	before := h.execToolCalls()
	for c, args := range map[string]json.RawMessage{
		"no instance_id": json.RawMessage(`{}`), "empty": json.RawMessage(`{"instance_id":""}`),
		"a number": json.RawMessage(`{"instance_id":5}`), "null": json.RawMessage(`{"instance_id":null}`),
		"not an object": json.RawMessage(`[]`), "absent arguments": nil,
	} {
		for _, name := range []string{"runner_get_report", "runner_run"} {
			params := map[string]any{"name": name}
			if args != nil {
				params["arguments"] = args
			}
			routed, _ := h.srv.Dispatch("s", testTok, rpcReq(7, "tools/call", params))
			direct := h.ex["suta"].direct(t, "author-suta", 7, "tools/call", params)
			if string(routed) != string(direct) || !bytes.Contains(routed, []byte(`instance_id is required`)) {
				t.Errorf("%s %s:\nrouter:   %s\nexecutor: %s", name, c, routed, direct)
			}
		}
	}
	if n := h.execToolCalls() - before; n != 0 {
		t.Fatalf("%d of these calls were forwarded", n)
	}
}

// ── B7 6: instance_id becomes "local" on a runner call; every other argument reaches the executor as sent ────────────

func TestRunnerCall_InstanceIDBecomesLocalAndTheOtherArgumentsAreKept(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	h.call("s", testTok, 8, "runner_get_sagas", json.RawMessage(`{ "instance_id" : "suta", "correlation_id":"tr-suta-x", "window":"15m" }`))
	got := h.ex["suta"].lastCall().Args
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("the executor received %s", got)
	}
	if m["instance_id"] != InEnvInstanceID || m["correlation_id"] != "tr-suta-x" || m["window"] != "15m" {
		t.Fatalf("the executor received %s — want instance_id %q and every other value as sent", got, InEnvInstanceID)
	}
	// The order and spelling of the other keys are kept (no decode into Go values): only the client's compaction applies.
	if want := `{"instance_id":"local","correlation_id":"tr-suta-x","window":"15m"}`; string(got) != want {
		t.Fatalf("the executor received\n %s\nwant\n %s", got, want)
	}
	// The author plane keeps the real id (part 1's rule, unchanged).
	h.call("s", testTok, 9, "author_list_runs", map[string]any{"instance_id": "suta"})
	if got := h.cp.lastCall().Args; !bytes.Contains(got, []byte(`"instance_id":"suta"`)) {
		t.Fatalf("the control plane received %s", got)
	}
}

func TestLocaliseRaw(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"instance_id":"orderservice-compose","layer":"http-ingestion"}`, `{"instance_id":"local","layer":"http-ingestion"}`},
		{`{"layer":"x","instance_id":"a"}`, `{"layer":"x","instance_id":"local"}`},
		{` { "a" : 1.50 , "instance_id" : "s" , "n" : 12345678901234567890 } `, `{ "a" : 1.50 , "instance_id" : "local" , "n" : 12345678901234567890 }`},
		{`{"x":{"instance_id":"nested"},"instance_id":"top"}`, `{"x":{"instance_id":"nested"},"instance_id":"local"}`},
		{`{"t":"a\u003cb","instance_id":"\u0073uta"}`, `{"t":"a\u003cb","instance_id":"local"}`},
	} {
		if got := string(localiseRaw(json.RawMessage(c.in))); got != c.want {
			t.Errorf("localiseRaw(%s)\n got %s\nwant %s", c.in, got, c.want)
		}
	}
	for _, in := range []string{`[]`, `"x"`, `null`, `{"run_id":"r1"}`} {
		if got := string(localiseRaw(json.RawMessage(in))); got != in {
			t.Errorf("localiseRaw(%s) = %s, want it unchanged", in, got)
		}
	}
}

// ── B7 15 / decision 14 / A9 #9 P2: the executor's answers, unchanged ──────────────────────────────────────────────

func TestRunnerCall_AnswersComeBackUnchanged(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"runner_get_report", map[string]any{"instance_id": "suta"}},                                      // a result, the report
		{"runner_get_report", map[string]any{"instance_id": "suta", "run_id": "no-such-run"}},             // a result: not found
		{"runner_get_sagas", map[string]any{"instance_id": "suta", "correlation_id": "not-a-minted-one"}}, // a tool error
		{"runner_get_report", map[string]any{"instance_id": "suta", "junk": "x"}},                         // the closed schema's refusal
		{"runner_run", map[string]any{"instance_id": "suta", "run_id": "mine"}},                           // the run tool refusing a run_id
		{"runner_get_dashboard_url", map[string]any{"instance_id": "suta", "correlation_id": "tr-1"}},     // a templated answer
	} {
		routed := h.call("s", testTok, 9, c.name, c.args)
		local := map[string]any{}
		for k, v := range c.args {
			local[k] = v
		}
		local["instance_id"] = InEnvInstanceID
		direct := h.ex["suta"].direct(t, "author-suta", 9, "tools/call", map[string]any{"name": c.name, "arguments": local})
		if string(routed) != string(direct) {
			t.Errorf("%s %v: answers differ\nrouter:   %s\nexecutor: %s", c.name, c.args, routed, direct)
		}
	}
}

// A JSON-RPC error from the executor is the executor's own error, not a tool error with extra fields (decision 14) —
// here the real "run already in progress" refusal. It is not "could not reach", and nothing is read again.
func TestRunnerCall_AnExecutorsJSONRPCErrorIsPassedBackAsItsOwn(t *testing.T) {
	h := newHarness(t, Options{})
	release := h.ex["suta"].holdRuns()
	defer release()
	h.initialize("s", testTok)
	first := h.call("s", testTok, 10, "runner_run", map[string]any{"instance_id": "suta"})
	if !bytes.Contains(first, []byte(`\"status\":\"running\"`)) {
		t.Fatalf("the first run did not start: %s", first)
	}
	lists := h.ex["suta"].count("tools/list")
	second := h.call("s", testTok, 11, "runner_run", map[string]any{"instance_id": "suta"})
	if want := `{"jsonrpc":"2.0","id":11,"error":{"code":-32002,"message":"run already in progress on this instance"}}`; string(second) != want {
		t.Fatalf("\n got: %s\nwant: %s", second, want)
	}
	if h.ex["suta"].count("tools/list") != lists {
		t.Fatal("a JSON-RPC error other than -32601 made the router read the list again")
	}
}

// ── A5 / B7 7: the target instance's own offer, both refusal texts ─────────────────────────────────────────────────

func TestRunnerCall_EachCallIsCheckedAgainstTheTargetsOwnDefinition(t *testing.T) {
	a, b := twoVersions(t)
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}, "b": {URL: b.URL(), Token: "runner-b"}}))
	r.initialize("s", pTok)
	// The listed runner_x is b's (p2). A call to a with p2 reaches a, and a's OWN refusal comes back unchanged — the router
	// does not refuse by the newest definition, it leaves the target's check to the target.
	routed := r.call("s", pTok, 12, "runner_x", map[string]any{"instance_id": "a", "p2": "v"})
	direct := a.direct(t, "runner-a", 12, "tools/call", map[string]any{"name": "runner_x", "arguments": map[string]any{"instance_id": "local", "p2": "v"}})
	if string(routed) != string(direct) || !bytes.Contains(routed, []byte("p2")) {
		t.Fatalf("p2 to a:\nrouter: %s\na:      %s", routed, direct)
	}
	// An argument only a's OWN definition has is accepted by a — an older executor is never refused a call it accepts.
	if raw := r.call("s", pTok, 13, "runner_x", map[string]any{"instance_id": "a", "p0": "v"}); bytes.Contains(raw, []byte(`"isError":true`)) {
		t.Fatalf("p0 to a was refused: %s", raw)
	}
	if got := a.lastCall(); got.Tool != "runner_x" || !bytes.Contains(got.Args, []byte(`"p0":"v"`)) {
		t.Fatalf("a received %+v", got)
	}
	// b accepts p2.
	if raw := r.call("s", pTok, 14, "runner_x", map[string]any{"instance_id": "b", "p2": "v"}); bytes.Contains(raw, []byte(`"isError":true`)) {
		t.Fatalf("p2 to b was refused: %s", raw)
	}
	// A tool only the NEWER executor offers, on the older instance: "update that instance's executor".
	callsA := a.count("tools/call")
	if got := toolErrorText(t, r.call("s", pTok, 15, "runner_onlyb", map[string]any{"instance_id": "a"})); got != fmt.Sprintf(textNotOfferedUpdateFmt, "runner_onlyb", "a") {
		t.Fatalf("runner_onlyb on a: %q", got)
	}
	// A tool only the OLDER executor still offers, on the newer instance: "only older executors still offer it".
	if got := toolErrorText(t, r.call("s", pTok, 16, "runner_onlya", map[string]any{"instance_id": "b"})); got != fmt.Sprintf(textNotOfferedOlderFmt, "runner_onlya", "b") {
		t.Fatalf("runner_onlya on b: %q", got)
	}
	if a.count("tools/call") != callsA || b.lastCall().Tool == "runner_onlya" {
		t.Fatal("an A5 refusal forwarded the call")
	}
}

// R19: the read made before an A5 refusal exists only to avoid a WRONG refusal (the target may have been updated). When
// it brings back no usable list — the executor stopped, or it now lists a tool that is not a runner tool — it changes
// nothing the session knew: the A5 text from the session-start read, never a session-start sentence ("… when it read the
// tools for this session … reconnect"), which is false while the target's tools ARE listed in this session.
func TestRunnerCall_AReReadThatIsNotAUsableListChangesNothing(t *testing.T) {
	for name, spoil := range map[string]func(a *stubExec){
		"the target stopped":          func(a *stubExec) { a.setMode("drop") },
		"the target answers HTTP 503": func(a *stubExec) { a.setMode("status:503") },
		"the target lists an author tool": func(a *stubExec) {
			a.setRawList(`{"tools":[{"name":"author_x","description":"d","inputSchema":{"type":"object"}}]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := twoVersions(t)
			r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}, "b": {URL: b.URL(), Token: "runner-b"}}))
			r.initialize("s", pTok)
			spoil(a)
			inits, calls := a.count("initialize"), a.count("tools/call")
			if got := toolErrorText(t, r.call("s", pTok, 33, "runner_onlyb", map[string]any{"instance_id": "a"})); got != fmt.Sprintf(textNotOfferedUpdateFmt, "runner_onlyb", "a") {
				t.Fatalf("got %q — want the A5 text from the session-start read", got)
			}
			if a.count("initialize") == inits || a.count("tools/call") != calls {
				t.Fatalf("+%d initialize, +%d tools/call — want the read before refusing, and no call", a.count("initialize")-inits, a.count("tools/call")-calls)
			}
		})
	}
}

func TestRunnerCall_TheA5TextFollowsTheVersionOrder(t *testing.T) {
	inst := func(id, v string) *runnerInstance { return &runnerInstance{id: id, version: v} }
	update, older := fmt.Sprintf(textNotOfferedUpdateFmt, "runner_x", "t"), fmt.Sprintf(textNotOfferedOlderFmt, "runner_x", "t")
	for _, c := range []struct {
		target   string
		offerers []string
		want     string
	}{
		{"0.3.72", []string{"0.3.72"}, update},           // equal: never "older"
		{"0.3.72", []string{"0.3.71"}, older},            // every offerer older
		{"0.3.72", []string{"0.3.71", "0.3.73"}, update}, // one newer
		{"0.3.72", []string{"M2.5"}, older},              // M2.5 is older than any real version
		{"0.3.72", []string{"0.0.0-src+x"}, update},      // AC-D70.2: a developer's own build is NEWER than any release
		{"0.0.0-dev", []string{"0.3.72"}, older},         // … so a release is older than it
		{"0.0.0-dev", []string{"0.0.0-src+y"}, update},   // two developer builds: equal
		{"M2.5", []string{"0.0.0-dev"}, update},          // M2.5 stays the oldest
		{"0.0.0-dev", []string{"M2.5"}, older},           //
		{"M2.5", []string{"0.3.71"}, update},             // the target is the oldest
		{"M2.5", []string{"M2.5"}, update},               // two unreadable versions: equal
		{"0.3.10", []string{"0.3.9"}, older},             // numbers, not text
		{"0.3.72", []string{"0.3.72-dev+x"}, older},      // a suffixed build is below its release
	} {
		var offs []*runnerInstance
		for i, v := range c.offerers {
			offs = append(offs, inst(fmt.Sprint("o", i), v))
		}
		if got := notOfferedText("runner_x", inst("t", c.target), offs); got != c.want {
			t.Errorf("target %s, offerers %v:\n got %q\nwant %q", c.target, c.offerers, got, c.want)
		}
	}
}

// R6: an instance updated while the session is open is not told to update — the router reads its list again before an
// A5 refusal, and forwards when it offers the tool now.
func TestRunnerCall_AnExecutorUpdatedMidSessionIsForwarded(t *testing.T) {
	a, b := twoVersions(t)
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}, "b": {URL: b.URL(), Token: "runner-b"}}))
	r.initialize("s", pTok)
	a.serve("0.3.73", "runner-a", "author-a", []mcpserver.Tool{execTool("runner_x", "A's x", "p2"), execTool("runner_onlyb", "now A too")})
	raw := r.call("s", pTok, 17, "runner_onlyb", map[string]any{"instance_id": "a"})
	if bytes.Contains(raw, []byte(`"isError":true`)) || a.lastCall().Tool != "runner_onlyb" {
		t.Fatalf("the updated instance was refused: %s", raw)
	}
}

// ── B7 8 / D-8: an instance onboarded after the session started is callable at once ──────────────────────────────────

func TestRunnerCall_AnInstanceOnboardedAfterTheSessionStartedIsCallableAtOnce(t *testing.T) {
	a := newStubExec(t, "0.3.73", "runner-a", "author-a")
	late := newStubExec(t, "0.3.73", "runner-l", "author-l")
	late.serve("0.3.73", "runner-l", "author-l", append(late.newTools(), execTool("runner_lateonly", "only the late one")))
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	r.replace(productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}, "late": {URL: late.URL(), Token: "runner-l"}}))

	// A name the session lists: forwarded to the new instance's executor, with the folder's token for it.
	if raw := r.call("s", pTok, 18, "runner_get_report", map[string]any{"instance_id": "late"}); bytes.Contains(raw, []byte(`"isError":true`)) {
		t.Fatalf("the late instance was refused: %s", raw)
	}
	if got := late.lastCall(); got.Tool != "runner_get_report" || got.Bearer != "Bearer runner-l" {
		t.Fatalf("late received %+v", got)
	}
	// A name ONLY the new instance offers (not in the session's list): read ONCE, then forwarded.
	lists := late.count("tools/list")
	if raw := r.call("s", pTok, 19, "runner_lateonly", map[string]any{"instance_id": "late"}); bytes.Contains(raw, []byte(`"isError":true`)) || bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("the late-only tool: %s", raw)
	}
	if n := late.count("tools/list") - lists; n != 1 {
		t.Fatalf("the late instance's list was read %d times for one call, want 1", n)
	}
	// Not on the instance the session started with: the instance onboarded since offers it, so B4 step 2's A5 text is
	// true (R20) — after a read of a once more (R6), with no call to a.
	aLists, aCalls := a.count("tools/list"), a.count("tools/call")
	if got := toolErrorText(t, r.call("s", pTok, 20, "runner_lateonly", map[string]any{"instance_id": "a"})); got != fmt.Sprintf(textNotOfferedUpdateFmt, "runner_lateonly", "a") {
		t.Fatalf("the late-only tool on a: %q", got)
	}
	if a.count("tools/list")-aLists != 1 || a.count("tools/call") != aCalls {
		t.Fatalf("a: +%d tools/list, +%d tools/call — want 1 read and no call", a.count("tools/list")-aLists, a.count("tools/call")-aCalls)
	}
	// The run tool through this path is the executor's own async run: its id, not one the router minted.
	raw := r.call("s", pTok, 21, "runner_run", map[string]any{"instance_id": "late"})
	late.mu.Lock()
	minted := append([]string(nil), late.minted...)
	late.mu.Unlock()
	if len(minted) != 1 || !bytes.Contains(raw, []byte(minted[0])) {
		t.Fatalf("runner_run on the late instance: %s (the executor minted %v)", raw, minted)
	}
}

// R20: a name the session does not list, on an instance usable at session start. The instance offers it now (updated
// while the session is open) → forwarded under the name it lists; no instance in the folder offers it → the executor's
// own "unknown tool" (D-11), with no call made.
func TestRunnerCall_AnUnlistedNameIsForwardedWhenTheTargetOffersItNow(t *testing.T) {
	a := newStubExec(t, "0.3.73", "runner-a", "author-a")
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	if raw := r.call("s", pTok, 34, "runner_nobody", map[string]any{"instance_id": "a"}); !bytes.Contains(raw, []byte(`"code":-32601`)) || a.count("tools/call") != 0 {
		t.Fatalf("a name no instance offers: %s (%d calls)", raw, a.count("tools/call"))
	}
	a.serve("0.3.74", "runner-a", "author-a", append(a.newTools(), execTool("runner_new", "added by the update")))
	raw := r.call("s", pTok, 35, "runner__new", map[string]any{"instance_id": "a"})
	if bytes.Contains(raw, []byte(`"isError":true`)) || bytes.Contains(raw, []byte(`"error"`)) {
		t.Fatalf("the tool the updated target offers now: %s", raw)
	}
	if got := a.lastCall(); got.Tool != "runner_new" || !bytes.Contains(got.Args, []byte(`"instance_id":"local"`)) {
		t.Fatalf("a received %+v — want runner_new with instance_id local", got)
	}
}

// The late instance's read follows every rule a session start does: decision 13, and a tool it does not offer.
func TestRunnerCall_ALateInstanceIsReadByTheSameRules(t *testing.T) {
	a := newStubExec(t, "0.3.73", "runner-a", "author-a")
	bad := newStubExec(t, "0.3.73", "runner-bad", "author-bad")
	bad.setRawList(`{"tools":[{"name":"author_x","description":"d","inputSchema":{"type":"object"}}]}`)
	r := newRig(t, Options{}, productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	r.replace(productFolder(pTok, map[string]Upstream{"a": {URL: a.URL(), Token: "runner-a"}, "bad": {URL: bad.URL(), Token: "runner-bad"}}))
	got := toolErrorText(t, r.call("s", pTok, 22, "runner_get_report", map[string]any{"instance_id": "bad"}))
	if !strings.HasPrefix(got, "The executor of instance bad listed a tool that is not a runner tool") || bad.count("tools/call") != 0 {
		t.Fatalf("a late instance breaching decision 13: %q (%d calls)", got, bad.count("tools/call"))
	}
}

// ── B7 12 / B4 step 7: a stale name is listed again and retried ONCE ────────────────────────────────────────────────

func TestRunnerCall_AStaleNameIsReadAgainAndRetriedOnce(t *testing.T) {
	t.Run("a rollback during the session", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		ex := h.ex["suta"]
		ex.serve("", "runner-suta", "author-suta", ex.oldTools()) // rolled back: runner__X only, no aliases
		lists, calls := ex.count("tools/list"), ex.count("tools/call")
		raw := h.call("s", testTok, 23, "runner_get_report", map[string]any{"instance_id": "suta"})
		if bytes.Contains(raw, []byte(`"isError":true`)) || !bytes.Contains(raw, []byte(sentinelExpected)) {
			t.Fatalf("the call after the rollback: %s", raw)
		}
		if ex.count("tools/list")-lists != 1 || ex.count("tools/call")-calls != 2 {
			t.Fatalf("+%d tools/list, +%d tools/call — want exactly 1 read again and 2 calls", ex.count("tools/list")-lists, ex.count("tools/call")-calls)
		}
		tc := ex.toolCalls()
		if tc[len(tc)-2].Tool != "runner_get_report" || tc[len(tc)-1].Tool != "runner__get_report" {
			t.Fatalf("the two calls were %q then %q", tc[len(tc)-2].Tool, tc[len(tc)-1].Tool)
		}
	})
	t.Run("two stale calls at once each retry once", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		ex := h.ex["suta"]
		ex.serve("", "runner-suta", "author-suta", ex.oldTools())
		lists, calls := ex.count("tools/list"), ex.count("tools/call")
		var wg sync.WaitGroup
		out := make([][]byte, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				out[i], _ = h.srv.Dispatch("s", testTok, rpcReq(24+i, "tools/call", map[string]any{"name": "runner_get_report", "arguments": map[string]any{"instance_id": "suta"}}))
			}(i)
		}
		wg.Wait()
		for i, raw := range out {
			if !bytes.Contains(raw, []byte(sentinelExpected)) {
				t.Fatalf("call %d: %s", i, raw)
			}
		}
		if ex.count("tools/list")-lists != 2 || ex.count("tools/call")-calls != 4 {
			t.Fatalf("+%d tools/list, +%d tools/call — want 2 and 4", ex.count("tools/list")-lists, ex.count("tools/call")-calls)
		}
	})
	t.Run("an executor that lists the name and still does not know it", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		ex := h.ex["suta"]
		ex.setMode("unknowntool")
		lists, calls := ex.count("tools/list"), ex.count("tools/call")
		raw := h.call("s", testTok, 26, "runner_get_report", map[string]any{"instance_id": "suta"})
		if want := `{"jsonrpc":"2.0","id":26,"error":{"code":-32601,"message":"unknown tool runner_get_report"}}`; string(raw) != want {
			t.Fatalf("\n got: %s\nwant: %s (the executor's own answer, passed back)", raw, want)
		}
		if ex.count("tools/list")-lists != 1 || ex.count("tools/call")-calls != 1 {
			t.Fatalf("+%d tools/list, +%d tools/call — want 1 and 1: no retry under the same name", ex.count("tools/list")-lists, ex.count("tools/call")-calls)
		}
	})
	t.Run("a re-read that breaches decision 13", func(t *testing.T) {
		// R19: the re-read is not a usable list, so it is not used — no retry under any name it lists, and the executor's
		// own answer stands (a session-start sentence would be false here: suta's tools ARE listed in this session).
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		ex := h.ex["suta"]
		ex.serve("", "runner-suta", "author-suta", append(ex.oldTools(), execTool("author_x", "d")))
		calls := ex.count("tools/call")
		raw := h.call("s", testTok, 27, "runner_get_report", map[string]any{"instance_id": "suta"})
		direct := ex.direct(t, "author-suta", 27, "tools/call", map[string]any{"name": "runner_get_report", "arguments": map[string]any{"instance_id": "local"}})
		if string(raw) != string(direct) || !bytes.Contains(raw, []byte(`"code":-32601`)) || ex.count("tools/call")-calls != 1 {
			t.Fatalf("+%d calls\nrouter:   %s\nexecutor: %s — want the executor's own -32601 after 1 call", ex.count("tools/call")-calls, raw, direct)
		}
		h.mu.Lock()
		logs := strings.Join(h.logs, "\n")
		h.mu.Unlock()
		if !strings.Contains(logs, "decision 13") {
			t.Fatalf("the breach is not logged: %q", logs)
		}
	})
	t.Run("a re-read that no longer offers the tool, while another instance does", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.initialize("s", testTok)
		ex := h.ex["suta"]
		ex.serve("0.3.70", "runner-suta", "author-suta", []mcpserver.Tool{execTool("runner_other", "d")})
		got := toolErrorText(t, h.call("s", testTok, 28, "runner_get_report", map[string]any{"instance_id": "suta"}))
		if got != fmt.Sprintf(textNotOfferedUpdateFmt, "runner_get_report", "suta") {
			t.Fatalf("got %q", got)
		}
	})
}

// ── TS-R6: two instances of one folder, each at its own executor with its own token ─────────────────────────────────

func TestRunnerCall_TwoInstancesOfOneFolderEachReachTheirOwnExecutor(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	for _, id := range []string{"suta", "sutb"} {
		if raw := h.call("s", testTok, 29, "runner_get_report", map[string]any{"instance_id": id}); bytes.Contains(raw, []byte(`"isError":true`)) {
			t.Fatalf("%s: %s", id, raw) // each stub accepts ONLY its own tokens, so a crossed token is a 401 here
		}
	}
	for _, id := range []string{"suta", "sutb"} {
		if got := h.ex[id].toolCalls(); len(got) != 1 || got[0].Bearer != "Bearer author-"+id {
			t.Fatalf("%s's executor received %+v — want one call, with its own token", id, got)
		}
	}
}

// ── VR-R1: the vantage rewrite on every executor request ──────────────────────────────────────────────────────────

func TestRunnerCall_TheVantageRewriteAppliesToEveryExecutorRequest(t *testing.T) {
	ex := newStubExec(t, "0.3.73", "runner-a", "author-a")
	late := newStubExec(t, "0.3.73", "runner-l", "author-l")
	loop := func(e *stubExec) string {
		return "http://127.0.0.1:" + e.ts.URL[strings.LastIndex(e.ts.URL, ":")+1:] + "/mcp"
	}
	r := newRig(t, Options{HostAlias: "localhost"}, productFolder(pTok, map[string]Upstream{"a": {URL: loop(ex), Token: "runner-a"}}))
	r.initialize("s", pTok)                                                        // the list at session start
	r.call("s", pTok, 30, "runner_get_report", map[string]any{"instance_id": "a"}) // a call
	ex.serve("", "runner-a", "author-a", ex.oldTools())                            // rolled back:
	r.call("s", pTok, 31, "runner_get_report", map[string]any{"instance_id": "a"}) // a call, a re-read, a retry
	r.replace(productFolder(pTok, map[string]Upstream{"a": {URL: loop(ex), Token: "runner-a"}, "late": {URL: loop(late), Token: "runner-l"}}))
	r.call("s", pTok, 32, "runner_get_report", map[string]any{"instance_id": "late"}) // a late instance's read + call
	for _, e := range []*stubExec{ex, late} {
		e.mu.Lock()
		calls := append([]stubCall(nil), e.calls...)
		e.mu.Unlock()
		if len(calls) == 0 {
			t.Fatal("an executor received no request")
		}
		for _, c := range calls {
			if !strings.HasPrefix(c.Host, "localhost:") {
				t.Fatalf("a %s went to %q — the alias was not applied", c.Method, c.Host)
			}
		}
	}
}

// ── R18: a runner call is bounded by the router's own bound, not the MCP client's 30 s ─────────────────────────────

// The executor's synchronous part of runner_run and runner_validate_config can take about 65 s — the catalog fetch
// (toolcore catalogCallTimeout, 55 s) and then the control plane's BeginRun (10 s). Under the client's 30 s the agent was
// told "could not reach" while the executor went on and could start the run. A forwarded runner call now waits under the
// router's own bound, and only past it gets today's text.
func TestRunnerCall_TheCallTimeoutIsTheRoutersOwnBound(t *testing.T) {
	if DefaultRunnerCallTimeout < 120*time.Second {
		t.Fatalf("DefaultRunnerCallTimeout %s leaves no room above the executor's slowest synchronous answer (about 65 s)", DefaultRunnerCallTimeout)
	}
	if s := newSessions(New(), Options{}); s.opts.RunnerCallTimeout != DefaultRunnerCallTimeout {
		t.Fatalf("an unset RunnerCallTimeout became %s", s.opts.RunnerCallTimeout)
	}
	slowExec := func() *stubExec {
		e := newStubExec(t, "0.3.73", "runner-a", "author-a")
		e.serve("0.3.73", "runner-a", "author-a", []mcpserver.Tool{{Name: "runner_slow", Namespace: mcpserver.NSRunner,
			Description: "slow", InputSchema: map[string]any{"type": "object"},
			Handler: func(json.RawMessage, mcpserver.Principal) mcpserver.Outcome {
				time.Sleep(600 * time.Millisecond)
				return mcpserver.Ok("done")
			}}})
		return e
	}
	args := map[string]any{"instance_id": "a"}

	// Inside the bound: the executor's own answer.
	e := slowExec()
	r := newRig(t, Options{RunnerCallTimeout: 5 * time.Second}, productFolder(pTok, map[string]Upstream{"a": {URL: e.URL(), Token: "runner-a"}}))
	r.initialize("s", pTok)
	routed := r.call("s", pTok, 40, "runner_slow", args)
	if direct := e.direct(t, "runner-a", 40, "tools/call", map[string]any{"name": "runner_slow", "arguments": map[string]any{"instance_id": "local"}}); string(routed) != string(direct) {
		t.Fatalf("a slow call inside the bound\nrouter:   %s\nexecutor: %s", routed, direct)
	}

	// Past the bound: today's text, at the bound — not at the client's own 30 s.
	e2 := slowExec()
	r2 := newRig(t, Options{RunnerCallTimeout: 300 * time.Millisecond}, productFolder(pTok, map[string]Upstream{"a": {URL: e2.URL(), Token: "runner-a"}}))
	r2.initialize("s", pTok)
	start := time.Now()
	got := toolErrorText(t, r2.call("s", pTok, 41, "runner_slow", args))
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the call took %s with a 300 ms bound", took)
	}
	if got != fmt.Sprintf(textNoAnswerFmt, "the executor of instance a", kindTimedOut) {
		t.Fatalf("past the bound: %q", got)
	}
}

func TestRunnerCallTimeoutFromEnv(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
		warn bool
	}{{"", DefaultRunnerCallTimeout, false}, {"5m", 5 * time.Minute, false}, {"30s", 30 * time.Second, false},
		{"10s", MinRunnerCallTimeout, true}, {"0s", DefaultRunnerCallTimeout, true}, {"-1m", DefaultRunnerCallTimeout, true},
		{"slow", DefaultRunnerCallTimeout, true}, {"2h", MaxRunnerCallTimeout, true}} {
		t.Setenv(RunnerCallTimeoutEnv, c.env)
		got, warn := RunnerCallTimeoutFromEnv()
		if got != c.want || (warn != "") != c.warn || (warn != "" && !strings.Contains(warn, RunnerCallTimeoutEnv)) {
			t.Errorf("%q: got %v %q", c.env, got, warn)
		}
	}
}

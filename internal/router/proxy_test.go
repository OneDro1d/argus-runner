package router

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
)

// The transport half. These tests drive the REAL mcpserver — the same protocol code the in-env and
// cloud servers use — so the scope filtering under test is the one that will actually run, not a
// re-implementation of it.

type fakeFwd struct {
	lastTarget Target
	lastTool   string
	// lastArgs is RECORDED, and that is not a detail. It used to be discarded as `_`, which is
	// exactly why four separate JOIN defects reached production on 2026-08-09: every test here
	// asserted that the right TARGET was chosen, and none could see what payload the upstream would
	// actually receive. A stub that echoes anything answers every question except the one that
	// matters.
	lastArgs json.RawMessage
	err      error
	// isErr is the UPSTREAM's result.isError — the tool was reached and answered "failed". VR2-13
	// made the router wrap once, and this lets a case prove that flag survives the hop instead of
	// being flattened into a success.
	isErr bool
}

func (f *fakeFwd) Forward(t Target, tool string, args json.RawMessage) (any, bool, error) {
	f.lastTarget, f.lastTool, f.lastArgs = t, tool, args
	if f.err != nil {
		return nil, false, f.err
	}
	return map[string]any{"ok": true, "plane": t.Plane}, f.isErr, nil
}

func serverFor(t *testing.T, tbl *Table, fwd Forwarder) *mcpserver.Server {
	t.Helper()
	srv, err := mcpserver.NewServerWithAuth(Authenticator(tbl), Tools(tbl, fwd)...)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// TS-R1, through the REAL protocol server: a product folder's tools/list must not contain author__*.
// This is the assertion the whole router exists to make true, and it runs against mcpserver's own
// visibleTools rather than against the router's opinion of it.
func TestServer_ProductFolderToolsListHasNoAuthorTools(t *testing.T) {
	tbl := testTable(t)
	srv := serverFor(t, tbl, &fakeFwd{})

	prodPrin, err := Authenticator(tbl).Authenticate(prodTok)
	if err != nil {
		t.Fatal(err)
	}
	testPrin, _ := Authenticator(tbl).Authenticate(testTok)

	for _, name := range toolNames(srv, prodPrin) {
		if strings.HasPrefix(name, "author__") {
			t.Errorf("the PRODUCT folder's tools/list contains %q — the holdout is broken at the protocol layer", name)
		}
	}
	var sawAuthor int
	for _, name := range toolNames(srv, testPrin) {
		if strings.HasPrefix(name, "author__") {
			sawAuthor++
		}
	}
	if sawAuthor != len(AuthorTools) {
		t.Errorf("the TEST folder sees %d author tools, want %d", sawAuthor, len(AuthorTools))
	}
}

// toolNames asks the server what a principal may see, using the same path tools/list takes.
func toolNames(srv *mcpserver.Server, prin mcpserver.Principal) []string {
	var out []string
	for _, tl := range srv.VisibleToolsFor(prin.Hat) {
		out = append(out, tl.Name)
	}
	return out
}

// VR-R4 end to end: the SAME tool, the SAME instance, from two folders — and the credential that
// leaves the router differs. If these ever converge the product agent starts receiving unredacted
// reports, which is the failure this whole component exists to prevent.
func TestProxy_ForwardsThePerHatToken(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	args := json.RawMessage(`{"instance_id":"suta"}`)

	pp, _ := Authenticator(tbl).Authenticate(prodTok)
	if out := proxy(tbl, fwd, "runner__get_report", args, pp); out.IsError {
		t.Fatalf("product call failed: %+v", out)
	}
	prodToken := fwd.lastTarget.Token

	tp, _ := Authenticator(tbl).Authenticate(testTok)
	if out := proxy(tbl, fwd, "runner__get_report", args, tp); out.IsError {
		t.Fatalf("test call failed: %+v", out)
	}
	testToken := fwd.lastTarget.Token

	if prodToken == testToken {
		t.Fatal("both hats forwarded the SAME upstream token — the product agent would get unredacted reports")
	}
	if prodToken != "runner-suta" || testToken != "author-suta" {
		t.Errorf("product=%q test=%q, want the per-hat tokens", prodToken, testToken)
	}
}

// Defence in depth: even called directly — bypassing mcpserver's scope filter entirely — the proxy
// refuses an author tool for a product folder. The two checks derive the hat independently, and if
// they ever disagree the FOLDER wins.
func TestProxy_AuthorToolFromAProductFolderIsRefusedEvenBypassingTheFilter(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	pp, _ := Authenticator(tbl).Authenticate(prodTok)

	out := proxy(tbl, fwd, "author__read_scenario", json.RawMessage(`{"instance_id":"suta"}`), pp)
	if !out.IsError {
		t.Fatal("a product folder reached an author tool by calling the handler directly")
	}
	if fwd.lastTool != "" {
		t.Fatalf("the call was FORWARDED (%q) before being refused", fwd.lastTool)
	}
}

// Nothing is forwarded for an out-of-folder instance: the refusal happens before the network, so the
// control plane never sees a call the folder had no business making.
func TestProxy_OutOfFolderInstanceIsNeverForwarded(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	tp, _ := Authenticator(tbl).Authenticate(testTok)

	out := proxy(tbl, fwd, "runner__run", json.RawMessage(`{"instance_id":"sutz"}`), tp)
	if !out.IsError {
		t.Fatal("an out-of-folder instance was accepted")
	}
	if fwd.lastTool != "" {
		t.Fatal("the call reached the forwarder before the scope check")
	}
}

func TestProxy_JoinedInstanceIDIsNeverForwarded(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	tp, _ := Authenticator(tbl).Authenticate(testTok)

	out := proxy(tbl, fwd, "runner__run", json.RawMessage(`{"instance_id":"suta,sutb"}`), tp)
	if !out.IsError {
		t.Fatal(`"suta,sutb" was accepted — a half-run would be reported as success`)
	}
	if fwd.lastTool != "" {
		t.Fatal("the joined id reached the forwarder")
	}
}

// An author call from a test folder goes to the CLOUD with the user-global token; a runner call goes
// to the instance's own executor. Two planes, one endpoint for the agent.
func TestProxy_RoutesTheTwoPlanesDifferently(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	tp, _ := Authenticator(tbl).Authenticate(testTok)
	args := json.RawMessage(`{"instance_id":"suta"}`)

	if out := proxy(tbl, fwd, "author__list_scenarios", args, tp); out.IsError {
		t.Fatalf("author call failed: %+v", out)
	}
	if fwd.lastTarget.Plane != "author" || fwd.lastTarget.Token != "odts_user_global" {
		t.Errorf("author plane target = %+v", fwd.lastTarget)
	}
	if out := proxy(tbl, fwd, "runner__run", args, tp); out.IsError {
		t.Fatalf("runner call failed: %+v", out)
	}
	if fwd.lastTarget.Plane != "runner" || fwd.lastTarget.Token != "author-suta" {
		t.Errorf("runner plane target = %+v", fwd.lastTarget)
	}
}

// An upstream that could not be REACHED is not an upstream that answered "no" — the gate distinction,
// applied to the proxy path. The payload says reached:false so a caller can tell the two apart.
func TestProxy_UnreachableUpstreamSaysSoRatherThanLookingLikeAnAnswer(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{err: errors.New("dial tcp 127.0.0.1:8765: connection refused")}
	tp, _ := Authenticator(tbl).Authenticate(testTok)

	out := proxy(tbl, fwd, "runner__run", json.RawMessage(`{"instance_id":"suta"}`), tp)
	if !out.IsError {
		t.Fatal("an unreachable upstream was reported as a successful call")
	}
	blob, _ := json.Marshal(out.Payload)
	s := string(blob)
	if !strings.Contains(s, "could not reach") || !strings.Contains(s, "connection refused") {
		t.Errorf("the error must say it could not REACH the upstream and carry the reason; got %s", s)
	}
	if !strings.Contains(s, `"reached":false`) {
		t.Errorf(`the payload must carry reached:false; got %s`, s)
	}
}

// A folder torn down mid-session fails the CALL, not the connection — and says why.
func TestProxy_FolderRemovedMidSessionIsAToolError(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	stale := mcpserver.Principal{Hat: "test", Subject: "odtr_a_token_that_no_longer_exists"}

	out := proxy(tbl, fwd, "runner__run", json.RawMessage(`{"instance_id":"suta"}`), stale)
	if !out.IsError {
		t.Fatal("a stale router token was accepted")
	}
}

func TestAuthenticator_RejectsUnknownTokens(t *testing.T) {
	tbl := testTable(t)
	if _, err := Authenticator(tbl).Authenticate("odtr_nope"); err == nil {
		t.Fatal("an unknown router token authenticated")
	}
}

// The router advertises exactly the tools it can route — no more (it would 404 on call) and no less
// (an agent would think a capability was missing).
func TestTools_ExposesExactlyTheRoutableSet(t *testing.T) {
	tbl := testTable(t)
	got := Tools(tbl, &fakeFwd{})
	if len(got) != len(RunnerTools)+len(AuthorTools) {
		t.Fatalf("Tools() = %d, want %d", len(got), len(RunnerTools)+len(AuthorTools))
	}
	for _, tl := range got {
		if tl.InputSchema == nil {
			t.Errorf("%s has no input schema", tl.Name)
		}
		if !known(tl.Name) {
			t.Errorf("%s is advertised but not routable", tl.Name)
		}
	}
}

// EVERY router tool must carry a PreCheck, and this test exists because its absence was found only by
// a live end-to-end run — the unit tests all passed without it.
//
// mcpserver dispatches runner__run ASYNCHRONOUSLY (DF-06, server.go:383): it takes the run lock, runs
// the handler in a goroutine and returns a canned {"status":"running"}. The handler's Outcome is
// DISCARDED. So without a PreCheck the router's scope refusals for runner__run happened in the
// background where nobody saw them, and the caller was told the run had started.
//
// Measured against a live router before the fix:
//
//	runner__run{instance_id:"sutz"}      -> "run started in the background"   (out of folder!)
//	runner__run{instance_id:"suta,sutb"} -> "run started in the background"   (VR-R13!)
//
// A half-run reported as success is the exact failure VR-R13 names, so the router's own rule was
// being defeated by the transport it reuses. Removing a PreCheck re-opens that silently.
func TestTools_EveryToolHasAPreCheck(t *testing.T) {
	for _, tl := range Tools(testTable(t), &fakeFwd{}) {
		if tl.PreCheck == nil {
			t.Errorf("%s has no PreCheck — an async tool would announce itself before the scope check ran", tl.Name)
		}
	}
}

// And the pre-check must actually REFUSE, not merely exist.
func TestPreCheck_RefusesBeforeTheHandlerRuns(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	tp, _ := Authenticator(tbl).Authenticate(testTok)

	var run *mcpserver.Tool
	for i, tl := range Tools(tbl, fwd) {
		if tl.Name == "runner__run" {
			run = &Tools(tbl, fwd)[i]
		}
	}
	if run == nil {
		t.Fatal("runner__run is not advertised")
	}
	for _, bad := range []string{`{"instance_id":"sutz"}`, `{"instance_id":"suta,sutb"}`, `{}`} {
		if o := run.PreCheck(json.RawMessage(bad), tp); o == nil {
			t.Errorf("PreCheck(%s) allowed the call — it would be announced as started", bad)
		}
	}
	if o := run.PreCheck(json.RawMessage(`{"instance_id":"suta"}`), tp); o != nil {
		t.Errorf("PreCheck refused a legitimate in-scope call: %+v", o)
	}
}

// THE GUARD FOR THE BLIND SPOT ITSELF. The router must hand the forwarder the caller's arguments
// UNCHANGED — every tool, every field. Rewriting for the in-env executor is the FORWARDER's job
// (localiseInstanceID, tested against a real HTTP server in localise_test.go), and splitting the two
// is what keeps each one assertable.
//
// Before this, `Forward` took `_ json.RawMessage` and no test in this file could observe a payload
// at all. Four join defects shipped under that gap in a single day.
func TestProxy_ForwardsTheCallersArgumentsUnchanged_EveryTool(t *testing.T) {
	tbl := testTable(t)
	fwd := &fakeFwd{}
	tp, err := Authenticator(tbl).Authenticate(testTok)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"instance_id":"suta","marker":"keep-me","n":7}`)

	for _, tool := range append(append([]string{}, RunnerTools...), AuthorTools...) {
		t.Run(tool, func(t *testing.T) {
			fwd.lastArgs = nil
			if out := proxy(tbl, fwd, tool, args, tp); out.IsError {
				t.Fatalf("%s refused: %+v", tool, out)
			}
			if len(fwd.lastArgs) == 0 {
				t.Fatalf("%s: the forwarder received NO arguments", tool)
			}
			var got map[string]any
			if err := json.Unmarshal(fwd.lastArgs, &got); err != nil {
				t.Fatalf("%s: forwarded arguments are not an object: %v (%s)", tool, err, fwd.lastArgs)
			}
			if got["marker"] != "keep-me" || got["n"] != float64(7) {
				t.Fatalf("%s: the router altered or dropped the caller's arguments: %s", tool, fwd.lastArgs)
			}
			// instance_id must reach the FORWARDER unchanged. Localising it to `local` is the
			// forwarder's job on the runner plane, and doing it here instead would break the author
			// plane, where the control plane needs the real id.
			if got["instance_id"] != "suta" {
				t.Fatalf("%s: instance_id was rewritten before the forwarder saw it: %s", tool, fwd.lastArgs)
			}
		})
	}
}

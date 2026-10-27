package router

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// The transport half. These tests drive the REAL mcpserver — the same protocol code the in-env and cloud servers use —
// so the scope filtering under test is the one that will actually run, not a re-implementation of it. Since AC-D70 the
// router has no static tool and no forwarder: every tool comes per session from the control plane (author) or from each
// instance's executor (runner), and the stub executors here are the executor's own protocol code (stubexec_test.go).

// TS-R1, through the REAL protocol server: a product folder's tools/list must not contain an author tool.
// This is the assertion the whole router exists to make true. It runs through a real session (initialize, then
// tools/list) — VisibleToolsFor reads static tools only and is empty on the router for every hat, so a check through it
// would pass while testing nothing. The test folder seeing them is the positive control.
func TestServer_ProductFolderToolsListHasNoAuthorTools(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("p", prodTok)
	h.initialize("t", testTok)
	_, prodNames := h.listed("p", prodTok)
	for _, name := range prodNames {
		if strings.HasPrefix(name, "author_") { // covers both spellings
			t.Errorf("the PRODUCT folder's tools/list contains %q — the holdout is broken at the protocol layer", name)
		}
	}
	if got := strings.Join(prodNames, ","); got != runnerNames {
		t.Errorf("the PRODUCT folder lists %q, want exactly the runner tools its executors list", got)
	}
	_, testNames := h.listed("t", testTok)
	if n := len(authorNames(testNames)); n != len(cpTools()) {
		t.Errorf("the TEST folder sees %d author tools, want the control plane's %d", n, len(cpTools()))
	}
}

// VR-R4 end to end: the SAME tool, the SAME instance, from two folders — and the credential that leaves the router
// differs. If these ever converge the product agent starts receiving unredacted reports, which is the failure this whole
// component exists to prevent. The executor here redacts by the token it is given, so the answers show it too.
func TestProxy_ForwardsThePerHatToken(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("p", prodTok)
	h.initialize("t", testTok)
	pRaw := h.call("p", prodTok, 1, "runner_get_report", map[string]any{"instance_id": "suta"})
	prodToken := h.ex["suta"].lastCall().Bearer
	tRaw := h.call("t", testTok, 2, "runner_get_report", map[string]any{"instance_id": "suta"})
	testToken := h.ex["suta"].lastCall().Bearer
	if prodToken == testToken {
		t.Fatal("both hats forwarded the SAME upstream token — the product agent would get unredacted reports")
	}
	if prodToken != "Bearer runner-suta" || testToken != "Bearer author-suta" {
		t.Errorf("product=%q test=%q, want the per-hat tokens", prodToken, testToken)
	}
	if strings.Contains(string(pRaw), sentinelExpected) || !strings.Contains(string(tRaw), sentinelExpected) {
		t.Errorf("redaction: the product report carries the expected value %v, the test report %v",
			strings.Contains(string(pRaw), sentinelExpected), strings.Contains(string(tRaw), sentinelExpected))
	}
}

// Defence in depth: even called directly — bypassing mcpserver's scope filter entirely — a runner handler forwards only
// a runner name the target's executor listed. An author name is never offered by an executor (decision 13), so it is
// refused, and nothing reaches any executor's tools/call.
func TestProxy_AnAuthorNameNeverReachesAnExecutorEvenBypassingTheFilter(t *testing.T) {
	h := newHarness(t, Options{})
	s := newSessions(h.tbl, Options{})
	set := s.sessionTools("x", mcpserver.Principal{Hat: role.Product, Subject: prodTok})
	snap := set.State.(*sessionState).runner
	out := s.runnerCall("author_read_scenario", snap, nil, json.RawMessage(`{"instance_id":"suta"}`), mcpserver.Principal{Hat: role.Product, Subject: prodTok})
	if !out.IsError {
		t.Fatal("a product folder reached an author tool by calling a runner handler directly")
	}
	if n := h.execToolCalls(); n != 0 {
		t.Fatalf("%d calls reached an executor", n)
	}
}

// An author call from a test folder goes to the CLOUD with the machine's author token; a runner call goes to the
// instance's own executor with the folder's executor token. Two planes, one endpoint for the agent.
func TestProxy_RoutesTheTwoPlanesDifferently(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	h.call("s", testTok, 1, "author_list_runs", map[string]any{"instance_id": "suta"})
	if got := h.cp.lastCall(); got.Tool != "author_list_runs" || got.Bearer != "Bearer "+cpTokA {
		t.Errorf("author call reached the control plane as %+v", got)
	}
	if n := h.execToolCalls(); n != 0 {
		t.Errorf("the author call reached an executor (%d calls)", n)
	}
	cpCalls := h.cp.count("tools/call")
	h.call("s", testTok, 2, "runner_run", map[string]any{"instance_id": "suta"})
	if got := h.ex["suta"].lastCall(); got.Tool != "runner_run" || got.Bearer != "Bearer author-suta" {
		t.Errorf("runner call reached the executor as %+v", got)
	}
	if h.cp.count("tools/call") != cpCalls {
		t.Error("the runner call reached the control plane")
	}
}

// A folder torn down mid-session fails the CALL, not the connection — and says why.
func TestProxy_FolderRemovedMidSessionIsAToolError(t *testing.T) {
	h := newHarness(t, Options{})
	s := newSessions(h.tbl, Options{})
	set := s.sessionTools("x", mcpserver.Principal{Hat: role.Test, Subject: testTok})
	stale := mcpserver.Principal{Hat: role.Test, Subject: "odtr_a_token_that_no_longer_exists"}
	out := s.runnerCall("runner_run", set.State.(*sessionState).runner, nil, json.RawMessage(`{"instance_id":"suta"}`), stale)
	if !out.IsError {
		t.Fatal("a stale router token was accepted")
	}
	if n := h.execToolCalls(); n != 0 {
		t.Fatalf("%d calls were forwarded for a folder that is gone", n)
	}
}

func TestAuthenticator_RejectsUnknownTokens(t *testing.T) {
	tbl := testTable(t)
	if _, err := Authenticator(tbl).Authenticate("odtr_nope"); err == nil {
		t.Fatal("an unknown router token authenticated")
	}
}

// THE GUARD FOR THE BLIND SPOT ITSELF. The executor must receive the caller's arguments — every tool, every field — with
// only instance_id localised. Four join defects reached production on 2026-08-09 because no test could see what payload
// the upstream received; the stub executor records it.
func TestProxy_ForwardsTheCallersArgumentsUnchanged_EveryTool(t *testing.T) {
	h := newHarness(t, Options{})
	h.initialize("s", testTok)
	for _, tool := range strings.Split(runnerNames, ",") {
		t.Run(tool, func(t *testing.T) {
			h.call("s", testTok, 3, tool, json.RawMessage(`{"instance_id":"suta","marker":"keep-me","n":7}`))
			got := h.ex["suta"].lastCall()
			if got.Tool != tool {
				t.Fatalf("the executor received %q", got.Tool)
			}
			if want := `{"instance_id":"local","marker":"keep-me","n":7}`; string(got.Args) != want {
				t.Fatalf("the executor received %s, want %s — every other argument as sent, instance_id localised", got.Args, want)
			}
		})
	}
}

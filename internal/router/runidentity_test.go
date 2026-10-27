package router

import (
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// INT-037 — the router must NOT mint a run id of its own.
//
// Measured live 2026-08-10, before this fix: a run driven through the router returned run_id 20260810T052833985, while
// the executor ran, reported and ledgered 20260810T052834090. Polling the caller's id gave `pending_or_unknown` forever;
// polling the executor's gave the real report. The run was fine — its IDENTITY was not, and the run tool → get_report is
// the loop CLAUDE.md documents as primary.
//
// Cause: mcpserver's async branch keyed on the tool NAME, and the router registered a tool with the same name whose
// handler merely forwarded. Both servers ran the branch and each minted an id. Async is a field now (types.go), and the
// router never sets it: the executor runs the run in the background and answers at once. The end-to-end proof — the id
// the router hands back IS the one the executor minted, and get_report with it returns that run — is
// TestRunnerHoldout_ARunThroughTheRouterIsTheExecutorsOwnEndToEnd.

func TestRouterTools_RunIsNotAsync(t *testing.T) {
	h := newHarness(t, Options{})
	s := newSessions(h.tbl, Options{ListTimeout: time.Second})
	for _, prin := range []mcpserver.Principal{{Hat: role.Test, Subject: testTok}, {Hat: role.Product, Subject: prodTok}} {
		tools := s.sessionTools("x", prin).Tools.Tools()
		found := false
		for _, tool := range tools {
			found = found || tool.Name == "runner_run"
			if tool.Async {
				t.Fatalf("a session set holds %q as Async — the router would mint its own run id and hand the caller a "+
					"receipt for a run filed under a different number (INT-037)", tool.Name)
			}
		}
		if !found {
			t.Fatalf("%s: the session set holds no runner_run — the guard would check nothing", prin.Hat)
		}
	}
}

// ...while the IN-ENV executor still IS async, because a scenario run must not make an MCP client wait minutes for a
// reply. Both halves matter: if this ever became false the fix would have traded one bug for a worse one.
func TestDefaultTools_RunStaysAsyncInEnv(t *testing.T) {
	var found bool
	for _, tool := range mcpserver.DefaultTools(toolcore.Env{}) {
		if tool.Name == "runner_run" {
			found = true
			if !tool.Async {
				t.Fatal("the in-env runner_run is no longer Async — an MCP client would block for " +
					"the whole run instead of getting {status:running, run_id}")
			}
		}
	}
	if !found {
		t.Fatal("DefaultTools no longer registers runner_run")
	}
}

// A refusal is not unreachability: a busy executor's -32002 comes back as the executor's own JSON-RPC error
// (TestRunnerCall_AnExecutorsJSONRPCErrorIsPassedBackAsItsOwn), and only an executor that could not be reached is
// reported as "… got no usable answer … it is not reachable …", reached:false
// (TestRunnerCauses_AnExecutorThatGoesDownMidSessionFailsAsToday).

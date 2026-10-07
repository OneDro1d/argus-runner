package router

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// INT-037 — the router must NOT mint a run id of its own.
//
// Measured live 2026-08-10, before this fix: a run driven through the router returned run_id
// 20260810T052833985, while the executor ran, reported and ledgered 20260810T052834090. Polling the
// caller's id gave `pending_or_unknown` forever; polling the executor's gave the real report. The run
// was fine — its IDENTITY was not, and `runner__run` -> `runner__get_report` is the loop CLAUDE.md
// documents as primary.
//
// Cause: mcpserver's async branch keyed on the tool NAME, and the router registers a tool with the
// same name whose handler merely forwards. Both servers ran the branch and each minted an id.

func TestRouterTools_RunIsNotAsync(t *testing.T) {
	tbl := &Table{}
	for _, tool := range Tools(tbl, nil) {
		if tool.Async {
			t.Fatalf("the router registered %q as Async — it would mint a run id the executor then "+
				"discards, and hand the caller a receipt for a run filed under a different number (INT-037)",
				tool.Name)
		}
	}
}

// ...while the IN-ENV executor still IS async, because a scenario run must not make an MCP client
// wait minutes for a reply. Both halves matter: if this ever became false the fix would have traded
// one bug for a worse one.
func TestDefaultTools_RunStaysAsyncInEnv(t *testing.T) {
	var found bool
	for _, tool := range mcpserver.DefaultTools(toolcore.Env{}) {
		if tool.Name == "runner__run" {
			found = true
			if !tool.Async {
				t.Fatal("the in-env runner__run is no longer Async — an MCP client would block for " +
					"the whole run instead of getting {status:running, run_id}")
			}
		}
	}
	if !found {
		t.Fatal("DefaultTools no longer registers runner__run")
	}
}

// --- the other half of fix B: a refusal is not unreachability ---

type refusingFwd struct{ err error }

func (f refusingFwd) Forward(Target, string, json.RawMessage) (any, bool, error) {
	return nil, false, f.err
}

// A busy executor answers -32002. Now that the router forwards runner__run synchronously, that
// answer travels back through the proxy — and reporting it as "could not reach" would send someone
// to check the network for a perfectly healthy instance that simply said "not yet".
func TestProxy_ARefusalIsReportedAsReached(t *testing.T) {
	tbl := testTable(t)
	prin, err := Authenticator(tbl).Authenticate(testTok)
	if err != nil {
		t.Fatal(err)
	}
	out := proxy(tbl, refusingFwd{&UpstreamRefusal{Code: -32002, Message: "run already in progress on this instance"}},
		"runner__run", json.RawMessage(`{"instance_id":"suta"}`), prin)

	m, ok := out.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is not a map: %#v", out.Payload)
	}
	if m["reached"] != true {
		t.Fatalf("reached = %v, want true — the upstream ANSWERED, it was not unreachable", m["reached"])
	}
	if !strings.Contains(m["error"].(string), "run already in progress") {
		t.Fatalf("the upstream's own message was lost: %v", m["error"])
	}
	if m["code"] != -32002 {
		t.Fatalf("code = %v, want -32002 preserved — a caller that sees it knows to retry later, "+
			"which a flattened error cannot tell it", m["code"])
	}
}

// ...and a genuinely unreachable upstream still says so. The two must not collapse into one message.
func TestProxy_UnreachableStillSaysUnreachable(t *testing.T) {
	tbl := testTable(t)
	prin, err := Authenticator(tbl).Authenticate(testTok)
	if err != nil {
		t.Fatal(err)
	}
	out := proxy(tbl, refusingFwd{errNotReachable{}},
		"runner__run", json.RawMessage(`{"instance_id":"suta"}`), prin)

	m := out.Payload.(map[string]any)
	if m["reached"] != false {
		t.Fatalf("reached = %v, want false for a genuinely unreachable upstream", m["reached"])
	}
	if !strings.Contains(m["error"].(string), "could not reach") {
		t.Fatalf("an unreachable upstream must say so: %v", m["error"])
	}
}

type errNotReachable struct{}

func (errNotReachable) Error() string { return "endpoint not reachable" }

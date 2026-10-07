package mcpserver

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// TestRunnerRun_ArgumentGuardRunsBeforeTheFence — the gate-2026-08-06 defect.
//
// runner__run is dispatched ASYNC: the server takes the local run lock, mints a run_id and
// acquires the CP run fence BEFORE the handler executes, and the handler's Outcome is then
// discarded (only ok/not-ok reaches RunDone). The mandatory-instance_id guard lived only in
// the handler, so a call missing instance_id was answered {"status":"running"} with a run_id
// that never resolved, left the instance FENCED, and wrote a phantom failed run to the cloud
// ledger — while get_report told the caller to keep polling, forever.
//
// The guard must refuse SYNCHRONOUSLY, before anything is locked, fenced or announced.
func TestRunnerRun_ArgumentGuardRunsBeforeTheFence(t *testing.T) {
	env := toolcore.Env{Instance: "local", Grafana: "http://localhost:3000"}
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", rtok)

	fenced := false
	s.RunBeginner = func(runID, scope string) error { fenced = true; return nil }
	s.RunPreflight = func(layer, tag, scenarioID string) error { return nil }

	raw, _ := s.Dispatch("s", rtok, reqBytes(1, "tools/call", map[string]any{
		"name": "runner__run", "arguments": map[string]any{"scenario_ref": "ORDE-017"},
	}))
	got := string(raw)

	if !strings.Contains(got, "instance_id is required") {
		t.Fatalf("a missing instance_id must be refused synchronously; got: %s", got)
	}
	if strings.Contains(got, `"status":"running"`) {
		t.Errorf("a refused call must NOT announce a run; got: %s", got)
	}
	if strings.Contains(got, `"run_id"`) {
		t.Errorf("a refused call must NOT hand out a run_id to poll; got: %s", got)
	}
	if fenced {
		t.Error("the CP run fence was acquired for a call that never had a valid instance_id")
	}
	if s.runActive {
		t.Error("the local run lock leaked on a refused call")
	}

	// A WRONG instance_id must be refused the same way, and equally early.
	fenced = false
	raw2, _ := s.Dispatch("s", rtok, reqBytes(2, "tools/call", map[string]any{
		"name": "runner__run", "arguments": map[string]any{"instance_id": "not-this-one"},
	}))
	if got2 := string(raw2); !strings.Contains(got2, "not-this-one") {
		t.Errorf("a non-matching instance_id must be refused by name; got: %s", got2)
	}
	if fenced {
		t.Error("the fence was acquired for a non-matching instance_id")
	}
}

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// up_protocol_naming_test.go — #48: the --json output must match docs/ARGUS-UP-PROTOCOL.md exactly:
// (a) every refusal — including one before onboarding's first numbered step — carries a step NAME,
// never "", (b) `up`'s own usage errors are one JSON object on stdout, exactly like every other
// event, and (c) the dashboard link reaches a final done/ok event.

// TestUp_JSON_StrayArgument_IsOneLineFailEvent — `up`'s own usage error (an unexpected positional
// argument) must be exactly one JSON object on stdout under --json, like every other event — not the
// multi-line indented blob emitErr's generic emit() produces.
func TestUp_JSON_StrayArgument_IsOneLineFailEvent(t *testing.T) {
	stdout, code := upRun(t, "", "--json", "stray")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	lines := nonBlankLines(stdout)
	if len(lines) != 1 {
		t.Fatalf("want exactly one line of --json output for a usage error, got %d:\n%s", len(lines), stdout)
	}
	var ev jsonEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("stdout line did not parse as one JSON object: %v\n  line: %q", err, lines[0])
	}
	if ev.State != "fail" {
		t.Fatalf("got state=%q, want fail:\n%s", ev.State, stdout)
	}
}

// TestUpStepTranslator_OnExitSuccess_CarriesDoneDetailThrough — onboard.sh's own fd-5 wire never
// writes "ok" (json_step's contract); `up`'s inference must still carry the LAST open step's own
// detail (e.g. the dashboard URL onboard.sh's "done" step names) into the inferred "ok" event, not
// drop it, since the "ok" line is the only one a --json caller sees for that step's outcome.
func TestUpStepTranslator_OnExitSuccess_CarriesDoneDetailThrough(t *testing.T) {
	var buf strings.Builder
	tr := &upStepTranslator{w: &buf}
	tr.onLine("8/8", "start", "done.")
	tr.onLine("done", "start", "http://cp.example.test/")
	tr.onExitSuccess()

	var last jsonEvent
	lines := nonBlankLines(buf.String())
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("final line did not parse as JSON: %v\n  line: %q", err, lines[len(lines)-1])
	}
	if last.Step != "done" || last.State != "ok" {
		t.Fatalf("got step=%q state=%q, want done/ok:\n%s", last.Step, last.State, buf.String())
	}
	if last.Detail != "http://cp.example.test/" {
		t.Fatalf("the inferred \"ok\" for \"done\" dropped its own detail (the dashboard URL): got %q", last.Detail)
	}
}

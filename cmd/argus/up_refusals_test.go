package main

import (
	"encoding/json"
	"testing"
)

// The three refusals onboard.sh can raise before the stack comes up must reach the caller of
// `argus up --json` as `fail` lines that name the step — not only on onboard.sh's own fd-5 wire
// (onboard_json_step_hook_test.go proves that layer) but through `up`'s translator on stdout,
// which is the surface the protocol document and the skill page describe.

func upFailSteps(t *testing.T, stdout string) map[string]string {
	t.Helper()
	fails := map[string]string{}
	for _, l := range nonBlankLines(stdout) {
		var ev jsonEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("stdout line did not parse as one JSON object: %v\n  line: %q\n  all stdout:\n%s", err, l, stdout)
		}
		if ev.State == "fail" {
			fails[ev.Step] = ev.Detail
		}
	}
	return fails
}

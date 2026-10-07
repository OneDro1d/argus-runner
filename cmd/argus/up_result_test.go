package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// T2.4 — "no screen-scraping to learn what happened": a completed `up --json` ends with ONE
// {"step":"result"} line carrying what onboarding actually installed and where it can be reached.

// fakeOnboardFullRecord writes the whole #46/T2.4 record (renamed id, a k8s tier, URLs) and exits
// with the given code — 0, or onboard.sh's own "registered but not Ready yet" 3.
func fakeOnboardFullRecord(exit int) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail
INSTANCE_ID=""
while [ $# -gt 0 ]; do
  case "$1" in
    --instance-id) INSTANCE_ID="$2"; shift 2;;
    *) shift;;
  esac
done
if [ -n "${ARGUS_UP_RECORD:-}" ]; then
  printf '{"instance_id":"%%s-v1","tier":"k3d","executor":"img@sha256:abc","workspace_id":"ws_42","control_plane":"https://cp.example","web":"https://cp.example/","grafana":"https://grafana.example","executor_mcp":"http://localhost:8766/mcp"}\n' "$INSTANCE_ID" > "$ARGUS_UP_RECORD"
fi
exit %d
`, exit)
}

// lastResult returns the final stdout line decoded, failing unless it is the result event.
func lastResult(t *testing.T, stdout string) upResult {
	t.Helper()
	lines := nonBlankLines(stdout)
	if len(lines) == 0 {
		t.Fatalf("`up --json` wrote nothing")
	}
	var ev jsonEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil {
		t.Fatalf("last line is not one JSON object: %v\n  %q", err, lines[len(lines)-1])
	}
	if ev.Step != "result" || ev.State != "ok" || ev.Result == nil {
		t.Fatalf("the LAST line must be {\"step\":\"result\",\"state\":\"ok\",\"result\":{…}}, got %q\n  all stdout:\n%s", lines[len(lines)-1], stdout)
	}
	return *ev.Result
}

func TestUp_JSON_EndsWithTheStructuredResult(t *testing.T) {
	args := append(upManifestRecordArgs(t, "probe-result", "", "img:given"), "--json")
	upFakeOnboardHarness(t, fakeOnboardFullRecord(0))

	stdout, code := upRun(t, "", args...)
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, stdout)
	}
	r := lastResult(t, stdout)
	// The id onboarding REGISTERED (renamed on a collision), never the one it was asked for.
	if r.InstanceID != "probe-result-v1" || r.Tier != "k3d" || r.Namespace != "argus-inst-probe-result-v1" || r.ComposeProject != "" {
		t.Errorf("identity/placement: %+v", r)
	}
	if r.Executor != "img@sha256:abc" || r.WorkspaceID != "ws_42" || !r.Ready || !r.RecordedByOnboarding {
		t.Errorf("executor/workspace/ready/recorded: %+v", r)
	}
	want := map[string]string{"control_plane": "https://cp.example", "web": "https://cp.example/",
		"grafana": "https://grafana.example", "executor_mcp": "http://localhost:8766/mcp"}
	for k, v := range want {
		if r.URLs[k] != v {
			t.Errorf("urls[%s] = %q, want %q (all: %v)", k, r.URLs[k], v, r.URLs)
		}
	}
	// The protocol contract still holds for the new line: one object, a valid state.
	for _, l := range nonBlankLines(stdout) {
		var ev jsonEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil || ev.State == "" {
			t.Errorf("not a protocol line: %q", l)
		}
	}
}

// Exit 3 is "registered, wired, not Ready yet" — the result still comes, and says which.
func TestUp_JSON_ResultOnNotReadySaysSo(t *testing.T) {
	args := append(upManifestRecordArgs(t, "probe-notready", "", "img:given"), "--json")
	upFakeOnboardHarness(t, fakeOnboardFullRecord(3))

	stdout, code := upRun(t, "", args...)
	if code != 3 {
		t.Fatalf("exit=%d, want onboard.sh's own 3 passed through\n%s", code, stdout)
	}
	r := lastResult(t, stdout)
	if r.Ready || r.NotReadyReason == "" {
		t.Errorf("an exit-3 onboard must report ready=false with a reason: %+v", r)
	}
}

// An older onboard.sh writes no record: the result still comes, from argv, and SAYS it came from argv.
func TestUp_JSON_ResultWithoutARecordSaysItIsFromArgv(t *testing.T) {
	args := append(upManifestRecordArgs(t, "probe-norec", "compose", "img:given"), "--json")
	upFakeOnboardHarness(t, fakeOnboardNoRecord)

	stdout, _ := upRun(t, "", args...)
	r := lastResult(t, stdout)
	if r.RecordedByOnboarding {
		t.Errorf("no record was written, so recorded_by_onboarding must be false: %+v", r)
	}
	if r.InstanceID != "probe-norec" || r.Tier != "compose" || r.ComposeProject != "argus-inst-probe-norec" || r.Namespace != "" {
		t.Errorf("argv fallback: %+v", r)
	}
}

// A failed onboard ends with its fail event — never a result claiming an instance exists.
func TestUp_JSON_NoResultOnFailure(t *testing.T) {
	args := append(upManifestRecordArgs(t, "probe-fail", "compose", "img:given"), "--json")
	upFakeOnboardHarness(t, "#!/usr/bin/env bash\nexit 1\n")

	stdout, code := upRun(t, "", args...)
	if code == 0 {
		t.Fatalf("a failing onboard exited 0")
	}
	if strings.Contains(stdout, `"step":"result"`) {
		t.Errorf("a failed onboard must not emit a result:\n%s", stdout)
	}
}

// The token passed to `up` never reaches the result (or any line): URLs and ids only.
func TestUp_JSON_ResultCarriesNoToken(t *testing.T) {
	const secret = "tok_T24_must_not_appear"
	args := append(upManifestRecordArgs(t, "probe-notoken", "", "img:given"), "--json", "--token", secret)
	upFakeOnboardHarness(t, fakeOnboardFullRecord(0))

	stdout, _ := upRun(t, "", args...)
	if strings.Contains(stdout, secret) {
		t.Fatalf("the --token value reached `up --json` stdout:\n%s", stdout)
	}
	_ = lastResult(t, stdout)
}

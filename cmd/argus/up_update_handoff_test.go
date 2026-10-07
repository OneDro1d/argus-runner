package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// up_update_handoff_test.go — second pass: THE SECOND-RUN HANDOFF (up.go's runUp against an
// instance that already has an installed manifest).
//
// ⛔ RED-FIRST. secondRunInstance and upSecondRun do not exist yet when this file is first added —
// these tests are expected to fail to COMPILE until up_second_run.go lands, and then to fail on their
// own assertions until runUp's stub seam is filled in.

// TestUp_SecondRun_BuildsInstanceAndCallsRenderBlock proves secondRunInstance builds EXACTLY the
// updatecmd.Instance a hand-built one would, and that RenderBlock renders identically from either —
// the thing that mechanically enforces "never a copy of RenderBlock's own logic", not just "looks
// similar".
func TestUp_SecondRun_BuildsInstanceAndCallsRenderBlock(t *testing.T) {
	a := upArgs{KubeContext: "k3d-argus", Kubeconfig: `/home/op/.kube/config`, Image: "ghcr.io/x/y@sha256:abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabc"}
	m := updatecmd.Manifest{InstanceID: "orders-k3d", Tier: "k3d", Executor: "ghcr.io/x/y@sha256:oldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldoldold"}
	kitDir := "/home/op/kit"

	want := updatecmd.Instance{
		Tier:        "k3d",
		InstanceID:  "orders-k3d",
		Image:       a.Image, // --image (and ARGUS_MCP_IMAGE) win over the installed executor
		KitDir:      kitDir,
		KubeContext: a.KubeContext,
		Kubeconfig:  a.Kubeconfig,
	}

	got := secondRunInstance(a, m, kitDir)
	if got != want {
		t.Fatalf("secondRunInstance = %+v, want %+v", got, want)
	}

	wantBlock, wantPH := updatecmd.RenderBlock(want)
	gotBlock, gotPH := updatecmd.RenderBlock(got)
	if gotBlock == "" {
		t.Fatalf("RenderBlock refused a well-formed second-run instance: %+v", got)
	}
	if gotBlock != wantBlock || gotPH != wantPH {
		t.Fatalf("RenderBlock(secondRunInstance(...)) differs from RenderBlock(the hand-built Instance):\ngot:\n%s\nwant:\n%s", gotBlock, wantBlock)
	}

	// no --image and no ARGUS_MCP_IMAGE: falls back to the manifest's currently installed image, not "".
	bare := upArgs{}
	fallback := secondRunInstance(bare, m, kitDir)
	if fallback.Image != m.Executor {
		t.Fatalf("with no --image given, secondRunInstance.Image = %q, want the installed executor %q", fallback.Image, m.Executor)
	}

	// Rollback/RollbackToVersion/TargetImage are left at zero values — `up` exposes no rollback flag.
	if got.Rollback || got.RollbackToVersion != "" || got.TargetImage != "" {
		t.Fatalf("secondRunInstance set a rollback field despite `up` having no --rollback flag: %+v", got)
	}
}

// ⛔ AC-D53 (BR-3): AFTER AN UNCONFIRMED EXECUTOR MOVE THE RECORD HAS NO EXECUTOR IMAGE — it is unknown, on
// purpose — and a bare `argus up` fell back to that empty image, so RenderBlock refused and `up` stopped with
// "its recorded tier, image or kit directory is incomplete". The re-run the NOTE calls the intended next step
// never started. The not-confirmed marker carries the image that update was moving to: `up` re-runs THAT update.
func TestUp_SecondRun_ACD53_AnUnconfirmedExecutorReRunsTheUpdateItWasMovingTo(t *testing.T) {
	const moving = "ghcr.io/x/y@sha256:newnewnewnewnewnewnewnewnewnewnewnewnewnewnewnewnewnewnewnewnew"
	m := updatecmd.Manifest{InstanceID: "orders-compose", Tier: "compose",
		LastOutcome: &updatecmd.LastOutcome{Word: "rollback-unreachable", FailedStep: "A-3"},
		Unconfirmed: &updatecmd.Unconfirmed{Image: moving}}
	got := secondRunInstance(upArgs{}, m, "/home/op/kit")
	if got.Image != moving {
		t.Fatalf("with the executor unconfirmed and no --image, `up` targets %q, want the image the update was moving to (%s)", got.Image, moving)
	}
	if block, _ := updatecmd.RenderBlock(got); block == "" {
		t.Fatalf("RenderBlock refused the re-run of an unconfirmed update: %+v", got)
	}
	// --image still wins
	if got := secondRunInstance(upArgs{Image: "ghcr.io/x/y@sha256:other"}, m, "/home/op/kit"); got.Image != "ghcr.io/x/y@sha256:other" {
		t.Errorf("--image lost to the marker: %q", got.Image)
	}
	// design check O-1: a blank --image names no image — here as in ImageGiven — so the fallback applies; it rendered
	// IMG=' ' and the block died on `docker pull ' '`
	if got := secondRunInstance(upArgs{Image: " "}, m, "/home/op/kit"); got.Image != m.Unconfirmed.Image {
		t.Errorf("a blank --image was used as the image: %q, want the marker's %q", got.Image, m.Unconfirmed.Image)
	}
}

// ⛔ AC-D53 (K-3): AN UNCONFIRMED ROLLBACK IS NEVER RE-RUN AS A FORWARD UPDATE OF THE OLDER IMAGE. The marker of a
// rollback names the rollback's target; a bare `argus up` fell back to it and rendered the FORWARD block — the plan
// run inside the older image, `updated` for `rolled-back-to`, `previous` moved, and A-6 run. `up` has no rollback
// flag, so it stops and says how the rollback is re-run instead; the block never starts.
//
// ⛔ AND AN EXPORTED ARGUS_MCP_IMAGE IS NOT "--image" HERE (operator decision 2026-09-30): the runbook exports it for
// onboarding, so it is set in the very shell an operator re-runs from — ambient, not a choice to leave a rollback.
// Only --image typed on the command line runs a forward update instead.
func TestUp_SecondRun_ACD53_AnUnconfirmedRollbackIsNotReRunAsAForwardUpdate(t *testing.T) {
	const newer = "ghcr.io/x/y@sha256:5555555555555555555555555555555555555555555555555555555555555555"
	for _, c := range []struct {
		name     string
		exported string
		args     []string
		refused  bool
	}{
		{"no --image", "", nil, true},
		{"ARGUS_MCP_IMAGE exported, no --image", newer, nil, true},
		{"--image on the command line", "", []string{"--image", newer}, false},
		// L2-2: an EMPTY --image names no image — it is not a choice to leave the rollback (a wrapper passing
		// --image "$VAR" with VAR unset); counting it fell back to the marker's older image and ran the forward block
		{"--image given empty", "", []string{"--image", ""}, true},
		{"--image= given empty", "", []string{"--image="}, true},
		{"--image given blank, ARGUS_MCP_IMAGE exported", newer, []string{"--image", " "}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ARGUS_MCP_IMAGE", c.exported)
			instanceID := "orders-compose"
			older := "ghcr.io/x/y@sha256:" + strings.Repeat("3", 64)
			// a block that WOULD run to `updated` — so a forward re-run cannot pass for the refusal
			planStub := `        cat > "$stage/preflight.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf 'preflight ok\n'
EOSCRIPT
        chmod +x "$stage/preflight.sh"
        cat > "$stage/apply.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf 'updated\n'
EOSCRIPT
        chmod +x "$stage/apply.sh"`
			secondRunHarness(t, instanceID, "ghcr.io/x/y@sha256:"+strings.Repeat("4", 64), planStub)
			state := os.Getenv("ARGUS_ROUTER_STATE")
			if err := os.WriteFile(filepath.Join(state, "installed", instanceID+".unconfirmed"),
				[]byte("image="+older+"\nversion=0.3.32\nrollback_to=0.3.32\nat=2026-09-30T13:00:00Z\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			stdout, code := upRun(t, "", append([]string{"--runner-id", instanceID, "--json"}, c.args...)...)
			if !c.refused {
				if code != 0 || !strings.Contains(stdout, `"step":"apply"`) {
					t.Errorf("--image given on the command line did not run the forward update (exit %d):\n%s", code, stdout)
				}
				return
			}
			if code == 0 {
				t.Fatalf("`argus up` after an unconfirmed ROLLBACK exited 0 — it re-ran it as a forward update:\n%s", stdout)
			}
			if strings.Contains(stdout, `"step":"apply"`) || strings.Contains(stdout, `"step":"discover"`) {
				t.Errorf("the update block ran:\n%s", stdout)
			}
			if !strings.Contains(stdout, "rollback") || !strings.Contains(stdout, "0.3.32") {
				t.Errorf("`up` does not say the unconfirmed move was a rollback to 0.3.32:\n%s", stdout)
			}
			// round-4 review R4-BASH-3: an operator who DID pass --image, empty, is told why it did not count
			if !strings.Contains(stdout, "an empty --image") {
				t.Errorf("the refusal does not say an empty --image does not count:\n%s", stdout)
			}
		})
	}
}

// ⛔ AC-D53 (L8): THE --json PROTOCOL CARRIES THE RUN'S OWN SENTENCE. After rollback-unreachable what the operator
// (or the agent driving `up`) does next depends on what that run saw — who stopped answering, whether the executor
// was put back — and apply.sh writes it to $STAGE/outcome-note. The terminal event carried only the word, and the
// argus skill told the agent to report the sentence it had no way to read.
func TestUp_SecondRun_ACD53_RollbackUnreachableCarriesTheRunsOwnNote(t *testing.T) {
	t.Setenv("ARGUS_MCP_IMAGE", "")
	instanceID := "orders-compose"
	const saw = "the executor was moved to X and its health there could not be confirmed; it could not be put back because the container runtime stopped answering (Cannot connect)."
	const advice = "Re-run this update once the container runtime answers — it is safe to run again."
	planStub := `        cat > "$stage/preflight.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf 'preflight ok\n'
EOSCRIPT
        chmod +x "$stage/preflight.sh"
        cat > "$stage/apply.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf '%s\n%s\n' '` + saw + `' '` + advice + `' > "$(dirname "$0")/outcome-note"
printf 'rollback-unreachable\n'
exit 1
EOSCRIPT
        chmod +x "$stage/apply.sh"`
	secondRunHarness(t, instanceID, "ghcr.io/x/y@sha256:"+strings.Repeat("5", 64), planStub)

	stdout, code := upRun(t, "", "--runner-id", instanceID, "--json")
	if code == 0 {
		t.Fatalf("rollback-unreachable exited 0:\n%s", stdout)
	}
	lines := nonBlankLines(stdout)
	var ev jsonEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil {
		t.Fatalf("the terminal event did not parse: %v\n%s", err, stdout)
	}
	if ev.Step != "apply" || ev.State != "fail" || ev.Detail != "rollback-unreachable" {
		t.Fatalf("terminal event %+v, want apply/fail/rollback-unreachable", ev)
	}
	if !strings.Contains(ev.Note, saw) || !strings.Contains(ev.Note, advice) {
		t.Errorf("the terminal event does not carry the run's own sentence (note %q)", ev.Note)
	}
}

// TestUp_SecondRun_DetectsExistingInstance proves runUp's own dispatch: given a prior manifest for
// the resolved instance id (read through the REAL internal/router state-dir function, pointed at a
// temp dir via ARGUS_ROUTER_STATE — the same pattern upPrepareHarness already uses), `up` takes the
// second-run branch rather than upFirstRun's onboard.sh exec.
//
// ⛔ NOT VIA A SPY — via the DISTINGUISHING BEHAVIOUR upFirstRun's own header describes: a first run
// looks for ./onboarding/onboard.sh relative to cwd and fails with that specific message when it is
// not there; a second run never touches onboard.sh at all and fails a different way (rendering the
// update block from an incomplete/absent kit directory) once it gets far enough to try.
func TestUp_SecondRun_DetectsExistingInstance(t *testing.T) {
	routerState := t.TempDir()
	t.Setenv("ARGUS_ROUTER_STATE", routerState)

	instanceID := "orders-compose"
	if err := updatecmd.WriteManifest(routerState, updatecmd.Manifest{
		InstanceID: instanceID, Tier: "compose", Version: "0.3.32",
		Executor: "ghcr.io/x/y@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// An empty cwd with no onboarding/onboard.sh: a FIRST run would fail with "could not find
	// onboarding/onboard.sh …". A second run never looks for it at all.
	kit := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(kit); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(prevWD) })

	stdout, code := upRun(t, "", "--runner-id", instanceID)
	if code == 0 {
		t.Fatalf("expected a non-zero exit (the update block cannot run under a stubless docker), got 0:\n%s", stdout)
	}
	if strings.Contains(stdout, "onboarding/onboard.sh") {
		t.Fatalf("runUp took the FIRST-run path (looked for onboard.sh) for an instance with an installed manifest:\n%s", stdout)
	}
}

// secondRunHarness builds the manifest, the hand-written docker stub and the kit/cwd every
// up_second_run.go test in this file needs, and returns the pieces a test still has to fill in: the
// bin dir (so a test can add its own docker stub) and the instance id / image the manifest names.
//
// The docker binary on PATH is a hand-written stub (not tests/harness/bin's generic fixture
// mechanism): it has to WRITE the discover.sh / preflight.sh (/ apply.sh) files RenderBlock's block
// expects to find at a `--stage` path it only learns from argv, which the static .match/.out fixture
// shape cannot express — the same reason apply_exec_test.go/preflight_exec_test.go's own stubBin
// writes a real script instead of a fixture.
func secondRunHarness(t *testing.T, instanceID, image, planStubBody string) {
	t.Helper()
	routerState := t.TempDir()
	t.Setenv("ARGUS_ROUTER_STATE", routerState)

	if err := updatecmd.WriteManifest(routerState, updatecmd.Manifest{
		InstanceID: instanceID, Tier: "compose", Version: "0.3.32", Executor: image,
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	dockerStub := "#!/usr/bin/env bash\n" + `case "$1" in
  pull) exit 0 ;;
  image) echo '` + image + `'; exit 0 ;;
  run)
    stage=""; prev=""
    for a in "$@"; do
      if [ "$prev" = "--stage" ]; then stage="$a"; fi
      prev="$a"
    done
    case "$*" in
      *"update discover"*)
        mkdir -p "$stage"
        cat > "$stage/discover.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf 'wrote %s\n' "$(dirname "$0")/observed.json"
EOSCRIPT
        chmod +x "$stage/discover.sh"
        : > "$stage/observed.json"
        exit 0 ;;
      *"update plan"*)
        mkdir -p "$stage"
` + planStubBody + `
        exit 0 ;;
    esac
    exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(dockerStub), 0o755); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// KitDir needs a parent directory (RenderBlock's own "1-STAGE: the staging directory sits BESIDE
	// the kit" rule) — an ordinary nested temp dir satisfies that.
	kitDir := filepath.Join(t.TempDir(), "kit")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(kitDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(prevWD) })
}

// TestUp_SecondRun_NoOp_EmitsUpToDateAndExitsZero — a GENUINE no-op: preflight SUCCEEDS ("preflight
// ok") and apply itself determines nothing needed to change (its own outcome word, "untouched" — the
// one apply.sh can print, scripts_apply.go:63's A-7 skip). `up` must still exit 0 and emit exactly
// one terminal step, up-to-date/ok.
//
// ⛔ #45 — THIS USED TO SIMULATE THE BUG, NOT THE FIX. The prior version of this test had preflight
// itself print bare `untouched` and exit 1 with NO preflight.json at all — indistinguishable, on
// purpose, from every OTHER preflight refusal (a missing env file, no --kube-context, …), which is
// exactly the defect: a refusal was being read as up-to-date. Genuine up-to-date only ever happens
// AFTER preflight succeeds; TestUp_SecondRun_PreflightRefusal_IsAFailStepWithReason below is the
// refusal case this test no longer conflates with it.
func TestUp_SecondRun_NoOp_EmitsUpToDateAndExitsZero(t *testing.T) {
	instanceID := "orders-compose"
	image := "ghcr.io/x/y@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	planStub := `        cat > "$stage/preflight.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf 'preflight ok\n'
EOSCRIPT
        chmod +x "$stage/preflight.sh"
        cat > "$stage/apply.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf 'untouched\n'
exit 1
EOSCRIPT
        chmod +x "$stage/apply.sh"`
	secondRunHarness(t, instanceID, image, planStub)

	stdout, code := upRun(t, "", "--runner-id", instanceID, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (a no-op second run must still succeed)\nstdout:\n%s", code, stdout)
	}
	lines := nonBlankLines(stdout)
	last := lines[len(lines)-1]
	var ev jsonEvent
	if err := json.Unmarshal([]byte(last), &ev); err != nil {
		t.Fatalf("event did not parse as JSON: %v\nline: %q", err, last)
	}
	if ev.Step != "up-to-date" || ev.State != "ok" {
		t.Fatalf("got step=%q state=%q, want up-to-date/ok:\n%s", ev.Step, ev.State, stdout)
	}
}

// TestUp_SecondRun_PreflightRefusal_IsAFailStepWithReason — #45's actual reproduction: the update
// preflight refuses for a real reason (a missing env file, here) and writes preflight.json with
// "ok":false and that reason under "failed". `up` must report it as {"step":"plan","state":"fail"}
// carrying that reason, and exit non-zero — never up-to-date, never exit 0.
func TestUp_SecondRun_PreflightRefusal_IsAFailStepWithReason(t *testing.T) {
	instanceID := "orders-compose"
	image := "ghcr.io/x/y@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	reason := "no env file for this instance: env." + instanceID
	planStub := fmt.Sprintf(`        cat > "$stage/preflight.sh" <<'EOSCRIPT'
#!/usr/bin/env bash
printf 'untouched\n'
printf '{"instance_id":"%s","image":"%s","ok":false,"failed":"%s"}\n' > "$(dirname "$0")/preflight.json"
exit 1
EOSCRIPT
        chmod +x "$stage/preflight.sh"`, instanceID, image, reason)
	secondRunHarness(t, instanceID, image, planStub)

	stdout, code := upRun(t, "", "--runner-id", instanceID, "--json")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero: a preflight refusal is never a success\nstdout:\n%s", stdout)
	}
	fails := upFailSteps(t, stdout)
	detail, ok := fails["plan"]
	if !ok {
		t.Fatalf("no {\"step\":\"plan\",\"state\":\"fail\"} line on stdout; fail steps seen: %v\n  stdout:\n%s", fails, stdout)
	}
	if detail != reason {
		t.Fatalf("got detail %q, want the preflight.json \"failed\" reason %q\n  stdout:\n%s", detail, reason, stdout)
	}
	for _, l := range nonBlankLines(stdout) {
		var ev jsonEvent
		if err := json.Unmarshal([]byte(l), &ev); err == nil && ev.Step == "up-to-date" {
			t.Fatalf("a genuine refusal was ALSO reported as up-to-date: %s", stdout)
		}
	}
}

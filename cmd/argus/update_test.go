package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// V31-001 (VR13-UP) — `update commit` AND `update report`.
//
// The block an operator pastes runs `update plan` → `preflight.sh` → `apply.sh`, and apply.sh calls
// `update commit` when every step has succeeded. Commit is the moment the machine's record of what
// is installed becomes true again — so its rules are the manifest's rules, applied through a CLI an
// apply script can call without knowing any of them.
//
// ⛔ EVERY EXIT PATH REPORTS (C-14). Three of the four outcomes previously had no path to the
// control plane at all, which is "absence is not health" in its purest form: a rollback, a
// rollback-failed and a PHASE-1 abort all looked, from the page, exactly like an update that never
// started.

func TestUpdateCommit_WritesTheManifestAndCarriesPreviousForward(t *testing.T) {
	state := t.TempDir()

	// onboarding's last step wrote this
	if err := updatecmd.WriteManifest(state, updatecmd.Manifest{
		InstanceID: "i1", Tier: "compose", Version: "0.3.31",
		Executor: "ghcr.io/x@sha256:old",
	}); err != nil {
		t.Fatal(err)
	}

	next := filepath.Join(t.TempDir(), "manifest.next.json")
	writeJSONFile(t, next, updatecmd.Manifest{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		Executor: "ghcr.io/x@sha256:new",
	})

	if code := cmdUpdateCommit(updateArgs{RouterState: state, ManifestPath: next, Outcome: "updated"}); code != exitOK {
		t.Fatalf("update commit exited %d", code)
	}

	got, err := updatecmd.ReadManifest(state, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.3.32" {
		t.Errorf("version = %q, want the one just applied", got.Version)
	}
	if got.Previous == nil || got.Previous.Version != "0.3.31" {
		t.Fatalf("previous must record where the machine came from: %+v", got.Previous)
	}
	if got.LastOutcome == nil || got.LastOutcome.Word != "updated" {
		t.Errorf("last_outcome = %+v", got.LastOutcome)
	}
}

func TestUpdateCommit_ARollbackLeavesPreviousAlone(t *testing.T) {
	state := t.TempDir()
	if err := updatecmd.WriteManifest(state, updatecmd.Manifest{
		InstanceID: "i1", Tier: "compose", Version: "0.3.31", Executor: "ghcr.io/x@sha256:old",
	}); err != nil {
		t.Fatal(err)
	}
	fwd := filepath.Join(t.TempDir(), "next.json")
	writeJSONFile(t, fwd, updatecmd.Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32", Executor: "ghcr.io/x@sha256:new"})
	if code := cmdUpdateCommit(updateArgs{RouterState: state, ManifestPath: fwd, Outcome: "updated"}); code != exitOK {
		t.Fatal("forward commit failed")
	}

	back := filepath.Join(t.TempDir(), "back.json")
	writeJSONFile(t, back, updatecmd.Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31", Executor: "ghcr.io/x@sha256:old"})
	if code := cmdUpdateCommit(updateArgs{RouterState: state, ManifestPath: back, RollbackTo: "0.3.31",
		Outcome: "rolled-back-to"}); code != exitOK {
		t.Fatal("rollback commit failed")
	}

	got, _ := updatecmd.ReadManifest(state, "i1")
	if got.Previous == nil || got.Previous.Version != "0.3.31" {
		t.Fatalf("a rollback must not move previous: %+v", got.Previous)
	}
	if updatecmd.RollbackOffered(got) {
		t.Error("after a rollback the page must offer no further rollback — that is the ping-pong")
	}
}

// ⛔ AC-D49 (#296): `update commit` used to print a bare `"rollback_offered": false` with nothing
// naming why — matching what the reporter measured verbatim on the machine ("rollback_offered": false
// and nothing else, cmd/argus/update.go:197). The CLI must now say the same sentence the page renders
// (updatecmd.RollbackReason), never a bare boolean.
func TestUpdateCommit_EmitsWhyRollbackIsWithheld(t *testing.T) {
	state := t.TempDir()
	// a manifest with a known installed version but NO previous at all — the shape the reporter's own
	// measured JSON had (a committed manifest with no `previous` key whatsoever).
	next := filepath.Join(t.TempDir(), "manifest.next.json")
	writeJSONFile(t, next, updatecmd.Manifest{
		InstanceID: "i1", Tier: "k3d", Version: "0.3.40", Executor: "ghcr.io/x@sha256:new",
	})

	var code int
	out := captureStdout(t, func() {
		code = cmdUpdateCommit(updateArgs{RouterState: state, ManifestPath: next, Outcome: "updated"})
	})
	if code != exitOK {
		t.Fatalf("update commit exited %d:\n%s", code, out)
	}
	var emitted map[string]any
	if err := json.Unmarshal([]byte(out), &emitted); err != nil {
		t.Fatalf("commit did not print JSON: %v\n%s", err, out)
	}
	if emitted["rollback_offered"] != false {
		t.Fatalf("rollback_offered = %v, want false", emitted["rollback_offered"])
	}
	reason, _ := emitted["rollback_offered_reason"].(string)
	if reason == "" {
		t.Fatalf("rollback_offered_reason is empty — a bare boolean with no reason is exactly the defect "+
			"AC-D49 reports (\"rollback_offered\": false and nothing else):\n%s", out)
	}
	if !strings.Contains(reason, "0.3.40") {
		t.Errorf("rollback_offered_reason = %q, want it to name the installed version", reason)
	}
}

// The pair from the issue actually orders once the dev suffix is handled, so it MUST be offered, not
// withheld — this is the positive case: previous is recorded AND ordered AND offered.
func TestUpdateCommit_OffersRollbackFromADevSuffixedPrevious(t *testing.T) {
	state := t.TempDir()
	next := filepath.Join(t.TempDir(), "manifest.next.json")
	writeJSONFile(t, next, updatecmd.Manifest{
		InstanceID: "i1", Tier: "k3d", Version: "0.3.40", Executor: "ghcr.io/x@sha256:new",
		Previous: &updatecmd.Previous{Version: "0.3.37-dev+9816842", Image: "registry.example.com/x@sha256:old"},
	})
	var code int
	out := captureStdout(t, func() {
		code = cmdUpdateCommit(updateArgs{RouterState: state, ManifestPath: next, Outcome: "updated"})
	})
	if code != exitOK {
		t.Fatalf("update commit exited %d:\n%s", code, out)
	}
	var emitted map[string]any
	if err := json.Unmarshal([]byte(out), &emitted); err != nil {
		t.Fatalf("commit did not print JSON: %v\n%s", err, out)
	}
	if emitted["rollback_offered"] != true {
		t.Errorf(`rollback_offered = %v, want true — "0.3.37-dev+9816842" orders below "0.3.40" once the `+
			"dev suffix is handled (AC-D49 / #296):\n%s", emitted["rollback_offered"], out)
	}
	if reason, _ := emitted["rollback_offered_reason"].(string); reason != "" {
		t.Errorf("rollback_offered_reason = %q, want empty when the rollback IS offered", reason)
	}
}

// ⛔ C-15 — on `rollback-failed` the manifest is what is REALLY installed, and the failing step is
// named. That outcome exists because the machine is where NEITHER version says it should be, so an
// operator needs to read, per artefact, what it actually has.
func TestUpdateCommit_RollbackFailedNamesTheStep(t *testing.T) {
	state := t.TempDir()
	if err := updatecmd.WriteManifest(state, updatecmd.Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31"}); err != nil {
		t.Fatal(err)
	}
	rehash := filepath.Join(t.TempDir(), "rehash.json")
	writeJSONFile(t, rehash, updatecmd.Manifest{
		InstanceID: "i1", Tier: "compose", Version: "0.3.31",
		Artefacts: []updatecmd.Artefact{
			{Kind: "executor", Name: "exec", Version: "0.3.32"}, // the half that DID move
			{Kind: "kit", Name: "kit", Version: "0.3.31"},       // the half that did not
		},
	})
	if code := cmdUpdateCommit(updateArgs{RouterState: state, ManifestPath: rehash,
		Outcome: "rollback-failed", FailedStep: "A-3"}); code != exitOK {
		t.Fatalf("commit exited %d", code)
	}
	got, _ := updatecmd.ReadManifest(state, "i1")
	if got.LastOutcome == nil || got.LastOutcome.FailedStep != "A-3" {
		t.Fatalf("the failing step must be named: %+v", got.LastOutcome)
	}
	if len(got.Artefacts) != 2 {
		t.Fatalf("the re-hash must be stored per artefact, so an operator can see the mixed state: %+v", got.Artefacts)
	}
}

func TestUpdateCommit_RefusesAnIncompleteCall(t *testing.T) {
	// A commit that cannot say WHERE or WHAT is not a commit. Failing loudly here is the difference
	// between "the manifest is stale" and "the manifest is wrong", and only one of those is
	// recoverable by reading it.
	for _, c := range []struct {
		name string
		a    updateArgs
	}{
		{"no router state", updateArgs{ManifestPath: "x.json"}},
		{"no manifest", updateArgs{RouterState: t.TempDir()}},
	} {
		if code := cmdUpdateCommit(c.a); code == exitOK {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ⭐ THE VERB IS REACHABLE FROM A REAL `dispatch`, WITH NO TOKEN PAIR IN THE ENVIRONMENT.
//
// This asserts the two things a string-match on main.go cannot: that the flags actually ARRIVE
// (the argv partition declares --router-state, --instance-id and --token itself, so an un-owned
// `update` would see an empty router state — V19-010 exactly), and that the command sits ABOVE the
// M2.5 token gate. The operator pastes this block into their own shell; on compose there is no
// ARGUS_TOKEN to paste, which is precisely how the V28-001 fence check shipped denied on the one
// tier that needed it. The test therefore runs with the environment CLEARED.
func TestUpdate_DispatchReachesCommitPreAuthWithItsFlagsIntact(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	// ⛔ ANTI-VACUITY. If the token gate were not live in this process, "update is pre-auth" would
	// pass without meaning anything. Prove the gate bites first, on a command that sits BELOW it.
	if code := dispatch([]string{"get-dashboard-url"}); code != exitDenied {
		t.Fatalf("the control command exited %d, want exitDenied (%d) — the token gate is not live "+
			"in this process, so this test cannot prove `update` sits above it", code, exitDenied)
	}

	state := t.TempDir()
	next := filepath.Join(t.TempDir(), "next.json")
	writeJSONFile(t, next, updatecmd.Manifest{InstanceID: "i9", Tier: "compose", Version: "0.3.32"})

	code := dispatch([]string{"update", "commit", "--router-state", state, "--manifest", next, "--outcome", "updated"})
	if code != exitOK {
		t.Fatalf("`argus update commit …` exited %d with no token in the environment.\n"+
			"  exitDenied (%d) means it fell below the M2.5 token gate; exitUsage (%d) means a flag\n"+
			"  the argv partition owns never reached it.", code, exitDenied, exitUsage)
	}
	got, err := updatecmd.ReadManifest(state, "i9")
	if err != nil {
		t.Fatalf("the commit exited 0 but wrote no manifest — --router-state did not arrive: %v", err)
	}
	if got.Version != "0.3.32" {
		t.Errorf("version = %q, want 0.3.32", got.Version)
	}
}

// ⛔ AC-D53 (K-1, BASH-1): `update commit` TAKES `previous` FROM THE MARKER STANDING AT THE COMMIT — where the
// executor was before the move this run made — never from the record under it, which an earlier interrupted attempt
// left stale (here: 0.3.33, the release the operator rolled away from). And run twice, as runner() does after a
// container exit of 125, it records the same.
func TestUpdate_ACD53_TheCommitTakesPreviousFromTheMarkerAndARetryRecordsTheSame(t *testing.T) {
	const img32, img33, img34 = "ghcr.io/x/exec@sha256:v32", "ghcr.io/x/exec@sha256:v33", "ghcr.io/x/exec@sha256:v34"
	state := t.TempDir()
	// the record from before an interrupted rollback to 0.3.32 that landed: stale
	if err := updatecmd.WriteManifest(state, updatecmd.Manifest{InstanceID: "i9", Tier: "compose", Version: "0.3.33",
		Executor: img33, Previous: &updatecmd.Previous{Version: "0.3.32", Image: img32}}); err != nil {
		t.Fatal(err)
	}
	// this run's own marker: it moved the executor from 0.3.32 (its plan read it there) to 0.3.34
	if err := os.WriteFile(filepath.Join(state, "installed", "i9.unconfirmed"),
		[]byte("image="+img34+"\nversion=0.3.34\nfrom_version=0.3.32\nfrom_image="+img32+"\nat=2026-09-30T17:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(t.TempDir(), "next.json")
	writeJSONFile(t, next, updatecmd.Manifest{InstanceID: "i9", Tier: "compose", Version: "0.3.34", Executor: img34})

	for attempt := 1; attempt <= 2; attempt++ {
		if code := dispatch([]string{"update", "commit", "--router-state", state, "--manifest", next, "--outcome", "updated"}); code != exitOK {
			t.Fatalf("attempt %d: `update commit` exited %d", attempt, code)
		}
		got, err := updatecmd.ReadManifest(state, "i9")
		if err != nil {
			t.Fatal(err)
		}
		if got.Previous == nil || got.Previous.Version != "0.3.32" || got.Previous.Image != img32 {
			t.Errorf("attempt %d: previous is %+v — want 0.3.32, where the executor was before this update", attempt, got.Previous)
		}
	}
}

// ⛔ NO VERB EXITS 0 HAVING DONE NOTHING. That is the failure this row exists to remove: an update
// block that runs to completion, prints no error, and leaves the machine exactly as it was.
//
// A verb called without what it needs refuses with usage; an unknown verb is refused BY NAME rather
// than ignored, because a typo that silently does nothing is the same defect wearing a different hat.
func TestUpdate_NoVerbSucceedsWithoutDoingTheWork(t *testing.T) {
	for _, verb := range []string{"discover", "plan", "commit", "report", "wibble", ""} {
		args := []string{"update"}
		if verb != "" {
			args = append(args, verb)
		}
		if code := dispatch(args); code == exitOK {
			t.Errorf("`argus update %s` (with nothing else) exited 0 — an update block must not run "+
				"to completion having done nothing", verb)
		}
	}
}

// ⭐ PHASE 1 END TO END, THROUGH THE CLI: discover writes a script, plan turns an observation into the
// three artefacts, and apply.sh is the LAST of them (C-6).
func TestUpdatePhase1_DiscoverThenPlanWritesTheThreeArtefacts(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	root := t.TempDir()
	kit := filepath.Join(root, "kit")
	stage := filepath.Join(root, "stage")
	state := filepath.Join(root, "state")
	// The image's bundle: `update plan` stages it into $STAGE/kit, which is what A-1 swaps in. It is
	// part of the normal path, not an extra — see TestUpdatePlan_StagesTheKitAndRefusesWithoutABundle
	// for why an absent one refuses rather than landing an empty kit.
	bundle := filepath.Join(root, "bundle")
	for _, d := range []string{kit, state, filepath.Join(kit, "deploy", "compose"), filepath.Join(bundle, "onboarding")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "onboarding", "onboard.sh"), []byte("#!/usr/bin/env bash"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_BUNDLE_DIR", bundle)
	if err := os.WriteFile(filepath.Join(kit, "deploy", "compose", "env.i1"), []byte("ARGUS_MCP_PORT=8765\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := dispatch([]string{"update", "discover", "--stage", stage, "--kit", kit,
		"--instance-id", "i1", "--tier", "compose", "--router-state", state}); code != exitOK {
		t.Fatalf("update discover exited %d", code)
	}
	sh, err := os.ReadFile(filepath.Join(stage, "discover.sh"))
	if err != nil {
		t.Fatalf("discover.sh was not written: %v", err)
	}
	// ⛔ it must carry the kit as a LITERAL, not as the operator's shell variable (SA-D27).
	// in the spelling bash opens — the MSYS form on Windows, the path itself on POSIX
	if !strings.Contains(string(sh), updatecmd.MSYSPath(kit)) {
		t.Error("discover.sh does not carry the kit directory as a literal")
	}
	if strings.Contains(string(sh), "$ARGUS_KIT_DIR_HOST") {
		t.Error("discover.sh reads the operator's shell for the kit — a block that acts on whatever " +
			"happens to be set is VR6-S1")
	}

	// the observation the host script would have produced
	obs := filepath.Join(stage, "observed.json")
	if err := os.WriteFile(obs, []byte(`{"tier":"compose","compose":{"project":"argus-i1",`+
		`"services":[{"name":"argus-executor-i1","image":"ghcr.io/x/exec","digest":"sha256:old"}]},`+
		`"env":{"ARGUS_MCP_PORT":"8765","ARGUS_RUNNER_TOKEN":""},"folders":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := dispatch([]string{"update", "plan", "--stage", stage, "--kit", kit, "--instance-id", "i1",
		"--tier", "compose", "--observed", obs, "--router-state", state,
		"--image", "ghcr.io/x/exec:0.3.32", "--image-digest", "ghcr.io/x/exec@sha256:new",
		"--version", "0.3.32"}); code != exitOK {
		t.Fatalf("update plan exited %d", code)
	}
	for _, f := range []string{"plan.json", "preflight.sh", "apply.sh"} {
		if _, err := os.Stat(filepath.Join(stage, f)); err != nil {
			t.Errorf("%s was not written: %v", f, err)
		}
	}
}

// ⭐ `update plan` STAGES THE KIT, AND REFUSES WHEN IT CANNOT.
//
// A-1's first act is `mv "$STAGE/kit" "$KIT"`. Without a staged kit the apply dies on its first line
// having ALREADY renamed the operator's kit away — before any step could register itself for undo, so
// the trap has nothing to put back. That is the one failure the whole compensation design exists to
// prevent, and it would be reached by the happy path.
func TestUpdatePlan_StagesTheKitAndRefusesWithoutABundle(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	root := t.TempDir()
	kit := filepath.Join(root, "kit")
	stage := filepath.Join(root, "stage")
	bundle := filepath.Join(root, "bundle")
	for _, d := range []string{kit, stage, filepath.Join(bundle, "onboarding")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	obs := filepath.Join(stage, "observed.json")
	if err := os.WriteFile(obs, []byte(`{"tier":"compose","env":{"ARGUS_MCP_PORT":"8765"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"update", "plan", "--stage", stage, "--kit", kit, "--instance-id", "i1",
		"--tier", "compose", "--observed", obs, "--image-digest", "ghcr.io/x/exec@sha256:new",
		"--version", "0.3.32"}

	// ⛔ NO BUNDLE → REFUSE. An empty staged kit is worse than no plan: the apply would land it.
	t.Setenv("ARGUS_BUNDLE_DIR", filepath.Join(root, "does-not-exist"))
	if code := dispatch(args); code == exitOK {
		t.Fatal("planned with no onboarding bundle — the apply would swap an EMPTY kit into place")
	}
	if _, err := os.Stat(filepath.Join(stage, "apply.sh")); !os.IsNotExist(err) {
		t.Fatal("a refused plan left a runnable apply.sh behind")
	}

	// with a bundle, the kit is staged where A-1 looks for it
	if err := os.WriteFile(filepath.Join(bundle, "onboarding", "onboard.sh"), []byte("#!/usr/bin/env bash"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_BUNDLE_DIR", bundle)
	if code := dispatch(args); code != exitOK {
		t.Fatalf("update plan exited %d with a bundle present", code)
	}
	if _, err := os.Stat(filepath.Join(stage, "kit", "onboarding", "onboard.sh")); err != nil {
		t.Fatalf("the kit was not staged at $STAGE/kit, which is exactly where A-1 looks: %v", err)
	}
}

// ⛔ `update plan` WITHOUT AN OBSERVATION IS A HARD STOP, and it must not leave a runnable apply.sh
// behind — the operator's next pasted line is `bash apply.sh`, and a half-plan that runs is worse
// than no plan at all (C-6).
func TestUpdatePlan_ARefusedPlanLeavesNothingRunnable(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	root := t.TempDir()
	stage := filepath.Join(root, "stage")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	obs := filepath.Join(stage, "observed.json")
	if err := os.WriteFile(obs, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dispatch([]string{"update", "plan", "--stage", stage, "--kit", root, "--instance-id", "i1",
		"--tier", "compose", "--observed", obs, "--image-digest", "ghcr.io/x/exec@sha256:new"}); code == exitOK {
		t.Fatal("an unreadable observation was planned from — it would describe a bare machine")
	}
	if _, err := os.Stat(filepath.Join(stage, "apply.sh")); !os.IsNotExist(err) {
		t.Fatal("a refused plan left a runnable apply.sh behind")
	}
}

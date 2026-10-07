package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// up_second_run.go — THE SECOND-RUN HANDOFF.
//
// `argus up` against an instance that already has an installed manifest is not a fresh onboard: it
// is the SAME plan/apply update the control plane's dashboard renders as a block an operator pastes
// (internal/updatecmd.RenderBlock). This file execs that exact block VERBATIM — never a hand-assembled
// call into cmdUpdateDiscover/cmdUpdatePlan/etc. — because the whole point is byte-identical reuse of
// what RenderBlock's own tests (internal/updatecmd/renderblock_gate2_test.go and friends) already
// cover. See RenderBlock's own doc comment for why there is exactly one renderer.

// secondRunInstance builds the updatecmd.Instance RenderBlock renders from, out of `up`'s own flags
// and the installed manifest runUp already read. Named and package-level (rather than inlined into
// upSecondRun) so a test can call it directly and prove it never drifts from a hand-built Instance —
// up_update_handoff_test.go's TestUp_SecondRun_BuildsInstanceAndCallsRenderBlock.
//
// ⛔ Rollback/RollbackToVersion/TargetImage are left at their zero values. `up` exposes no --rollback
// flag, and RenderBlock's own branch logic renders the forward-update path whenever they are unset.
func secondRunInstance(a upArgs, m updatecmd.Manifest, kitDir string) updatecmd.Instance {
	// a blank --image names no image — the same reading as upArgs.ImageGiven
	image := strings.TrimSpace(a.Image)
	if image == "" {
		// No --image (and no ARGUS_MCP_IMAGE): fall back to the image already installed, so a bare
		// `argus up` re-run still checks THAT reference for a newer digest rather than refusing outright.
		image = m.Executor
	}
	if image == "" && m.Unconfirmed != nil {
		// ⛔ AC-D53: AN UNCONFIRMED EXECUTOR IS RECORDED WITH NO IMAGE — unknown, on purpose — so the fallback above
		// is empty. The not-confirmed marker names the image that update was moving to: re-running THAT update is
		// the intended next step (the page block's NOTE), and it is what a bare `argus up` now does.
		image = m.Unconfirmed.Image
	}
	return updatecmd.Instance{
		Tier:        m.Tier,
		InstanceID:  m.InstanceID,
		Image:       image,
		KitDir:      kitDir,
		KubeContext: a.KubeContext,
		Kubeconfig:  a.Kubeconfig,
	}
}

// untouchedMarker is apply.sh's own OUTCOME word (scripts_apply.go:63, an A-7 skip) when a
// successful preflight is followed by an apply that finds nothing left to change.
//
// ⛔ #45 — NOT PREFLIGHT'S "untouched" LINE. internal/updatecmd/scripts.go's preflight fail() ALSO
// prints this literal word, for EVERY refusal it has (a missing env file, no --kube-context, docker
// not answering, …) — it is not a signal that nothing changed, only that preflight stopped before
// apply ever ran. The two are told apart by "preflight ok": present only when preflight itself
// succeeded, which is the one precondition a genuine no-op requires. See secondRunEvents.
const untouchedMarker = "untouched"

// upToDateStep is the --json step name `up` emits for the no-op case. It names no onboard.sh step
// (there is none for a second run) and no update A-step id (the block never reached apply.sh).
const upToDateStep = "up-to-date"

// upSecondRun renders and runs the update block for an already-onboarded instance.
func upSecondRun(a upArgs, m updatecmd.Manifest) int {
	stdout := io.Writer(os.Stdout)

	kitDir, err := os.Getwd()
	if err != nil {
		return failUp(stdout, a.JSON, "update", fmt.Sprintf("could not resolve the kit directory: %v", err))
	}

	if u := m.Unconfirmed; !a.ImageGiven && m.Executor == "" && u != nil && u.RollbackTo != "" {
		// ⛔ AC-D53: THE UNCONFIRMED MOVE WAS A ROLLBACK, AND `up` CANNOT RE-RUN ONE. Falling back to the marker's image
		// renders the FORWARD block for the older image: its plan runs inside that image, the outcome is `updated`
		// for a rollback, `previous` moves, and A-6 refreshes the older bundle's scenarios. The rollback is re-run
		// the way it was run — its own block — and a forward update is the operator's to choose, with --image typed
		// on the command line: an exported ARGUS_MCP_IMAGE is set for onboarding, not a choice (operator, 2026-09-30).
		return failUp(stdout, a.JSON, "update", fmt.Sprintf(
			"the last move of %s's executor was a rollback to %s (%s) that could not be confirmed, and `argus up` has "+
				"no rollback flag, so it will not re-run it as a forward update. Re-run the same rollback block you "+
				"pasted, or pass --image with an image on the command line to update forward instead (an empty "+
				"--image, or an exported ARGUS_MCP_IMAGE, does not count)", m.InstanceID, u.RollbackTo, u.Image))
	}
	in := secondRunInstance(a, m, kitDir)
	block, _ := updatecmd.RenderBlock(in)
	if block == "" && in.Image == "" && m.LastOutcome != nil && m.Version == "" {
		// the executor is recorded unknown and nothing names an image to re-run: say THAT, not "incomplete"
		return failUp(stdout, a.JSON, "update", fmt.Sprintf(
			"the executor of %s is recorded unknown after its last update (%s), so there is no installed image to "+
				"re-run — pass --image with the image that update was installing", m.InstanceID, m.LastOutcome.Word))
	}
	if block == "" {
		return failUp(stdout, a.JSON, "update", fmt.Sprintf(
			"could not render the update block for %s — its recorded tier, image or kit directory is incomplete "+
				"(run `argus up` from the kit directory, and pass --image if none is installed yet)", m.InstanceID))
	}

	cmd := exec.Command("bash", "-c", block)
	cmd.Env = os.Environ()
	var captured bytes.Buffer
	if a.JSON {
		// JSON MODE: this process's OWN stdout carries only the --json protocol — the block's raw
		// transcript is captured (to detect `untouched` and the terminal outcome word) but never
		// written to the real stdout, mirroring upFirstRun's JSON-mode contract for onboard.sh.
		cmd.Stdout = &captured
	} else {
		// PLAIN MODE: the block's own transcript passes through unchanged, exactly like upFirstRun
		// does for onboard.sh — captured ALSO, only so this function can still tell `untouched` apart
		// from a real failure and choose its own exit code.
		cmd.Stdout = io.MultiWriter(os.Stdout, &captured)
	}
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	out := captured.String()

	events, code := secondRunEvents(out, runErr, stageDirFor(kitDir, m.InstanceID), m.InstanceID, in.Image)
	if a.JSON {
		for _, ev := range events {
			_ = emitStep(stdout, ev)
		}
	} else if len(events) > 0 {
		last := events[len(events)-1]
		fmt.Fprintf(stdout, "up: %s: %s: %s\n", last.Step, last.State, last.Detail)
	}
	return code
}

// stageDirFor derives $STAGE the same way updatecmd.RenderBlock does — "the staging directory sits
// BESIDE the kit" (updatecmd.go's own comment) — reusing its ONE exported name for the prefix
// (updatecmd.StagePrefix) rather than restating the rule. Safe to compute independently here (unlike
// the progress.json case secondRunEvents' doc comment warns against): this is a local file read on
// the SAME machine, in the SAME process's own path spelling, never rendered into a script another
// shell or OS has to open — none of MixedPath/MSYSPath's cross-shell concerns apply.
func stageDirFor(kitDir, instanceID string) string {
	return filepath.Join(filepath.Dir(kitDir), updatecmd.StagePrefix+instanceID)
}

// preflightRefusalReason reads $STAGE/preflight.json's own "failed" field — the ONLY place a
// refusal's real reason lives. The stdout literal "untouched" preflight's fail() also prints
// (scripts.go:170-185) carries no reason at all, and is shared with every OTHER refusal it has.
func preflightRefusalReason(stageDir string) string {
	b, err := os.ReadFile(filepath.Join(stageDir, "preflight.json"))
	if err != nil {
		return fmt.Sprintf("the update preflight refused, but its own preflight.json could not be read: %v", err)
	}
	var pf struct {
		OK     bool   `json:"ok"`
		Failed string `json:"failed"`
	}
	if err := json.Unmarshal(b, &pf); err != nil {
		return fmt.Sprintf("the update preflight refused, but its own preflight.json could not be parsed: %v", err)
	}
	if pf.Failed == "" {
		return "the update preflight refused for an unstated reason"
	}
	return pf.Failed
}

// secondRunEvents translates a completed block run into the --json protocol's coarse
// discover/plan/apply events, and this process's own exit code.
//
// ⚠ COARSE, ON PURPOSE. The block (updatecmd.RenderBlock) is ONE bash invocation, not something this
// file may split into phases without editing RenderBlock itself — out of scope here (see the package
// doc comment). What CAN be read, without guessing, are markers already relied on elsewhere in this
// codebase to prove what a run did: discover.sh's own `wrote <path>` line (internal/updatecmd/
// discover.go), preflight's own `preflight ok` line (internal/updatecmd/scripts.go's
// preflightCapture) and its preflight.json (read only on refusal, above), and apply.sh's finish()
// printing the outcome word as the LAST line on every exit path (internal/updatecmd/
// scripts_apply.go's renderFinish — the exact value internal/updatecmd/apply_exec_test.go's own
// lastWord helper reads).
//
// ⛔ #45 — UP-TO-DATE IS AN APPLY OUTCOME, NEVER A PREFLIGHT REFUSAL. preflight's own "untouched" line
// (untouchedMarker's doc comment) means only "preflight stopped before apply ran" — for a missing env
// file exactly as much as for nothing having changed. Genuinely nothing to change is apply's OWN
// outcome word, reachable only once preflight has already succeeded ("preflight ok" seen below).
//
// $STAGE/progress.json is still NOT tailed here: reading it would mean re-deriving the exact
// stage-directory path a SECOND time for a purpose stageDirFor does not already serve (per-A-step
// granularity) — the "two implementations of one rule" drift updatecmd's own package doc warns
// against. The markers above need no such derivation, so progress.json tailing stays out of scope.
func secondRunEvents(out string, runErr error, stageDir, instanceID, image string) (events []jsonEvent, code int) {
	if !strings.Contains(out, "wrote ") {
		return []jsonEvent{
			{Step: "discover", State: "start"},
			{Step: "discover", State: "fail", Detail: lastNonBlankLine(out)},
		}, exitCodeOf(runErr)
	}
	events = append(events, jsonEvent{Step: "discover", State: "start"}, jsonEvent{Step: "discover", State: "ok"})

	if !strings.Contains(out, "preflight ok") {
		// A REFUSAL, NEVER UP-TO-DATE (#45): reported as a fail step carrying preflight.json's own
		// reason, with a non-zero exit — regardless of whatever the block's own exit code happened to
		// be (preflight's fail() always exits 1, but a caller must not depend on that).
		events = append(events,
			jsonEvent{Step: "plan", State: "start"},
			jsonEvent{Step: "plan", State: "fail", Detail: preflightRefusalReason(stageDir)})
		code = exitCodeOf(runErr)
		if code == exitOK {
			code = exitErr
		}
		return events, code
	}
	events = append(events, jsonEvent{Step: "plan", State: "start"}, jsonEvent{Step: "plan", State: "ok"})

	events = append(events, jsonEvent{Step: "apply", State: "start"})
	word := lastNonBlankLine(out)
	switch {
	case word == "updated" || strings.HasPrefix(word, "rolled-back-to "):
		// mirrors scripts_apply.go renderFinish's own case: updated|rolled-back-to* -> exit 0
		events = append(events, jsonEvent{Step: "apply", State: "ok", Detail: word})
		return events, exitOK
	case word == untouchedMarker:
		// THE GENUINE NO-OP (#45): preflight succeeded and apply itself found nothing to change.
		events = append(events, jsonEvent{Step: upToDateStep, State: "ok",
			Detail: fmt.Sprintf("%s already matches %s — nothing to change", instanceID, image)})
		return events, exitOK
	case updatecmd.OutcomeUnrecorded(word):
		// AC-D58 (#392): the machine was changed and its record was not written — never a success, whatever the
		// block's own exit code was, and the detail says why the word is not `updated`. Merged with AC-D53
		//: the word also covers a commit docker exited 125 around, which may have written it.
		events = append(events, jsonEvent{Step: "apply", State: "fail", Detail: word + " — the update changed this machine but was not recorded (or, after a docker exit 125, not confirmed recorded: read the record before re-running)"})
		return events, exitErr
	case word == "rolled-back" || word == "rollback-failed":
		events = append(events, jsonEvent{Step: "apply", State: "fail", Detail: word})
		return events, exitErr
	default:
		ev := jsonEvent{Step: "apply", State: "fail", Detail: word}
		if word == "rollback-unreachable" {
			// ⛔ AC-D53: what the next step is depends on what that run SAW — who stopped answering, whether the executor
			// was put back — and the block printed it to stderr, which --json never carried. apply.sh's rollback()
			// writes it to $STAGE/outcome-note: line 1 what it saw, line 2 its advice.
			ev.Note = outcomeNote(stageDir)
		}
		events = append(events, ev)
		return events, exitCodeOf(runErr)
	}
}

// outcomeNote reads apply.sh's $STAGE/outcome-note as one line. "" when there is none.
func outcomeNote(stageDir string) string {
	b, err := os.ReadFile(filepath.Join(stageDir, "outcome-note"))
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

// lastNonBlankLine is renderFinish's own contract read back: the outcome word is the LAST non-blank
// line finish() prints on every exit path (C-14). Same rule apply_exec_test.go's lastWord asserts on.
func lastNonBlankLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\r\n \t"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

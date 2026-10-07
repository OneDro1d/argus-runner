package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/runner"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// V31-001 (VR13-UP) — THE `update` VERBS.
//
// The block an operator pastes is, in order:
//
//	docker run … update plan   →   bash "$STAGE/preflight.sh"   →   bash "$STAGE/apply.sh"
//
// and apply.sh calls `update commit` once every step has succeeded, or with the outcome that
// actually happened when one did not. These verbs are the machine-side record-keeping: they take
// what apply.sh knows (which step ran, what is really installed now) and turn it into the one
// manifest the page and the next update read.
//
// ⛔ THE SPLIT IS DELIBERATE (C-1). PHASE 1 cannot run wholly inside the container: the image has no
// docker CLI, no kubectl and no compose plugin, so it cannot ask "is the executor healthy", "does
// the compose project exist", "does the cluster answer". The container does what mounts and HTTP
// allow and emits preflight.sh for the HOST to run.
//
// ⛔ AND IT PARSES ITS OWN VECTOR (ownargv.go). Four of its flags are also common-flagset names, so
// the argv partition would take them; `update` reads none of the common flags, so taking one from it
// can only break it. See V19-010.

// updateArgs is what the verbs need, flattened so a test can call them without a CLI.
type updateArgs struct {
	Verb string
	// RouterState is the directory the manifest lives under — computed by the BLOCK, never assumed
	// here (C-5: four branches, ARGUS_ROUTER_STATE first).
	RouterState string
	// ManifestPath is the re-hash apply.sh produced: what is REALLY installed now.
	ManifestPath string
	// RollbackTo is set when this commit is a rollback — the one case that leaves `previous` alone.
	RollbackTo string
	// Outcome is one of the PO's five words; FailedStep names the step on a failure.
	Outcome    string
	FailedStep string
	// CPURL and Token are the control plane and the bearer `plan` uses for the siblings read.
	CPURL string
	Token string
	// Identity is the instance's Ed25519 identity key file (deploy/compose/identity.<id>.key,
	// onboard.sh:1969). `update report` signs with it — the credential /fed/installed already accepts.
	Identity string
	// RunnerImage is the image whose verbs the generated scripts call back into (SA-D19).
	RunnerImage string
	// InstanceID is only needed by `report`, which sends no manifest.
	InstanceID string

	// ── PHASE 1 (discover + plan) ────────────────────────────────────────────────────
	Tier        string
	KitDir      string
	StageDir    string
	KubeContext string
	Kubeconfig  string
	ImageTag    string
	ImageDigest string
	Version     string
	// ObservedPath is what discover.sh wrote. ⛔ A missing or unparseable file is a HARD STOP:
	// planning from an empty reading would install over a live instance as though it were bare.
	ObservedPath string
	// RouterOnly plans the MACHINE ROUTER alone (C-24) — the forward path `update.sh --router` was, for a
	// machine that may have no instance at all. It needs no --instance-id and no --observed.
	RouterOnly bool
	// Bundle is the kit `plan` stages instead of this image's own — the PREVIOUS release's bundle on a
	// rollback, extracted by that image's `init` (the rollback block does it).
	Bundle string
}

// cmdUpdate parses the verb's own argument vector and runs it.
//
// The verb is a POSITIONAL, so `update commit --manifest x` reads the way every other tool in the
// block does. Flags may precede or follow it: the vector is scanned for the first non-flag token
// rather than assuming position 0, because an operator who types `update --router-state X commit`
// has not made a mistake worth an error message.
func cmdUpdate(args []string) int {
	var a updateArgs
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.StringVar(&a.RouterState, "router-state", "", "the router state dir the block computed (C-5)")
	fs.StringVar(&a.ManifestPath, "manifest", "", "commit: the re-hash of what is REALLY installed now")
	fs.StringVar(&a.RollbackTo, "rollback-to", "", "commit: the version a rollback returned to")
	fs.StringVar(&a.Outcome, "outcome", "", "untouched | updated | rolled-back-to | rollback-failed | rollback-unreachable | held-back | unrecorded-updated | unrecorded-rolled-back-to <version>")
	fs.StringVar(&a.FailedStep, "failed-step", "", "the step that failed, on a failure outcome (e.g. A-3)")
	fs.StringVar(&a.CPURL, "control-plane", "", "report: the control plane to tell how the attempt ended")
	fs.StringVar(&a.Token, "token", "", "report: this machine's identity")
	fs.StringVar(&a.InstanceID, "instance-id", "", "report: the instance that was updated")
	fs.StringVar(&a.Tier, "tier", "", "discover|plan: compose | k3d | managed")
	fs.StringVar(&a.KitDir, "kit", "", "discover|plan: the kit directory, as the block computed it")
	fs.StringVar(&a.StageDir, "stage", "", "discover|plan|rehash|report: the staging directory beside the kit")
	fs.StringVar(&a.KubeContext, "kube-context", "", "discover|plan: the cluster context (k8s tiers)")
	fs.StringVar(&a.Kubeconfig, "kubeconfig", "", "discover|plan: the kubeconfig path (k8s tiers)")
	fs.StringVar(&a.ImageTag, "image", "", "plan: the image the block pulled")
	fs.StringVar(&a.ImageDigest, "image-digest", "", "plan: the host-resolved repo@sha256:… reference")
	fs.StringVar(&a.Version, "version", "", "plan: the version being installed")
	fs.StringVar(&a.ObservedPath, "observed", "", "plan: the observed.json discover.sh wrote")
	fs.StringVar(&a.Identity, "identity", "", "report: the instance identity key file the report is signed with")
	fs.StringVar(&a.RunnerImage, "runner-image", "", "plan: the image the scripts call back into (default: --image-digest; a rollback passes the INSTALLED image)")
	fs.BoolVar(&a.RouterOnly, "router-only", false, "plan: move the machine router alone — no instance, no observation (C-24)")
	fs.StringVar(&a.Bundle, "bundle", "", "plan: stage the kit from this bundle instead of this image's (a rollback: the previous image's, from its `init`)")

	var rest []string
	for i := 0; i < len(args); i++ {
		if tok := args[i]; len(tok) > 1 && tok[0] == '-' {
			rest = append(rest, tok)
			// A value-taking flag in `--name value` form owns the next token too. ⛔ A BOOL flag owns
			// nothing: `--router-only --stage X` must not hand `--stage` to --router-only as its value.
			if name, carries := flagToken(tok); !carries && i+1 < len(args) {
				if f := fs.Lookup(name); f != nil && !isBoolFlag(f) {
					i++
					rest = append(rest, args[i])
				}
			}
			continue
		}
		if a.Verb == "" {
			a.Verb = args[i]
			continue
		}
		return emitErr(exitUsage, "update: unexpected argument %q (one verb only)", args[i])
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return emitErr(exitUsage, "update: flag error: %v", err)
	}
	return runUpdate(a)
}

func runUpdate(a updateArgs) int {
	switch a.Verb {
	case "commit":
		return cmdUpdateCommit(a)
	case "report":
		return cmdUpdateReport(a)
	case "started":
		return cmdUpdateStarted(a)
	case "discover":
		return cmdUpdateDiscover(a)
	case "plan":
		return cmdUpdatePlan(a)
	case "rehash":
		return cmdUpdateRehash(a)
	case "":
		return emitErr(exitUsage, "update: name a verb (discover|plan|rehash|commit|report|started)")
	default:
		return emitErr(exitUsage, "unknown update verb %q (want discover|plan|rehash|commit|report|started)", a.Verb)
	}
}

// cmdUpdateCommit writes the machine's manifest after an apply.
//
// ⛔ IT WRITES WHAT APPLY.SH MEASURED, NEVER THE PLAN (C-15). The caller hands it a re-hash of what
// is really installed; on `rollback-failed` especially, the plan describes a state the machine is
// NOT in, and writing it would lay a comfortable fiction over the one outcome that needs the truth.
func cmdUpdateCommit(a updateArgs) int {
	if a.RouterState == "" {
		return emitErr(exitUsage, "update commit: --router-state is required (the block computes it)")
	}
	if a.ManifestPath == "" {
		return emitErr(exitUsage, "update commit: --manifest is required — it is the re-hash of what is really installed")
	}
	b, err := os.ReadFile(a.ManifestPath)
	if err != nil {
		return emitErr(exitErr, "update commit: read %s: %v", a.ManifestPath, err)
	}
	var next updatecmd.Manifest
	if err := json.Unmarshal(b, &next); err != nil {
		return emitErr(exitErr, "update commit: %s is not a readable manifest: %v", a.ManifestPath, err)
	}
	if next.InstanceID == "" {
		return emitErr(exitUsage, "update commit: the manifest names no instance")
	}
	got, err := updatecmd.CommitManifest(a.RouterState, next, updatecmd.CommitOpts{
		RollbackTo: a.RollbackTo,
		Outcome:    a.Outcome,
		FailedStep: a.FailedStep,
	})
	if err != nil {
		return emitErr(exitErr, "update commit: %v", err)
	}
	emit(map[string]any{
		"instance_id":      got.InstanceID,
		"version":          got.Version,
		"outcome":          got.LastOutcome.Word,
		"rollback_offered": updatecmd.RollbackOffered(got),
		// AC-D49 (#296): WHY, when rollback_offered is false — never a bare boolean. "" only when
		// there is truly nothing to add (no manifest recorded at all); every other case names both
		// versions, same sentence the Environments page renders (updatecmd.RollbackReason).
		"rollback_offered_reason": updatecmd.RollbackReason(got),
	})
	return exitOK
}

// cmdUpdateReport tells the control plane how the attempt ended.
//
// ⛔ IT RUNS ON EVERY EXIT PATH (C-14) — success, rollback, rollback-failed and a PHASE-1 abort.
// Three of those four previously had no path to the control plane at all, so from the page they
// looked identical to an update that never started: "absence is not health" in its purest form.
//
// ⛔ AND IT IS BEST-EFFORT, NEVER A ROLLBACK TRIGGER (C-13). A control-plane outage must not undo a
// successful update on the machine — A-8a wrote the local manifest and that is the authority. A
// failure here is reported to the operator and exits 0.
//
// ⭐ IT POSTS THE MANIFEST TO /fed/installed, SIGNED WITH THE INSTANCE IDENTITY (SA §1.3 / C-14).
// The first version POSTed to `/fed/update-report` — a route the control plane has never served — with
// a `--token` the generated scripts never passed, so every report printed `no control plane configured`
// and the page never learned how any update ended. The page reads the outcome from the stored manifest's
// `last_outcome` (instanceview.go:308), so the manifest IS the report.
func cmdUpdateReport(a updateArgs) int {
	if a.Outcome == "" {
		return emitErr(exitUsage, "update report: --outcome is required")
	}
	if a.InstanceID == "" {
		return emitErr(exitUsage, "update report: --instance-id is required")
	}
	notSent := func(reason string) int {
		// ⛔ NEVER A FAILURE OF THE UPDATE (C-13): the machine's own manifest is the authority.
		emit(map[string]any{"reported": false, "reason": reason, "outcome": a.Outcome})
		return exitOK
	}
	if a.CPURL == "" {
		return notSent("no control plane configured")
	}
	if a.Identity == "" {
		return notSent("no --identity key to sign the report with")
	}
	priv, err := runner.LoadKey(a.Identity)
	if err != nil {
		return notSent(fmt.Sprintf("the instance identity could not be read: %v", err))
	}

	// What the machine records after A-8a; on a preflight abort there may be none, which is the
	// honest state of an instance onboarded before 0.3.32.
	m, err := updatecmd.ReadManifest(a.RouterState, a.InstanceID)
	if err != nil || m.InstanceID == "" {
		m = updatecmd.Manifest{InstanceID: a.InstanceID, Tier: a.Tier}
	}
	// ⛔ `untouched` NEVER REACHES A-8a, so the manifest on disk still describes the PREVIOUS attempt.
	// The outcome of THIS attempt is what is reported, whatever the record says.
	// AC-D53: AND WITH A STAGE, THE WHOLE OUTCOME IS THIS ATTEMPT'S — its time too. The stage is recreated on every
	// paste, so it is always this attempt's; a record that merely ends the same way is an EARLIER attempt's when this
	// attempt's A-8a did not commit (the runtime down), and its time went out beside this attempt's verdict as
	// last_update.at. A record that IS this attempt's loses nothing: its time is the commit's, seconds earlier.
	if a.StageDir != "" || m.LastOutcome == nil || m.LastOutcome.Word != a.Outcome || m.LastOutcome.FailedStep != a.FailedStep {
		m.LastOutcome = &updatecmd.LastOutcome{Word: a.Outcome, At: time.Now().UTC().Format(time.RFC3339), FailedStep: a.FailedStep}
	}
	// design step 8: HOW this attempt's A-3 failed its health check comes from this attempt's OWN progress.json
	// whenever there is a stage. ⛔ Never from a record that merely ends the same way: its word and step can match
	// while its verdict is another attempt's.
	if a.StageDir != "" {
		m.LastOutcome.Health, m.LastOutcome.Detail = readProgressHealth(filepath.Join(a.StageDir, "progress.json"), "A-3")
	}
	body, _ := json.Marshal(m)

	tok, err := federation.MintJWT(priv, a.InstanceID, time.Now())
	if err != nil {
		return notSent(fmt.Sprintf("could not sign the report: %v", err))
	}
	req, err := http.NewRequest("POST", strings.TrimRight(a.CPURL, "/")+"/fed/installed", bytes.NewReader(body))
	if err != nil {
		return notSent(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return notSent("the control plane did not answer")
	}
	defer resp.Body.Close()
	emit(map[string]any{"reported": resp.StatusCode < 300, "status": resp.StatusCode, "outcome": a.Outcome})
	return exitOK
}

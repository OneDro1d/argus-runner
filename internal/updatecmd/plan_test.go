package updatecmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// V31-001 (VR13-UP) — THE PLANNER.
//
// `update plan` runs INSIDE the container, which has no docker CLI, no kubectl and no compose plugin
// (Dockerfile.onedroid-argus-execution-plane:88). It cannot touch the machine, so what it produces is two
// bash scripts the HOST runs: `preflight.sh` (the producer of `untouched`) and `apply.sh` (the
// producer of `updated` / `rolled-back` / `rollback-failed`). That split is C-1, and it is why every
// test here asserts on GENERATED TEXT rather than on an effect.
//
// The planner is otherwise PURE: (observed, manifest, target) → plan. Purity is what makes these
// assertions worth making — the same inputs must yield the same scripts on any machine.

func fixtureObserved(tier string) Observed {
	o := Observed{
		Tier:    tier,
		Healthz: "ok",
		Env: map[string]string{
			"ARGUS_MCP_PORT":         "8765",
			"ARGUS_KIT_DIR_HOST":     "/c/argus-kit",
			"PROMTAIL_CONFIG":        "./promtail-config.i1.yaml",
			"ARGUS_RUNNER_TOKEN":     "", // ⛔ present as a NAME, never a value (SA-D18)
			"ARGUS_AUTHOR_TOKEN":     "",
			"ARGUS_IDENTITY_KEY_B64": "",
		},
		Folders: []Folder{{Path: "/c/agents/product", Hat: "product", Writable: true}},
		Grafana: GrafanaState{Reachable: true, Dashboard: DocState{Present: true, UID: "argus-overview-i1"},
			Datasource: DocState{Present: true, UID: "loki-i1"}},
	}
	switch tier {
	case "compose":
		// the names discover.sh reads off the compose LABELS of project argus-inst-<id>
		o.Compose = ComposeState{Project: "argus-inst-i1", Services: []ServiceState{
			{Name: "executor", Image: "ghcr.io/x/exec", Digest: "sha256:old"},
			{Name: "loki", Image: "grafana/loki", Digest: "sha256:lokiold"},
			{Name: "promtail", Image: "grafana/promtail", Digest: "sha256:ptold"},
			{Name: "pushgateway", Image: "prom/pushgateway", Digest: "sha256:pgold"},
		}}
	default:
		o.K8s = K8sState{Context: "k3d-memstore", Namespace: "argus-inst-i1", Objects: []ObjectState{
			{Kind: "Deployment", Name: "executor", Image: "ghcr.io/x/exec", Digest: "sha256:old"},
		}}
	}
	return o
}

func fixtureInput(tier string) PlanInput {
	return PlanInput{
		InstanceID:  "i1",
		Tier:        tier,
		Image:       "ghcr.io/x/exec:0.3.32",
		ImageDigest: "ghcr.io/x/exec@sha256:new",
		Version:     "0.3.32",
		KitDir:      "/c/argus-kit",
		StageDir:    "/c/argus-kit.update-stage",
		RouterState: "/c/state",
		CPURL:       "https://cp.example",
		Observed:    fixtureObserved(tier),
		Installed:   Manifest{InstanceID: "i1", Tier: tier, Version: "0.3.31"},
		Now:         "2026-09-13T00:00:00Z",
	}
}

// ⛔ THE DIGEST REFUSAL. The block resolves the digest host-side and passes it in; an empty or
// unpinned value means the pull did not resolve, and planning against a floating tag is how an
// update silently applies a different image than the one preflight checked.
func TestPlan_RefusesAnUnpinnedImageDigest(t *testing.T) {
	for _, bad := range []string{"", "ghcr.io/x/exec:0.3.32", "sha256:new", "ghcr.io/x/exec@md5:new"} {
		in := fixtureInput("compose")
		in.ImageDigest = bad
		if _, err := BuildPlan(in); err == nil {
			t.Errorf("--image-digest %q was accepted; only a repo@sha256:… reference pins an image", bad)
		}
	}
}

// ⛔ A MISSING observed.json IS A HARD STOP, NEVER AN EMPTY RECONSTRUCTION. An empty Observed would
// plan every step as "nothing is there, install it" — which on a live machine is not an update, it is
// a first onboard performed over the top of one.
func TestPlan_RefusesAnEmptyObservation(t *testing.T) {
	in := fixtureInput("compose")
	in.Observed = Observed{}
	if _, err := BuildPlan(in); err == nil {
		t.Fatal("an empty observed.json was accepted — the planner would treat a live machine as bare")
	}
}

// The step list is the PO's, per tier: A-2 is k8s-only, A-3a is compose-only, and every step that
// runs carries an undo.
func TestPlan_TheStepsAreTheTiersAndEachCarriesAnUndo(t *testing.T) {
	for _, c := range []struct {
		tier                    string
		wantPresent, wantAbsent []string
	}{
		{"compose", []string{"A-1", "A-3", "A-3a", "A-4", "A-5", "A-5a", "A-6", "A-7", "A-8a", "A-8b"}, []string{"A-2"}},
		{"k3d", []string{"A-1", "A-2", "A-3", "A-4", "A-5", "A-5a", "A-6", "A-7", "A-8a", "A-8b"}, []string{"A-3a"}},
		{"managed", []string{"A-1", "A-2", "A-3", "A-4"}, []string{"A-3a"}},
	} {
		p, err := BuildPlan(fixtureInput(c.tier))
		if err != nil {
			t.Fatalf("%s: %v", c.tier, err)
		}
		got := map[string]Step{}
		for _, s := range p.Steps {
			got[s.ID] = s
		}
		for _, id := range c.wantPresent {
			s, ok := got[id]
			if !ok {
				t.Errorf("%s: step %s is missing", c.tier, id)
				continue
			}
			if s.Action == "" {
				t.Errorf("%s: step %s has no action", c.tier, id)
			}
			// A-8a/A-8b are the record-keeping pair and are OUTSIDE the trap (C-13), so they carry
			// no undo. Everything the trap covers must have one.
			if s.Undo == "" && !strings.HasPrefix(id, "A-8") {
				t.Errorf("%s: step %s has no undo — the trap could not put it back", c.tier, id)
			}
		}
		for _, id := range c.wantAbsent {
			if _, ok := got[id]; ok {
				t.Errorf("%s: step %s must not be planned on this tier", c.tier, id)
			}
		}
	}
}

// ⭐ C-13 — THE TRAP COVERS A-1..A-7 AND STOPS THERE.
//
// A-8a wrote the local manifest and that is the authority; A-8b only tells the control plane. If the
// trap covered them, a control-plane outage would UNDO a successful update on the machine — the
// failure mode this constraint exists to forbid.
func TestApplyScript_TheTrapCoversA1ThroughA7AndNotTheReport(t *testing.T) {
	p, err := BuildPlan(fixtureInput("compose"))
	if err != nil {
		t.Fatal(err)
	}
	sh := RenderApply(p, fixtureInput("compose"))

	// The trapped region runs from where the ERR trap is ARMED to where it is DISARMED.
	// ⚠ `trap - ERR` also appears inside rollback() itself (so a failing undo does not re-enter the
	// trap), so the disarm before A-8a is the LAST one — taking the first would make this test
	// assert on an empty region and pass no matter what the script said.
	arm := strings.Index(sh, "trap 'FAILED_STEP=")
	disarm := strings.LastIndex(sh, "trap - ERR")
	if arm < 0 || disarm <= arm {
		t.Fatalf("apply.sh does not arm then disarm the ERR trap (arm=%d disarm=%d) — without the "+
			"disarm, A-8b's failure would roll the machine back (C-13)", arm, disarm)
	}
	trapped, after := sh[arm:disarm], sh[disarm:]

	registers := func(region, id string) bool {
		return regexp.MustCompile(`APPLIED\+=\(['"]?` + regexp.QuoteMeta(id) + `['"]?\)`).MatchString(region)
	}
	// Derived from the plan, not listed: a step that RUNS registers inside the trap; a step the plan
	// skips must NOT register — the trap would "undo" something that never happened.
	ran := 0
	for _, s := range p.Steps {
		if strings.HasPrefix(s.ID, "A-8") {
			continue
		}
		switch {
		case s.SkipReason == "" && !registers(trapped, s.ID):
			t.Errorf("step %s runs but does not register itself for undo inside the trapped region", s.ID)
		case s.SkipReason != "" && registers(trapped, s.ID):
			t.Errorf("step %s is skipped (%s) but registers for undo", s.ID, s.SkipReason)
		}
		if s.SkipReason == "" {
			ran++
		}
	}
	if ran < 4 {
		t.Fatalf("only %d steps run in the compose fixture — this test would prove little", ran)
	}
	for _, id := range []string{"A-8a", "A-8b"} {
		if registers(trapped, id) {
			t.Errorf("step %s is inside the trap — a failed report would undo a successful update (C-13)", id)
		}
	}
	// ⭐ C-14 — THE REPORT RUNS ON EVERY EXIT PATH, AND IT RUNS DISARMED.
	//
	// It lives in finish(), which the success path calls after `trap - ERR` and which the trap
	// handler calls after rolling back. Written as straight-line code at the end it ran ONLY on
	// success: under `set -e` the shell exits as soon as the ERR handler returns, so a rollback
	// printed no outcome and told the control plane nothing. The execution matrix in
	// apply_exec_test.go is what proves the call actually happens; this asserts the structure that
	// makes it possible.
	if !strings.Contains(after, "finish") {
		t.Error("the success path does not call finish() after disarming the trap (C-14)")
	}
	if !strings.Contains(sh, "rollback; finish") {
		t.Error("the ERR handler rolls back without finishing — under `set -e` the shell exits the " +
			"moment the handler returns, so the outcome would never be printed or reported (C-14)")
	}
	fin := sh[strings.Index(sh, "finish() {"):]
	disarmInFinish := strings.Index(fin, "trap - ERR")
	reportInFinish := strings.Index(fin, `report "$OUTCOME"`)
	if disarmInFinish < 0 || reportInFinish < 0 || disarmInFinish > reportInFinish {
		t.Error("finish() reports before disarming the trap — a failed report could roll the machine back (C-13)")
	}
}

// The undo runs in REVERSE. Undoing A-1 (the kit swap) before A-7 (the router) would leave the router
// pointed at a kit that no longer exists.
func TestApplyScript_UndoRunsInReverse(t *testing.T) {
	p, _ := BuildPlan(fixtureInput("compose"))
	sh := RenderApply(p, fixtureInput("compose"))
	if !regexp.MustCompile(`for \(\( *[a-z]+=\$\{#APPLIED\[@\]\} *- *1; *[a-z]+ *>= *0; *[a-z]+-- *\)\)`).MatchString(sh) {
		t.Fatalf("the rollback loop does not walk APPLIED backwards — undoing A-1 before A-7 leaves the\n"+
			"router pointed at a kit that is gone. Script:\n%s", sh)
	}
}

// ⛔ apply.sh REFUSES a preflight it does not recognise. The plan's identity is
// {instance_id, image_digest, generated_at}: a preflight.json from an earlier plan checked different
// preconditions, and acting on it is acting on a stale reading of the machine.
func TestApplyScript_RefusesAPreflightThatIsNotThisPlans(t *testing.T) {
	in := fixtureInput("compose")
	p, _ := BuildPlan(in)
	sh := RenderApply(p, in)
	for _, must := range []string{p.ImageDigest, p.GeneratedAt, `"ok"`, "preflight.json"} {
		if !strings.Contains(sh, must) {
			t.Errorf("apply.sh does not check %q — it would run on another plan's preflight", must)
		}
	}
}

// Both scripts fail loudly. `set -euo pipefail` is C-6: without -e a failing step is skipped past and
// the trap never fires; without -o pipefail a failure inside a pipe is invisible.
func TestGeneratedScripts_FailLoudly(t *testing.T) {
	in := fixtureInput("compose")
	p, _ := BuildPlan(in)
	for name, sh := range map[string]string{"preflight.sh": RenderPreflight(p, in), "apply.sh": RenderApply(p, in)} {
		if !strings.Contains(sh, "set -euo pipefail") {
			t.Errorf("%s does not `set -euo pipefail` — a failing step would be stepped past", name)
		}
	}
}

// The compose executor moves by REWRITING an indirection, and compose must be told which env file to
// read — without --env-file it auto-loads the kit's committed .env and updates a different image.
func TestApplyScript_ComposeRewritesTheImageIndirectionAndNamesTheEnvFile(t *testing.T) {
	in := fixtureInput("compose")
	p, _ := BuildPlan(in)
	sh := RenderApply(p, in)
	if !strings.Contains(sh, "ARGUS_MCP_IMAGE=") {
		t.Error("A-3 does not rewrite ARGUS_MCP_IMAGE — the compose image is an indirection " +
			"(docker-compose.byo-m3.yml:16) and recreating without rewriting it re-runs the OLD image")
	}
	if !strings.Contains(sh, "--env-file") {
		t.Error("A-3 does not pass --env-file — compose auto-loads the kit's committed .env and the " +
			"per-instance env.<id> is ignored")
	}
}

// ⛔ V28-002 — `docker save <digest-ref> | ctr import -` exits 0 and imports NOTHING. The library
// call is the only import that works, and it must happen BEFORE the deployment is pointed at the tag.
func TestApplyScript_K3dImportsThroughTheLibraryBeforeSettingTheImage(t *testing.T) {
	in := fixtureInput("k3d")
	p, _ := BuildPlan(in)
	sh := RenderApply(p, in)
	imp := strings.Index(sh, "k3d_import_image")
	if imp < 0 {
		t.Fatal("A-3 does not call k3d_import_image — the naive `docker save | ctr import` exits 0 " +
			"having imported nothing (V28-002), so the cluster keeps running the old image")
	}
	set := strings.Index(sh, "set image")
	if set < 0 || set < imp {
		t.Fatal("`set image` runs before the import — the node has no such image and the pod " +
			"ImagePullBackOffs on a reference that was never pushed")
	}
}

// ⛔ NO TOKEN VALUE IS EVER RENDERED. The scripts sit in $STAGE, which outlives the update, and the
// operator pastes their output into chat when something goes wrong. Every credential is READ from
// env.<id> at run time by the script itself.
func TestGeneratedScripts_CarryNoCredential(t *testing.T) {
	in := fixtureInput("compose")
	in.Observed.Env["ARGUS_RUNNER_TOKEN"] = "rt_THIS_IS_A_LIVE_TOKEN"
	in.Observed.Env["ARGUS_AUTHOR_TOKEN"] = "at_THIS_IS_ALSO_LIVE"
	in.Observed.Env["ARGUS_IDENTITY_KEY_B64"] = "aXRJc0FLZXk="
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	blob := RenderPreflight(p, in) + RenderApply(p, in)
	b, _ := json.Marshal(p)
	blob += string(b)
	for _, secret := range []string{"rt_THIS_IS_A_LIVE_TOKEN", "at_THIS_IS_ALSO_LIVE", "aXRJc0FLZXk="} {
		if strings.Contains(blob, secret) {
			t.Errorf("a credential reached the generated artefacts: %q", secret)
		}
	}
	// ⛔ AC-D53: AND THE HEALTH WAIT NEEDS NO CREDENTIAL AT ALL. It used to read ARGUS_RUNNER_TOKEN by name to
	// ask the host's published port; /healthz is on the runner's plain mux and needs none, and the verdict is
	// now read over the Docker/Kubernetes API — so no token is read, sent or rendered by apply.sh.
	if strings.Contains(RenderApply(p, in), "ARGUS_RUNNER_TOKEN") {
		t.Error("apply.sh reads ARGUS_RUNNER_TOKEN — since AC-D53 the health wait needs no credential")
	}
}

// ⭐ C-6 — A FAILED PLAN LEAVES NO RUNNABLE apply.sh.
//
// apply.sh is written LAST. If the planner dies half-way, the operator's next line — `bash apply.sh` —
// must fail to find a file, not run a half-written one.
func TestWritePlan_ApplyIsWrittenLastSoAHalfPlanCannotRun(t *testing.T) {
	stage := t.TempDir()
	in := fixtureInput("compose")
	in.StageDir = stage

	// A plan that cannot be built writes nothing at all.
	bad := in
	bad.ImageDigest = "unpinned:tag"
	if err := WritePlan(bad); err == nil {
		t.Fatal("a refused plan was written")
	}
	if _, err := os.Stat(filepath.Join(stage, "apply.sh")); !os.IsNotExist(err) {
		t.Fatal("a refused plan left an apply.sh behind — the operator's next line would run it")
	}

	if err := WritePlan(in); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"plan.json", "preflight.sh", "apply.sh"} {
		if _, err := os.Stat(filepath.Join(stage, f)); err != nil {
			t.Errorf("%s was not written: %v", f, err)
		}
	}
	// The ordering claim itself: apply.sh is not older than the other two.
	ap, _ := os.Stat(filepath.Join(stage, "apply.sh"))
	pf, _ := os.Stat(filepath.Join(stage, "preflight.sh"))
	if ap.ModTime().Before(pf.ModTime()) {
		t.Error("apply.sh was written BEFORE preflight.sh — a failed preflight render leaves a runnable apply")
	}
}

// ⛔ A ROLLBACK IS A DOWNGRADE, AND ONLY A DELIBERATE ONE IS PERMITTED. The planner refuses a lower
// target unless --rollback-to names it, and refuses one whose image is not the recorded `previous`.
func TestPlan_RollbackPermitsTheDowngradeOnlyWhenItMatchesPrevious(t *testing.T) {
	base := fixtureInput("compose")
	base.Installed = Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		Previous: &Previous{Version: "0.3.31", Image: "ghcr.io/x/exec@sha256:prev"}}

	// A bare downgrade — no --rollback-to — is refused.
	down := base
	down.Version, down.ImageDigest = "0.3.31", "ghcr.io/x/exec@sha256:prev"
	if _, err := BuildPlan(down); err == nil {
		t.Error("an undeclared downgrade was planned — every A-step would run backwards unannounced")
	}

	// The declared rollback is permitted.
	ok := down
	ok.RollbackTo = "0.3.31"
	p, err := BuildPlan(ok)
	if err != nil {
		t.Fatalf("the declared rollback was refused: %v", err)
	}
	if p.RollbackTo != "0.3.31" {
		t.Errorf("plan.rollback_to = %q", p.RollbackTo)
	}

	// A rollback towards an image that is NOT the recorded previous is refused — the page renders
	// previous.image, so a mismatch means the operator is pasting a block for another instance.
	wrong := ok
	wrong.ImageDigest = "ghcr.io/x/exec@sha256:somethingelse"
	if _, err := BuildPlan(wrong); err == nil {
		t.Error("a rollback towards an image that is not `previous.image` was planned")
	}
}

// The shared-folder exception is a SKIP with a stated reason, never a failure. A folder shared with an
// instance still on the old version is left alone, and the plan says which instance held it back.
func TestPlan_ASharedFolderIsSkippedWithAStatedReasonNotFailed(t *testing.T) {
	in := fixtureInput("compose")
	in.Siblings = []Sibling{{InstanceID: "i2", InstalledVersion: "0.3.31", SkillsHostPaths: []string{"/c/agents/product"}}}
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("a shared folder must never fail the plan: %v", err)
	}
	var a5 Step
	for _, s := range p.Steps {
		if s.ID == "A-5" {
			a5 = s
		}
	}
	if a5.SkipReason == "" {
		t.Fatal("A-5 was not skipped although a sibling on 0.3.31 shares the folder")
	}
	if !strings.Contains(a5.SkipReason, "i2") {
		t.Errorf("the skip reason does not name the instance holding it back: %q", a5.SkipReason)
	}
}

// An UNKNOWN sibling version holds the folder back too — "absent = unknown → held back, said so".
// Treating unknown as up-to-date would overwrite a skill an older executor still reads.
func TestPlan_AnUnknownSiblingVersionAlsoHoldsTheFolderBack(t *testing.T) {
	in := fixtureInput("compose")
	in.Siblings = []Sibling{{InstanceID: "i3", InstalledVersion: "", SkillsHostPaths: []string{"/c/agents/product"}}}
	p, _ := BuildPlan(in)
	for _, s := range p.Steps {
		if s.ID == "A-5" && s.SkipReason == "" {
			t.Fatal("an unknown sibling version did not hold the folder back — not knowing is not " +
				"evidence of being current")
		}
	}
}

// A sibling already on the target version does NOT hold the folder back.
func TestPlan_AnUpToDateSiblingDoesNotHoldTheFolderBack(t *testing.T) {
	in := fixtureInput("compose")
	in.Siblings = []Sibling{{InstanceID: "i4", InstalledVersion: "0.3.32", SkillsHostPaths: []string{"/c/agents/product"}}}
	p, _ := BuildPlan(in)
	for _, s := range p.Steps {
		if s.ID == "A-5" && s.SkipReason != "" {
			t.Fatalf("A-5 was held back by a sibling already on the target version: %q", s.SkipReason)
		}
	}
}

// ⛔ AC-D49 (#296): A SHARING SIBLING ON A VERSION THIS CODEBASE CANNOT ORDER IS A BLOCKER TOO, with
// wording distinct from "still below <v>" — SemverLess alone answers false for an unparseable
// version, the same false a genuinely current sibling gets, so without the ValidVersion consult a
// sibling stuck on "latest" would read as up to date and its shared folder would be overwritten.
func TestPlan_ASiblingOnAnUnrankableVersionAlsoHoldsTheFolderBackDistinctly(t *testing.T) {
	in := fixtureInput("compose")
	in.Siblings = []Sibling{{InstanceID: "i8", InstalledVersion: "latest", SkillsHostPaths: []string{"/c/agents/product"}}}
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("a sibling on an unrankable version must never fail the plan: %v", err)
	}
	var a5 Step
	for _, s := range p.Steps {
		if s.ID == "A-5" {
			a5 = s
		}
	}
	if a5.SkipReason == "" {
		t.Fatal("a sibling on an unrankable version did not hold the folder back — not knowing how " +
			"to order it is not evidence it is current")
	}
	if !strings.Contains(a5.SkipReason, "i8") {
		t.Errorf("the skip reason does not name the instance holding it back: %q", a5.SkipReason)
	}
	if !strings.Contains(a5.SkipReason, "cannot be ordered") {
		t.Errorf("the skip reason does not say the version cannot be ordered: %q", a5.SkipReason)
	}
	if strings.Contains(a5.SkipReason, "still below") {
		t.Errorf("an unrankable version was worded as \"still below\" — that claims an ordering that "+
			"was never made: %q", a5.SkipReason)
	}
}

// ⛔ AC-D49 (#296): THE DOWNGRADE GATE REFUSES A TARGET IT CANNOT ORDER, RATHER THAN GUESS THE
// DIRECTION. Before the ValidVersion consult, SemverLess(target, installed) answered false for an
// unparseable target — read as "not a downgrade" — and BuildPlan would plan a move whose direction
// nobody could state.
func TestPlan_RefusesATargetVersionThatCannotBeOrdered(t *testing.T) {
	in := fixtureInput("compose")
	in.Version = "latest"
	_, err := BuildPlan(in)
	if err == nil {
		t.Fatal("a target version nobody can order was planned — the direction of the move is unknown")
	}
	if !strings.Contains(err.Error(), "latest") {
		t.Errorf("the refusal does not name the target version: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be ordered") {
		t.Errorf("the refusal does not say the version cannot be ordered: %v", err)
	}
}

// ⛔ AC-D49 (#296): AN UNRANKABLE INSTALLED VERSION IS NOT A DEAD END. "Not knowing" is not evidence
// the instance is ahead of the target, so it is treated as BELOW it and the update proceeds forward
// — the same instance that could never be offered a rollback (AC-D49/#296) must still be updatable.
func TestPlan_AnUnrankableInstalledVersionProceedsAsAForwardUpdate(t *testing.T) {
	in := fixtureInput("compose")
	in.Installed = Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3"} // two-part: unrankable
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("an unrankable installed version was refused as though it were a downgrade: %v", err)
	}
	if p.RollbackTo != "" {
		t.Errorf("plan.rollback_to = %q — nothing asked for a rollback", p.RollbackTo)
	}
}

// C-7 — the kit swap carries EVERY instance's per-instance state, not just the one being updated.
// A kit holds every instance onboarded from this machine; renaming it away without carrying the rest
// forward de-onboards them silently.
func TestApplyScript_TheKitSwapCarriesEveryInstancesState(t *testing.T) {
	in := fixtureInput("compose")
	in.KitInstances = []string{"i1", "i2", "i7"}
	p, _ := BuildPlan(in)
	sh := RenderApply(p, in)
	for _, id := range in.KitInstances {
		// ⛔ k8s-rendered/<id> IS CARRIED FOR THE INSTANCE BEING UPDATED TOO. This test used to assert
		// the opposite, on the belief that the staged kit held a freshly rendered set; it holds the
		// image's bundle, which renders nothing per instance — so the "exception" deleted the set.
		for _, class := range []string{"env." + id, "identity." + id + ".key", "promtail-config." + id + ".yaml",
			"k8s-rendered/" + id, "demo-sut." + id + ".project", ".image-prev." + id} {
			if !strings.Contains(sh, "carry 'deploy/compose/"+class+"' || false") {
				t.Errorf("A-1 does not carry %q across the kit swap (failing the step if the copy fails) — that instance loses it (C-7)", class)
			}
		}
	}
	for _, f := range []string{"cp-author.token", "cp-session.token", "sut-secrets.env"} {
		if !strings.Contains(sh, "carry 'deploy/compose/"+f+"' || false") {
			t.Errorf("A-1 does not carry the machine-wide %s across the swap", f)
		}
	}
	// the shared Prometheus's registry holds OTHER kits' instances too: it is carried whole
	if !strings.Contains(sh, "carry_dir_contents 'deploy/compose/targets.d' || false") {
		t.Error("A-1 does not carry targets.d whole — instances onboarded from other kits lose their Prometheus target")
	}
	// ⛔ .env is REGENERATED, not carried: it pins the OLD image.
	if regexp.MustCompile(`carry[^\n]*deploy/compose/\.env\b`).MatchString(sh) {
		t.Error("A-1 carries deploy/compose/.env forward — it pins the old image and must be regenerated")
	}
}

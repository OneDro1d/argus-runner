package updatecmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/skills"
)

// plan.go — V31-001 (VR13-UP): THE PLANNER, and the two scripts the host runs.
//
// ⛔ C-1 — THE SPLIT IS NOT A STYLE CHOICE. `update plan` runs inside the executor image, which has
// no docker CLI, no kubectl and no compose plugin (Dockerfile.onedroid-argus-execution-plane:88). It can
// read what it was given and write files into a mounted stage directory, and that is all. Every
// effect on the machine therefore happens in bash, on the HOST, from text this file generates.
//
// That constraint decides the shape of everything below: the planner is PURE, and its whole output is
// reviewable text an operator can read before running. A test that asserts on generated text is
// asserting on the real artefact, not on a proxy for it.

// Step is one A-step: what it does, and how to put it back.
type Step struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	// Undo is the compensation the trap runs in reverse. Empty ONLY for A-8a/A-8b, which sit
	// outside the trap (C-13).
	Undo string `json:"undo,omitempty"`
	// SkipReason states why this step will not run. ⛔ A skip is a PLANNED outcome with a reason an
	// operator can read — never a silent omission and never a failure.
	SkipReason string `json:"skip_reason,omitempty"`
}

// Plan is `$STAGE/plan.json`. Its first three fields are the plan's IDENTITY: preflight.sh stamps
// them into preflight.json and apply.sh refuses a preflight whose stamp differs, because a preflight
// from an earlier plan checked different preconditions against a different machine state.
type Plan struct {
	InstanceID  string     `json:"instance_id"`
	ImageDigest string     `json:"image_digest"`
	GeneratedAt string     `json:"generated_at"`
	Tier        string     `json:"tier"`
	Image       string     `json:"image"`
	Version     string     `json:"version"`
	RollbackTo  string     `json:"rollback_to,omitempty"`
	Steps       []Step     `json:"steps"`
	Artefacts   []Artefact `json:"artefacts,omitempty"`
}

// PlanInput is everything the planner is given. It reads nothing else — no environment, no clock, no
// filesystem — so the same inputs yield the same scripts on any machine.
type PlanInput struct {
	InstanceID string
	Tier       string // compose | k3d | managed
	Image      string // the tag the block pulled
	// ImageDigest is the host-resolved repo@sha256:… reference. ⛔ REQUIRED and PINNED: planning
	// against a floating tag lets the apply install something preflight never checked.
	ImageDigest string
	// RunnerImage is the image whose `update` verbs the scripts call back into. "" means ImageDigest.
	// ⛔ ON A ROLLBACK IT MUST BE THE INSTALLED (NEWEST) IMAGE (SA-D19): the target is pre-0.3.32 on
	// the first rollback of every existing instance and answers `unknown command "update"`.
	RunnerImage string
	Version     string
	KitDir      string
	StageDir    string
	RouterState string
	CPURL       string
	// KubeContext / Kubeconfig are rendered as literals into kube() — never inherited from whatever
	// context the operator last selected (VR6-S1).
	KubeContext string
	Kubeconfig  string
	// HealthBudget is how long, in seconds, a moved executor or router has to come back. Zero means
	// 180, update.sh's proven budget. A literal, so no operator environment variable can change it.
	HealthBudget int
	// KitInstances is every instance this kit holds. C-7: A-1 carries ALL of their per-instance
	// state across the swap, not just the one being updated.
	KitInstances []string
	Observed     Observed
	Installed    Manifest
	Siblings     []Sibling
	// RollbackTo, when set, PERMITS the downgrade to that version and asserts the target image is
	// the recorded `previous.image`.
	RollbackTo string
	// RouterOnly plans the machine router alone (C-24): step A-7 from the NEW kit's router definition,
	// no kit swap, no instance, no manifest. See router_only_test.go for why it records nothing.
	RouterOnly bool
	// Now is the generated_at stamp, injected so the plan is reproducible in a test.
	Now string
}

// ⛔ EVERY PER-INSTANCE FILE LIVES UNDER <kit>/deploy/compose/ — onboard.sh's $COMPOSE_DIR.
//
// ⭐ THIS WAS WRONG UNTIL THE LIVE QA LOOKED AT A REAL KIT, and wrong here is not cosmetic. The
// generated scripts resolved `env.<id>` at the KIT ROOT, where onboarding has never written it, so
// preflight's `[ -f "$ENV_FILE" ]` would have failed on EVERY real instance: the update path would
// have printed `untouched` and stopped, on every tier, for everyone. And had that been fixed alone,
// A-1's swap would have carried NOTHING forward — every instance's env, identity, tokens and
// targets lost on the rename, which is the exact catastrophe C-7 exists to prevent.
//
// ⛔ IT WAS INVISIBLE TO THE TESTS BECAUSE THE FIXTURE AGREED WITH THE CODE. `fakeMachine` built the
// kit I had imagined, so the execution matrix ran green against a layout that does not exist. The
// guard is now kit_layout_test.go, which derives these paths from onboard.sh itself.
//
// The authority is onboard.sh, and each line is cited:
//
//	env.<id>                   :1149    identity.<id>.key          :1969
//	cp-session.token           :1559    promtail-config.<id>.yaml  :2236
//	targets.d/                 :2988    k8s-rendered/<id>          :2252
const kitComposeDir = "deploy/compose"

// perInstanceClasses are the classes of per-instance state A-1 carries across the kit swap, relative
// to the kit root. They are named here once so the script, the plan and the grounding test agree.
//
// ⛔ `k8s-rendered/<id>` IS CARRIED FOR EVERY INSTANCE, THE ONE BEING UPDATED INCLUDED. An earlier
// version excluded it on the belief that the staged kit held a freshly rendered set. It does not: the
// staged kit is the image's bundle (Dockerfile.onedroid-argus-execution-plane:133-162), which renders
// nothing per instance, so the exclusion DELETED this instance's rendered set on every update.
//
//	demo-sut.<id>.project   lib/demo-sut.sh:77-79 — teardown's only record of the demo SUT it started
//	.image-prev.<id>        update.sh:291 — update.sh's rollback breadcrumb
func perInstanceClasses(id string) []string {
	return []string{
		kitComposeDir + "/env." + id,
		kitComposeDir + "/identity." + id + ".key",
		kitComposeDir + "/promtail-config." + id + ".yaml",
		kitComposeDir + "/k8s-rendered/" + id,
		kitComposeDir + "/demo-sut." + id + ".project",
		kitComposeDir + "/.image-prev." + id,
	}
}

// machineWideFiles are carried once, not per instance: they belong to the MACHINE, not to any one
// instance. All sit beside the per-instance files, under $COMPOSE_DIR.
//
//	sut-secrets.env   onboard.sh:26 — docker-compose.byo-m3.yml:52-54 reads it as the executor's
//	                  env_file, so A-3's recreate WITHOUT it starts an executor that cannot resolve
//	                  a single SUT secret.
var machineWideFiles = []string{
	kitComposeDir + "/cp-author.token",
	kitComposeDir + "/cp-session.token",
	kitComposeDir + "/sut-secrets.env",
}

// machineWideDirs are carried WHOLE.
//
// ⛔ targets.d IS THE SHARED PROMETHEUS'S TARGET REGISTRY, NOT THIS KIT'S. docker-compose.obs-shared.yml:44
// mounts `./targets.d` of whichever kit first started argus-obs, and onboard.sh:2992-2997 writes
// EVERY later instance's target into that mounted directory — a k3d instance from another kit
// included. Carrying only this kit's instances' files would silently drop theirs, and their
// Prometheus panels would go empty.
var machineWideDirs = []string{
	kitComposeDir + "/targets.d",
}

// KitEnvFile is where an instance's env file really is, relative to the kit root. ONE definition,
// read by both generated scripts and by the grounding test.
func KitEnvFile(instanceID string) string { return kitComposeDir + "/env." + instanceID }

// InstanceProject and InstanceNamespace are the names onboarding gives an instance: the compose
// project (onboard.sh:1148) and the k8s namespace (update.sh:292). ONE spelling, which the generated
// scripts render as `argus-inst-$INSTANCE` and kit_layout_test.go derives from update.sh.
//
// ⛔ THEY WERE `argus-<id>` AND `argus-executor-<id>` UNTIL A REAL KIT WAS OPENED — names no
// onboarding has ever produced, so the fence, the recreate and discovery all addressed nothing.
func InstanceProject(id string) string   { return "argus-inst-" + id }
func InstanceNamespace(id string) string { return "argus-inst-" + id }

// installedForGate is the installed version the downgrade gate compares a target with — the HIGHEST version the
// executor may be on that it can order — and whether a not-confirmed marker stood behind it.
//
// ⛔ WHILE THE MARKER STANDS, THE VIEW READS THE VERSION UNKNOWN — AND THE GATE MUST NOT OPEN. An unknown installed
// version is, by design, no evidence of being ahead of the target (the gate's own note, BuildPlan), so the view
// alone switched the gate off for exactly the instances whose executor nobody could confirm. The executor is where
// the unconfirmed move came FROM or where it was going TO — the marker names both; when its plan could not read the
// executor under an earlier marker, "from" is every version that one left it on (FromOneOf). A target below the
// highest could be a downgrade, and only --rollback-to may do that. When none can be ordered, the version is
// genuinely unknown and the design's rule stands.
//
// ⛔ THE RECORD'S VERSION COUNTS TOO, ALWAYS — kept as a deliberate over-refusal. Round 4 dropped it whenever "from" was
// read, and a downgrade got through (round-4 review G-3): the reading was then the ARGUS_VERSION override. discover now
// asks the binary for its own version (round-5 review R5-GATE-1) and reads no pod that is still terminating (R5-K8S-1),
// so no known wrong reading is left — the record stays counted beside it anyway, because a refusal armed wrongly names
// a way that works and one cleared wrongly moves the executor down. The price: after the same rollback run twice
// unconfirmed, the forward block to its version is refused too — the refusal names re-running the rollback block,
// which works, and says the version it counts is the record's.
// ⛔ AND DISCOVER'S READING IS NOT COUNTED (round-4 review G-1: it was the override then, and an override above the
// target refused every correct forward update). A record with no version (a fresh onboarding, or none at all) could
// now be bounded by the reading — not built: with no marker the refusal names the page's rollback block and
// --rollback-to, and such an instance has neither (round-5 review R5-GATE-3, open; the gate compares with nothing
// there, as on dev).
func installedForGate(m Manifest) (version string, unconfirmed bool) {
	cands := []string{m.Version}
	if u := m.Unconfirmed; m.Version == "" && u != nil {
		cands = unconfirmedVersions(u)
		unconfirmed = true
	}
	for _, v := range cands {
		if ValidVersion(v) && (version == "" || SemverLess(version, v)) {
			version = v
		}
	}
	return version, unconfirmed && version != ""
}

// unconfirmedVersions is every version a marker says the executor may be on: where its move was going, where it came
// from — read (FromVersion), or every version an earlier marker left it on (FromOneOf) — and the record's under it.
// Only versions (ValidVersion): the re-hash's literal "unknown" is not one.
func unconfirmedVersions(u *Unconfirmed) []string {
	return orderedVersions(append([]string{u.RecordVersion, u.Version, u.FromVersion}, u.FromOneOf...))
}

// orderedVersions is the distinct versions among vs, lowest first; anything ValidVersion refuses is left out, and one
// version written with and without its leading "v" is one version.
func orderedVersions(vs []string) []string {
	var out []string
	for _, v := range vs {
		v = strings.TrimSpace(v)
		if !ValidVersion(v) {
			continue
		}
		seen := false
		for _, o := range out {
			seen = seen || strings.TrimPrefix(o, "v") == strings.TrimPrefix(v, "v")
		}
		if !seen {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return SemverLess(out[i], out[j]) })
	return out
}

// unconfirmedDowngrade is the gate's refusal while a not-confirmed marker stands: it names EVERY version the executor
// can be on, and what to do — which is not "use the rollback block": the page offers none while the version is unknown.
// ⛔ "One of the two" was false when "from" was the version it was going to, and when "from" was never read. The list
// is the versions the sentence names: an origin that is not an orderable version is named as it is, and a "from" or a
// "to" nobody recorded is "an unrecorded version" there too.
func unconfirmedDowngrade(in PlanInput, highest string) error {
	const unrecorded = "an unrecorded version"
	u := in.Installed.Unconfirmed
	on := unconfirmedVersions(u)
	named := []string{u.Version} // the versions the "from … to …" clause names
	var from string
	switch {
	case executorKnown(u.FromVersion):
		from = u.FromVersion
		named = append(named, from)
		if !ValidVersion(from) {
			on = append(on, from)
		}
	case len(orderedVersions(u.FromOneOf)) > 0:
		from = "one of " + strings.Join(orderedVersions(u.FromOneOf), ", ")
		named = append(named, u.FromOneOf...)
	case ValidVersion(u.RecordVersion):
		from = u.RecordVersion
		named = append(named, from)
	default:
		// nobody recorded where it was: the executor may still be there
		from = unrecorded
		on = append(on, unrecorded)
	}
	to := u.Version
	if to == "" {
		to = unrecorded
		if from != unrecorded {
			on = append(on, unrecorded)
		}
	}
	// ⛔ round-5 review R5-MSG-1: EVERY VERSION THE GATE COUNTS, THE SENTENCE EXPLAINS — it can be the very version that
	// refuses. Beyond the move's own "from … to …": the record's version (installedForGate counts it), and on k3d, where
	// the unknown commit blanks the record, a version an earlier marker named that an unread re-run carried (from_one_of
	// beside a carried "from": round-6 design check S1/S6).
	record, earlier := "", []string(nil)
	for _, v := range on {
		switch {
		case !ValidVersion(v) || namesVersion(named, v):
		case namesVersion([]string{u.RecordVersion}, v):
			record = ", and its installed record still says " + v
		default:
			earlier = append(earlier, v)
		}
	}
	// the words say only what is known: an earlier marker named it — as where a move went or came from, or as the record
	// of that time (round-6 review R6-MSG-1: "may have left it on" claimed a move no run made)
	if len(earlier) > 0 {
		record += ", and an earlier unconfirmed run also named " + strings.Join(earlier, ", ")
	}
	onText := strings.Join(on, ", ")
	if len(on) > 1 {
		onText = "one of " + onText
	}
	what := "update"
	if u.RollbackTo != "" {
		what = "rollback to " + u.RollbackTo
	}
	return fmt.Errorf("this plan would move %s DOWN to %s and no --rollback-to says so: an earlier %s moved its executor "+
		"from %s to %s and could not confirm it%s, so it is on %s, and %s is below %s. Re-run that %s "+
		"(its block), or update to %s or above; a rollback is offered again once the executor is confirmed",
		in.InstanceID, in.Version, what, from, to, record, onText, in.Version, highest, what, highest)
}

// namesVersion reports whether vs holds v, a version written with and without its leading "v" being one version.
func namesVersion(vs []string, v string) bool {
	for _, x := range vs {
		if strings.TrimPrefix(strings.TrimSpace(x), "v") == strings.TrimPrefix(strings.TrimSpace(v), "v") {
			return true
		}
	}
	return false
}

// BuildPlan turns the observation into the step list. Pure.
func BuildPlan(in PlanInput) (Plan, error) {
	if !strings.Contains(in.ImageDigest, "@sha256:") {
		return Plan{}, fmt.Errorf("--image-digest %q is not pinned: the block resolves it host-side with "+
			"`docker image inspect --format '{{index .RepoDigests 0}}'` and it must be a repo@sha256:… "+
			"reference. Planning against a floating tag lets the apply install an image preflight never checked",
			in.ImageDigest)
	}
	if in.RouterOnly {
		// ⛔ A-7 ALONE. The router is machine-wide: no kit swap (the router's definition is read from the
		// kit this plan staged), no instance, and no manifest — a per-instance record written for a
		// machine-wide move would re-record an instance nothing measured as "unknown".
		return Plan{ImageDigest: in.ImageDigest, GeneratedAt: in.Now, Image: in.Image, Version: in.Version,
			Steps: []Step{{ID: "A-7",
				Action: "move the machine router to the new image, from the new kit's definition (router-only)",
				Undo:   "move the router back to the image it ran before"}}}, nil
	}
	if in.Observed.Empty() {
		return Plan{}, fmt.Errorf("observed.json says nothing about this machine — a hard stop, never an " +
			"empty reconstruction. Planning from an empty reading would install over a live instance as " +
			"though nothing were there. Re-run `update discover` and the host script it writes")
	}
	if in.InstanceID == "" {
		return Plan{}, fmt.Errorf("the plan names no instance")
	}

	// ── the downgrade gate ────────────────────────────────────────────────────────────────────
	// A lower target version is legal ONLY when --rollback-to names it. Otherwise it is a paste of
	// the wrong block, and running it backwards unannounced is the worst outcome available.
	//
	// ⛔ AC-D49 (#296): THE TARGET MUST BE ORDERABLE BEFORE "not below" MEANS ANYTHING. SemverLess
	// alone answers false for a version it cannot parse — the same answer a genuine forward move
	// gets — so a target this codebase cannot rank (e.g. "latest") would sail through unrefused
	// with the direction of the move genuinely unknown. ValidVersion is consulted FIRST and refuses
	// outright, naming the version, rather than plan a move whose direction nobody can state.
	//
	// An UNRANKABLE INSTALLED version is the opposite case: not knowing where the machine came from
	// is not evidence it is ahead of the target (1-SKILLS' rule, applied here too) — it is treated as
	// BELOW the target, so the plan proceeds as an ordinary forward update rather than a dead end no
	// operator can move an instance past.
	if in.Version != "" {
		if !ValidVersion(in.Version) {
			return Plan{}, fmt.Errorf("target version %q cannot be ordered against the installed version "+
				"%q — the direction of this update is unknown, so BuildPlan refuses rather than guess",
				in.Version, in.Installed.Version)
		}
		installed, unconfirmed := installedForGate(in.Installed)
		if installed != "" && ValidVersion(installed) && SemverLess(in.Version, installed) {
			if in.RollbackTo == "" {
				if unconfirmed {
					return Plan{}, unconfirmedDowngrade(in, installed)
				}
				return Plan{}, fmt.Errorf("this plan would move %s from %s DOWN to %s and no --rollback-to says so. "+
					"A rollback is deliberate: use the rollback block on the Environments page, which fills in "+
					"--rollback-to and the previous image",
					in.InstanceID, installed, in.Version)
			}
			if in.Installed.Previous == nil {
				return Plan{}, fmt.Errorf("--rollback-to %s, but this instance records no previous version to "+
					"return to", in.RollbackTo)
			}
		}
	}
	// ⛔ THE ROLLBACK'S OWN GUARD DOES NOT DEPEND ON THE INSTALLED VERSION. It sat inside the downgrade test, so a
	// record whose version reads unknown — the re-hash's "unknown", or ReadManifest's view while a not-confirmed
	// marker stands — skipped it, and a rollback block naming another image was planned.
	if in.RollbackTo != "" && in.Installed.Previous != nil {
		if prev := in.Installed.Previous.Image; prev != "" && prev != in.ImageDigest {
			return Plan{}, fmt.Errorf("--rollback-to %s targets %s, but the recorded previous image is %s. "+
				"The page renders the rollback block from `previous.image`, so a mismatch means this block "+
				"belongs to another instance or another update",
				in.RollbackTo, in.ImageDigest, prev)
		}
	}

	p := Plan{
		InstanceID: in.InstanceID, ImageDigest: in.ImageDigest, GeneratedAt: in.Now,
		Tier: in.Tier, Image: in.Image, Version: in.Version, RollbackTo: in.RollbackTo,
	}

	add := func(s Step) { p.Steps = append(p.Steps, s) }

	add(Step{ID: "A-1",
		Action: "swap the kit by rename, carrying every instance's per-instance state forward",
		Undo:   "swap the previous kit back by rename"})

	if in.Tier == "k3d" || in.Tier == "managed" {
		// ⛔ A SKIP WITH ITS REASON, NOT A STEP THAT PRETENDS. Nothing renders a new object set for an
		// update: the staged kit is the image's bundle, and SA-D20's digest-pinned render is not built.
		// Applying the carried set would re-apply what the cluster already runs, with the obs images
		// at `:latest` + `imagePullPolicy: Always` — an upgrade D-10 forbids.
		add(Step{ID: "A-2",
			Action:     "kubectl apply the rendered object set from the staged kit",
			Undo:       "kubectl apply the previous rendered set from the kept kit",
			SkipReason: "not built in this release: no new object set is rendered for an update (SA-D20), so the cluster's objects are left as they are; the executor moves by `set image` in A-3, as update.sh does"})
	}

	add(Step{ID: "A-3", Action: executorAction(in.Tier), Undo: executorUndo(in.Tier)})

	if in.Tier == "compose" {
		// ⛔ COMPOSE ONLY. On k3d/aks loki, promtail and pushgateway are OBJECTS in the set A-2
		// applies; recreating them by name here would fight the cluster's own reconciler (SA-D20).
		add(Step{ID: "A-3a",
			Action:     "recreate loki, promtail and pushgateway at their recorded digests",
			Undo:       "recreate them at the digests observed before this update",
			SkipReason: "not built in this release: the update pulls no observability image, so loki, promtail and pushgateway are left running exactly as they are"})
	}

	a4 := Step{ID: "A-4",
		Action: "re-provision this instance's Grafana datasource and dashboard from the new kit, rewrite its Prometheus target, recreate promtail (compose)",
		Undo:   "re-post the documents captured before anything moved (an absent one is deleted) and restore the previous target file"}
	switch {
	case in.Tier == "compose" && in.Observed.ObsNone:
		//. Decided HERE, before the function does anything and so before preflight captures a
		// Grafana document or the script writes a target: an `--obs none` instance has no loki, promtail or
		// pushgateway and no datasource or dashboard of its own, and provisioning them would start the promtail
		// onboarding deliberately did not. Applies to a rollback too (same plan, --rollback-to).
		a4.SkipReason = "this instance was onboarded with --obs none and has no observability: no Grafana datasource or dashboard, no Prometheus target and no promtail, loki or pushgateway exist for it, so none is provisioned or recreated"
	case in.Tier == "managed":
		a4.SkipReason = "not built in this release: the managed Grafana needs its admin credential re-discovered at run time (SA-D24) — the datasource and dashboard are left as they are"
	case !in.Observed.Grafana.Reachable:
		a4.SkipReason = "Grafana did not answer when this machine was read, so the datasource and dashboard are left as they are — re-run the update once it answers"
	}
	add(a4)

	// ── A-5: the shared-folder exception ──────────────────────────────────────────────────────
	a5 := Step{ID: "A-5",
		Action: "install the skills into every agent folder wired to this instance, backing up what is there first",
		Undo:   "restore the backups from <folder>/.argus-skill-backups/<stamp>/ and remove what was newly installed"}
	a5.SkipReason = skillsSkipReason(in)
	add(a5)

	add(Step{ID: "A-5a", Action: "merge and enable the agent config", Undo: "restore the previous agent config",
		SkipReason: "not built in this release: each folder's .mcp.json points at the machine router, and is left exactly as onboarding wrote it"})
	add(a6Step(in))
	add(Step{ID: "A-7",
		Action: "move the machine router to the new image, LAST",
		Undo:   "move the router back to the image it ran before"})

	// ⛔ OUTSIDE THE TRAP (C-13). A-8a wrote the authority; A-8b only informs the control plane, and
	// an outage there must never undo a successful update on the machine.
	add(Step{ID: "A-8a", Action: "write the installed manifest (`update commit`)"})
	add(Step{ID: "A-8b", Action: "report the outcome to the control plane (`update report`, best-effort)"})

	// The artefact list the manifest is built FROM. ⛔ It is the INTENDED shape, never the
	// recorded state: Rehash resolves each entry against what progress.json says actually
	// happened to the step that owns it (C-15). Emitting it here keeps one definition of what
	// this instance is made of, which the plan, the script and the manifest all read.
	p.Artefacts = plannedArtefacts(in)

	return p, nil
}

func executorAction(tier string) string {
	switch tier {
	case "compose":
		return "rewrite ARGUS_MCP_IMAGE in env.<id>, recreate the executor service of argus-inst-<id>, wait for /healthz"
	case "k3d":
		return "pull, k3d_import_image onto every node, `set image deploy/executor`, wait for the rollout"
	default:
		return "`set image deploy/executor` and wait for the rollout"
	}
}

func executorUndo(tier string) string {
	switch tier {
	case "compose":
		return "rewrite ARGUS_MCP_IMAGE back to the image the executor ran before, recreate, wait for /healthz"
	default:
		return "`set image deploy/executor` back to the image it ran before and wait for the rollout"
	}
}

// a6Step is A-6: the Path A demo scenarios, refreshed with the same file rules as A-5's skills.
//
// ⛔ THE SENTENCE IT REPLACES WAS FALSE (V32 Release QA finding (a)). A-6 used to be skipped with "Path A
// scenarios live in the control plane's catalog, not in files on this machine". A Path A kit holds them
// in <kit>/test-agent/scenarios, seeded from order-service-demo/scenarios-baked, and measured on
// orderservice-compose they were still the old format the new release refuses after the update.
//
// ⛔ ONLY A PATH A KIT. A test folder anywhere else is the operator's own scenario work, which an update
// never overwrites. The planner is pure, so it decides by the recorded folder alone; whether the new kit
// ships a set to refresh from is checked on the host, at run time, and a miss is a `skipped` step.
func a6Step(in PlanInput) Step {
	s := Step{ID: "A-6",
		Action: "refresh the Path A demo scenarios in <kit>/test-agent/scenarios from the new kit, keeping what was there beside the folder",
		Undo:   "restore the scenarios kept in <kit>/test-agent/.argus-scenario-backups/<stamp>/"}
	test := strings.TrimSpace(in.Observed.Env["ARGUS_TEST_DIR_HOST"])
	switch {
	case in.RollbackTo != "":
		s.SkipReason = "a rollback leaves the scenario files as they are — the forward update kept the ones it replaced in <kit>/test-agent/.argus-scenario-backups/"
	case test == "":
		s.SkipReason = "this instance's test-agent folder is not recorded (ARGUS_TEST_DIR_HOST), so there is no scenario folder an update may refresh"
	case normalizeFolder(test) != normalizeFolder(in.KitDir+"/test-agent"):
		s.SkipReason = "the scenarios in " + test + " are the operator's own (this is not a Path A demo kit) — an update never overwrites them"
	}
	return s
}

// PathATestDir is the Path A test-agent folder A-6 refreshes, or "" when A-6 does not run.
func pathATestDir(p Plan, in PlanInput) string {
	for _, s := range p.Steps {
		if s.ID == "A-6" && s.SkipReason == "" {
			return strings.TrimSpace(in.Observed.Env["ARGUS_TEST_DIR_HOST"])
		}
	}
	return ""
}

// skillsSkipReason returns why A-5 is skipped, or "".
func skillsSkipReason(in PlanInput) string {
	if len(in.Observed.Folders) == 0 {
		return "no agent folder in the router's state is wired to " + in.InstanceID + " — there is nowhere to install skills"
	}
	var bad []string
	for _, f := range in.Observed.Folders {
		if !f.Writable {
			bad = append(bad, f.Path+" (not writable)")
		} else if len(skills.Installed(role.Role(f.Hat))) == 0 {
			bad = append(bad, f.Path+" (hat "+strconvQuote(f.Hat)+" is not one onboarding installs skills for)")
		}
	}
	if len(bad) > 0 {
		return "the skills are left alone: " + strings.Join(bad, ", ")
	}
	return heldBack(in)
}

func strconvQuote(s string) string { return `"` + s + `"` }

// heldBack returns why A-5 is skipped, or "".
//
// ⛔ THE RULE IS "NOT KNOWING IS NOT EVIDENCE OF BEING CURRENT" (1-SKILLS). A sibling whose version
// the control plane cannot report holds the folder back and SAYS SO, because the alternative —
// treating unknown as up-to-date — overwrites a skill an older executor is still reading.
func heldBack(in PlanInput) string {
	ours := map[string]bool{}
	for _, f := range in.Observed.Folders {
		ours[normalizeFolder(f.Path)] = true
	}
	var blockers []string
	for _, s := range in.Siblings {
		if s.InstanceID == in.InstanceID {
			continue
		}
		shares := false
		for _, p := range s.SkillsHostPaths {
			if ours[normalizeFolder(p)] {
				shares = true
				break
			}
		}
		if !shares {
			continue
		}
		switch {
		case s.InstalledVersion == "":
			blockers = append(blockers, s.InstanceID+" (version unknown)")
		// ⛔ AC-D49 (#296): A NON-EMPTY VERSION THIS CODEBASE CANNOT ORDER IS A BLOCKER TOO, NOT A
		// PASS-THROUGH. SemverLess alone answers false for a version it cannot parse — the same
		// answer a genuinely current sibling gets — so without this ValidVersion consult first, a
		// sibling on e.g. "latest" or "0.3" falls through as "not a blocker" and its skills folder
		// gets overwritten out from under it. Named with wording distinct from "still below <v>":
		// this sibling is not KNOWN to be behind, it is UNRANKABLE, and the operator needs to know
		// which one it is.
		case !ValidVersion(s.InstalledVersion):
			blockers = append(blockers, s.InstanceID+" ("+s.InstalledVersion+", cannot be ordered)")
		case SemverLess(s.InstalledVersion, in.Version):
			blockers = append(blockers, s.InstanceID+" ("+s.InstalledVersion+", still below "+in.Version+")")
		}
	}
	if len(blockers) == 0 {
		return ""
	}
	return "a skills folder is shared with " + strings.Join(blockers, ", ") +
		" — the skills are left alone until that instance is updated"
}

// normalizeFolder compares paths the way two machines spell the same folder: case-insensitively, with
// both separators, and without a trailing one. A mixed-spelling miss here would silently UNSHARE a
// folder and overwrite the sibling's skills, so it fails towards "shared".
func normalizeFolder(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	// ⛔ THREE SPELLINGS OF ONE WINDOWS FOLDER (updatecmd.go MSYSPath's table). `/c/agents` and
	// `C:\agents` must compare equal, or a folder shared with a sibling reads as unshared and the
	// sibling's skills are overwritten.
	if len(p) >= 3 && p[0] == '/' && p[2] == '/' && ((p[1] >= 'a' && p[1] <= 'z') || (p[1] >= 'A' && p[1] <= 'Z')) {
		p = string(p[1]) + ":" + p[2:]
	}
	p = strings.TrimRight(p, "/")
	return strings.ToLower(p)
}

// WritePlan writes plan.json, then preflight.sh, then apply.sh.
//
// ⭐ C-6 — THE ORDER IS THE CONTRACT. apply.sh is written LAST so a planner that dies half-way leaves
// the operator's next line (`bash apply.sh`) failing to find a file rather than running a partial one.
// A refused plan writes nothing at all.
func WritePlan(in PlanInput) error {
	p, err := BuildPlan(in)
	if err != nil {
		return err
	}
	if in.StageDir == "" {
		return fmt.Errorf("no stage directory: the block computes it beside the kit")
	}
	if err := os.MkdirAll(filepath.Join(in.StageDir, "undo"), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(in.StageDir, "plan.json"), b, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(in.StageDir, "preflight.sh"), []byte(RenderPreflight(p, in)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(in.StageDir, "apply.sh"), []byte(RenderApply(p, in)), 0o700)
}

// shq quotes a value for bash. Every value the planner renders goes through it: the kit dir is a
// Windows path with spaces often enough, and an unquoted one splits into two arguments.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// plannedArtefacts is what this instance is made of, at the version this plan targets.
//
// ⛔ EVERY ENTRY CARRIES A VERSION, INCLUDING "unknown". The held-back rule reads absent-as-unknown
// and holds a folder back for it, so an artefact left blank would keep a shared folder amber forever
// — a rule that fails safe still has to be given real data. "unknown" is legal only where it is
// TRUE: a third-party program whose version is not ours to read.
func plannedArtefacts(in PlanInput) []Artefact {
	v := in.Version
	if v == "" {
		v = "unknown"
	}
	out := []Artefact{
		// "executor" is the compose SERVICE and the k8s Deployment name (update.sh:373, :375).
		{Kind: "executor", Name: "executor", Version: v, Image: in.ImageDigest},
		{Kind: "kit", Name: "argus-kit", Version: v, Loc: &Loc{HostPath: in.KitDir}},
		{Kind: "agentcfg", Name: ".mcp.json", Version: v},
		{Kind: "scenarios", Name: "scenarios-installed", Version: v},
		{Kind: "router", Name: "argus-router", Version: v, Image: in.ImageDigest},
	}
	// One skills entry per agent folder the ROUTER records — the same list A-5 installs into, and the
	// one the control plane groups by for the shared-folder exception. Never a second list.
	for _, f := range in.Observed.Folders {
		out = append(out, Artefact{Kind: "skills", Name: f.Path, Version: v,
			Loc: &Loc{HostPath: f.Path}})
	}
	switch in.Tier {
	case "compose":
		out = append(out, Artefact{Kind: "promtail_cfg", Name: "promtail-config." + in.InstanceID + ".yaml", Version: v})
		// ⛔ THE OBS STACK IS A THIRD PARTY. Loki, promtail and pushgateway are not ours and we record
		// the DIGEST we pinned them at, not a Argus version we would be inventing.
		for _, svc := range []string{"loki", "promtail", "pushgateway"} {
			out = append(out, Artefact{Kind: "obs_stack", Name: svc, Version: "unknown",
				Runtime: "compose_service", Image: observedServiceDigest(in.Observed, svc)})
		}
	default:
		out = append(out, Artefact{Kind: "k8s_object", Name: "rendered set", Version: v,
			Namespace: InstanceNamespace(in.InstanceID)})
	}
	return out
}

func observedServiceDigest(obs Observed, name string) string {
	for _, s := range obs.Compose.Services {
		if s.Name == name {
			return s.Digest
		}
	}
	return ""
}

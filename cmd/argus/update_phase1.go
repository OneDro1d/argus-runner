package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/buildinfo"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// applyPlanDefaults fills what the page's block deliberately does not pass.
//
// ⛔ THE BLOCK CARRIES NO --version AND NO --control-plane (SA §0.7). Neither is guessed here:
//   - the version: on a FORWARD update the image running this verb IS the target, so its own build stamp
//     is the version being installed; on a rollback it is the version --rollback-to names;
//   - the control plane: the one this instance already reports to, which onboarding wrote into its env
//     file and discovery records (ARGUS_CP_URL is on the SA-D18 allow-list). Without it `update
//     report` has nowhere to send the outcome and the page never learns how the update ended.
//
// An explicit flag always wins.
func applyPlanDefaults(a *updateArgs, obs updatecmd.Observed) {
	if a.Version == "" {
		if a.RollbackTo != "" {
			a.Version = a.RollbackTo
		} else {
			a.Version = buildinfo.Resolve(os.Getenv("ARGUS_VERSION"))
		}
	}
	if a.CPURL == "" {
		a.CPURL = strings.TrimSpace(obs.Env["ARGUS_CP_URL"])
	}
}

// update_phase1.go — V31-001 (VR13-UP): `update discover` and `update plan`.
//
// ⛔ C-1 — PHASE 1 CANNOT RUN WHOLLY INSIDE THE CONTAINER. The executor image carries no docker CLI,
// no kubectl and no compose plugin (Dockerfile.onedroid-argus-execution-plane:88), so it cannot ask "is the
// executor healthy", "does the compose project exist", "does the cluster answer". These two verbs do
// what mounts and HTTP allow, and emit bash for the HOST to run.
//
// That is not a workaround it apologises for. A generated script is an artefact the operator can READ
// before running — every value in it a literal, no token anywhere in it — which is a property no
// remote probe reaching into their machine would have.

// cmdUpdateDiscover writes the host script that answers "what is installed here?".
func cmdUpdateDiscover(a updateArgs) int {
	if a.StageDir == "" || a.KitDir == "" || a.InstanceID == "" {
		return emitErr(exitUsage, "update discover: --stage, --kit and --instance-id are required (the block computes all three)")
	}
	if !federation.ValidInstanceID(a.InstanceID) {
		return emitErr(exitUsage, "update discover: --instance-id %q is not an instance name onboarding accepts "+
			"(lowercase letters, digits and hyphens) — it is joined into host paths, so it is refused", a.InstanceID)
	}
	if err := os.MkdirAll(a.StageDir, 0o700); err != nil {
		return emitErr(exitErr, "update discover: %v", err)
	}
	in := updatecmd.DiscoverInput{
		InstanceID: a.InstanceID, Tier: a.Tier, KitDir: a.KitDir, StageDir: a.StageDir,
		KubeContext: a.KubeContext, Kubeconfig: a.Kubeconfig, CPURL: a.CPURL,
		Folders: routerFolders(a.RouterState, a.InstanceID), GrafanaURL: discoverGrafana(a.Tier),
	}
	path := filepath.Join(a.StageDir, "discover.sh")
	if err := os.WriteFile(path, []byte(updatecmd.RenderDiscover(in)), 0o700); err != nil {
		return emitErr(exitErr, "update discover: %v", err)
	}
	emit(map[string]any{"wrote": path, "next": "bash " + path})
	return exitOK
}

// cmdUpdatePlan turns the observation into plan.json, preflight.sh and apply.sh.
//
// ⛔ A MISSING OR UNPARSEABLE observed.json IS A HARD STOP. The alternative — planning from an empty
// reading — is not a cautious default: it describes a BARE machine, so every step becomes "install
// this", performed over the top of a live instance.
func cmdUpdatePlan(a updateArgs) int {
	var obs updatecmd.Observed
	var installed updatecmd.Manifest
	var siblings []updatecmd.Sibling
	// ⛔ ROUTER-ONLY PLANS FROM NO OBSERVATION, AND ONLY ROUTER-ONLY DOES (C-24). The router is machine-wide
	// and its move reads the running container itself at apply time; every other plan is refused without
	// the machine's reading, because planning an INSTANCE from nothing treats a live machine as bare.
	if !a.RouterOnly {
		if a.ObservedPath == "" {
			return emitErr(exitUsage, "update plan: --observed is required — it is the only thing the planner "+
				"learns about this machine from. Run `update discover` and the script it writes first")
		}
		b, err := os.ReadFile(a.ObservedPath)
		if err != nil {
			return emitErr(exitErr, "update plan: read %s: %v — run `update discover` and its script first", a.ObservedPath, err)
		}
		if err := json.Unmarshal(b, &obs); err != nil {
			return emitErr(exitErr, "update plan: %s is not readable as an observation: %v. That is a hard stop: "+
				"planning from an unreadable reading would treat a live machine as bare", a.ObservedPath, err)
		}
		// An absent manifest is a PRE-0.3.32 instance, which is the normal case on this release. The
		// planner reconstructs what it needs from the observation.
		installed, _ = updatecmd.ReadManifest(a.RouterState, a.InstanceID)
		siblings = planSiblings(a, obs.Folders)
	}
	applyPlanDefaults(&a, obs)
	if !a.RouterOnly {
		if !federation.ValidInstanceID(a.InstanceID) {
			return emitErr(exitUsage, "update plan: --instance-id %q is not an instance name onboarding accepts "+
				"(lowercase letters, digits and hyphens) — it is joined into host paths, so it is refused", a.InstanceID)
		}
		// ⛔ AN UNORDERABLE VERSION SWITCHES THE GATES OFF (V32 adversary gate). An image built without its
		// VERSION stamp reports `0.0.0-dev` or `0.0.0-src+<rev>`; neither orders, so the downgrade gate would
		// pass, a folder shared with an older sibling would not be held back, and no rollback would be
		// offered afterwards. A release image is always stamped; anything else says which version it is.
		if !updatecmd.ValidVersion(a.Version) {
			return emitErr(exitUsage, "update plan: the version %q cannot be ordered, and the downgrade gate and "+
				"the shared-folder hold-back both compare versions — this image carries no release stamp. "+
				"Pass --version <x.y.z>", a.Version)
		}
		// ⛔ A ROLLBACK LANDS THE PREVIOUS RELEASE'S KIT (V32 adversary gate). This verb runs in the NEWER
		// image, so its own bundle is the wrong kit to put back; the rollback block extracts the previous
		// image's bundle with that image's `init` and names it here.
		if a.RollbackTo != "" && a.Bundle == "" {
			return emitErr(exitUsage, "update plan: --rollback-to %s needs --bundle <dir> holding the PREVIOUS "+
				"release's kit (`docker run <previous image> init <dir>`). Staged from this image, the rollback "+
				"would put this release's kit, skills and dashboard back and record them at %s", a.RollbackTo, a.RollbackTo)
		}
	}

	in := updatecmd.PlanInput{
		InstanceID: a.InstanceID, Tier: a.Tier, Image: a.ImageTag, ImageDigest: a.ImageDigest,
		Version: a.Version, KitDir: a.KitDir, StageDir: a.StageDir, RouterState: a.RouterState,
		CPURL: a.CPURL, KubeContext: a.KubeContext, Kubeconfig: a.Kubeconfig, RunnerImage: a.RunnerImage,
		KitInstances: kitInstances(a.KitDir), Observed: obs, Installed: installed,
		Siblings: siblings, RollbackTo: a.RollbackTo, RouterOnly: a.RouterOnly,
		Now: time.Now().UTC().Format(time.RFC3339),
	}
	// ⛔ STAGE THE KIT BEFORE THE SCRIPTS THAT USE IT. A-1's first act is
	// `mv "$STAGE/kit" "$KIT"`, so without this the apply dies on its first line having already
	// renamed the operator's kit away — the one failure the whole trap design exists to avoid,
	// reached before any step could register itself for undo.
	//
	// It is the SAME extraction `init` performs, from the same bundle, because the kit the update
	// lands must be the one this image ships. Staging it HERE (not in apply.sh) is also what lets
	// a failed extraction abort while nothing on the machine has moved.
	if err := stageKit(a.StageDir, a.Bundle); err != nil {
		return emitErr(exitErr, "update plan: %v", err)
	}
	if err := updatecmd.WritePlan(in); err != nil {
		return emitErr(exitErr, "update plan: %v", err)
	}
	p, _ := updatecmd.BuildPlan(in)
	skipped := []string{}
	for _, s := range p.Steps {
		if s.SkipReason != "" {
			skipped = append(skipped, s.ID+": "+s.SkipReason)
		}
	}
	emit(map[string]any{
		"plan": filepath.Join(a.StageDir, "plan.json"), "steps": len(p.Steps), "skipped": skipped,
		"next": "bash " + filepath.Join(a.StageDir, "preflight.sh"),
	})
	return exitOK
}

// routerFolders reads the agent folders from the ROUTER's own records.
//
// ⛔ NEVER A SECOND LIST. A list maintained here would drift from the one the router actually serves,
// and the update would install skills into folders nobody reads while missing the ones they do.
//
// ⛔ ONLY THE FOLDERS WIRED TO THIS INSTANCE, WITH THE HAT THE ROUTER RECORDS. The router's table holds
// every folder on the machine; a folder whose upstreams do not include this instance belongs to others,
// and installing this release's skills there is the overwrite the shared-folder rule exists to stop.
// The keys are router.Folder's own field names (internal/router/table.go:75-92, no json tags) and
// Upstreams is keyed by instance id (registry.go:215).
func routerFolders(routerState, instanceID string) []updatecmd.Folder {
	if routerState == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(routerState, "state.json"))
	if err != nil {
		return nil
	}
	var st struct {
		Folders []struct {
			Path      string                     `json:"Path"`
			Hat       string                     `json:"Hat"`
			Upstreams map[string]json.RawMessage `json:"Upstreams"`
		} `json:"folders"`
	}
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	var out []updatecmd.Folder
	for _, f := range st.Folders {
		if f.Path == "" {
			continue
		}
		if _, wired := f.Upstreams[instanceID]; !wired {
			continue
		}
		out = append(out, updatecmd.Folder{Path: f.Path, Hat: f.Hat})
	}
	return out
}

// kitInstances lists every instance this kit holds (C-7). A-1 carries ALL of their per-instance state
// across the swap; carrying only the one being updated de-onboards the rest silently.
func kitInstances(kitDir string) []string {
	if kitDir == "" {
		return nil
	}
	// ⛔ THE ENV FILES ARE UNDER deploy/compose/, NOT AT THE KIT ROOT (onboard.sh:1149, $COMPOSE_DIR).
	// Scanning the root found ZERO instances on every real kit, and zero means A-1 falls back to
	// carrying only the instance being updated — silently de-onboarding every OTHER instance the kit
	// holds. That is the exact catastrophe C-7 exists to prevent, and it read as working because the
	// test fixture put the files where the code looked.
	ents, err := os.ReadDir(filepath.Join(kitDir, "deploy", "compose"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		// `.sh` is excluded: a sourced helper beside the env file is not an instance.
		if strings.HasPrefix(n, "env.") && !strings.HasSuffix(n, ".sh") && n != "env.example" {
			out = append(out, strings.TrimPrefix(n, "env."))
		}
	}
	return out
}

// planSiblings asks the control plane which OTHER instances share this machine's skills folders, and
// what version each is on (SA-D25).
//
// ⛔ THE LOCAL MANIFESTS ARE THE BOOTSTRAP FALLBACK, NOT THE AUTHORITY. On the first update of an
// estate the control plane holds no manifests yet, so reading the machine's own `installed/`
// directory is how the shared-folder exception works at all before the first report lands.
//
// ourFolders is this instance's OWN folders (the observation's, never a second list) — the local
// fallback needs them to find a sibling the router already knows shares one, even when that sibling
// never wrote a manifest.
func planSiblings(a updateArgs, ourFolders []updatecmd.Folder) []updatecmd.Sibling {
	if a.CPURL != "" && a.Token != "" {
		if sibs, err := fetchSiblings(a); err == nil {
			return sibs
		}
		// ⛔ AND AN UNREACHABLE CONTROL PLANE MUST NOT MEAN "NOBODY SHARES ANYTHING". That reading
		// would overwrite a sibling's skills precisely when we know least. Fall through to local.
	}
	return localSiblings(a, ourFolders)
}

func fetchSiblings(a updateArgs) ([]updatecmd.Sibling, error) {
	req, err := http.NewRequest("GET", a.CPURL+"/fed/installed/siblings", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the control plane answered %d", resp.StatusCode)
	}
	var out struct {
		Siblings []updatecmd.Sibling `json:"siblings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Siblings, nil
}

func localSiblings(a updateArgs, ourFolders []updatecmd.Folder) []updatecmd.Sibling {
	ids := map[string]bool{}
	if ents, err := os.ReadDir(filepath.Join(a.RouterState, "installed")); err == nil {
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			ids[strings.TrimSuffix(e.Name(), ".json")] = true
		}
	}
	// ⛔ A SIBLING CAN SHARE A FOLDER WITHOUT EVER HAVING WRITTEN A MANIFEST (onboarded before
	// recordInstalledManifest existed, or by the predecessor tool). Enumerating ids from installed/
	// alone reads that as "not a sibling" instead of "unknown" — the router's own wiring is the one
	// place that still knows such a sibling exists, independent of any manifest.
	shared := routerSiblingSkillPaths(a.RouterState, a.InstanceID, ourFolders)
	for id := range shared {
		ids[id] = true
	}
	delete(ids, a.InstanceID)

	var out []updatecmd.Sibling
	for id := range ids {
		m, err := updatecmd.ReadManifest(a.RouterState, id)
		if err != nil {
			// ⛔ UNREADABLE IS UNKNOWN, AND UNKNOWN HOLDS THE FOLDER BACK. Dropping it would read as
			// "shares nothing", which is the assumption that overwrites a sibling's skills.
			out = append(out, updatecmd.Sibling{InstanceID: id, SkillsHostPaths: shared[id]})
			continue
		}
		s := updatecmd.Sibling{InstanceID: id, InstalledVersion: m.Version, SkillsHostPaths: shared[id]}
		for _, art := range m.Artefacts {
			if art.Kind == "skills" && art.Loc != nil && art.Loc.HostPath != "" {
				s.SkillsHostPaths = append(s.SkillsHostPaths, art.Loc.HostPath)
			}
		}
		out = append(out, s)
	}
	return out
}

// routerSiblingSkillPaths finds every OTHER instance the router's own state records as wired to a
// folder THIS instance also uses, keyed by instance id, with the folder path(s) shared. This is
// independent of whether either instance ever wrote an installed manifest — it is what lets a
// manifest-less sibling still be seen as present rather than absent.
func routerSiblingSkillPaths(routerState, instanceID string, ourFolders []updatecmd.Folder) map[string][]string {
	if routerState == "" || len(ourFolders) == 0 {
		return nil
	}
	ours := map[string]bool{}
	for _, f := range ourFolders {
		ours[f.Path] = true
	}
	b, err := os.ReadFile(filepath.Join(routerState, "state.json"))
	if err != nil {
		return nil
	}
	var st struct {
		Folders []struct {
			Path      string                     `json:"Path"`
			Upstreams map[string]json.RawMessage `json:"Upstreams"`
		} `json:"folders"`
	}
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	out := map[string][]string{}
	for _, f := range st.Folders {
		if f.Path == "" || !ours[f.Path] {
			continue
		}
		for id := range f.Upstreams {
			if id == instanceID {
				continue
			}
			out[id] = append(out[id], f.Path)
		}
	}
	return out
}

// discoverGrafana is where the presence probe looks. On compose it is the provisioner's own
// unauthenticated localhost endpoint; on the k8s tiers the credential is re-discovered at RUN time by
// the host script (SA-D24), so nothing is rendered here.
func discoverGrafana(tier string) string {
	// onboard.sh:3016 (k3d) and :3183 (compose) both provision the SHARED Grafana at localhost:3000.
	// Returning "" for k3d recorded it unreachable, and A-4 was then skipped on every k3d instance.
	if tier == "compose" || tier == "k3d" {
		return "http://localhost:3000"
	}
	return ""
}

// stageKit extracts the image's onboarding kit into $STAGE/kit, ready for A-1 to swap in.
//
// ⛔ IT REFUSES RATHER THAN LANDING AN EMPTY KIT. An absent bundle means this is not the executor
// image, and a plan that staged nothing would produce an apply.sh whose first act renames the
// operator's real kit away and replaces it with an empty directory.
//
// from, when set, is the bundle to stage instead of this image's own: a rollback names the PREVIOUS
// image's bundle, extracted by that image's `init` (the rollback block does it).
func stageKit(stage, from string) error {
	src := bundleDir
	if v := os.Getenv("ARGUS_BUNDLE_DIR"); v != "" {
		src = v
	}
	if from != "" {
		src = from
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return fmt.Errorf("no onboarding bundle at %s — `update plan` must run from the "+
			"onedroid-testing-suite image, which is what carries the kit the update lands", src)
	}
	dst := filepath.Join(stage, "kit")
	// A previous plan's staging is stale by definition: the block clears $STAGE first, but a re-run
	// of `update plan` alone must not merge two kits.
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if _, err := copyTree(src, dst); err != nil {
		return fmt.Errorf("staging the kit into %s: %w", dst, err)
	}
	return nil
}

// cmdUpdateRehash writes $STAGE/manifest.next.json — the manifest `update commit` then stores.
//
// ⭐ A-8a CONSUMED A FILE NOTHING WROTE. `apply.sh` ended with
// `update commit --manifest "$STAGE/manifest.next.json"` and no step produced it. This is the
// producer, and it is a Go verb rather than bash for the reason the row already gave: ONE writer of
// the manifest, so the onboard path and the update path cannot describe the same machine differently.
//
// ⛔ AND IT READS PROGRESS, NOT THE PLAN (C-15). The plan says where the machine was going; progress
// says what actually happened to each step. A skipped, undone, failed or never-reached step means the
// artefact did NOT move, and it is recorded where the machine really has it.
func cmdUpdateRehash(a updateArgs) int {
	if a.StageDir == "" {
		return emitErr(exitUsage, "update rehash: --stage is required")
	}
	var p updatecmd.Plan
	if err := readJSONFile(filepath.Join(a.StageDir, "plan.json"), &p); err != nil {
		return emitErr(exitErr, "update rehash: %v", err)
	}
	var obs updatecmd.Observed
	// An unreadable observation is not fatal HERE: the plan already refused without one, and by this
	// point the machine has been changed. What it costs is the fallback version, which degrades to
	// "unknown" — an honest answer rather than a missing manifest.
	_ = readJSONFile(filepath.Join(a.StageDir, "observed.json"), &obs)

	progress, err := readProgress(filepath.Join(a.StageDir, "progress.json"))
	if err != nil {
		return emitErr(exitErr, "update rehash: %v", err)
	}
	m := updatecmd.Rehash(p, progress, obs, a.Outcome, a.FailedStep)
	// AC-D53, design step 8: HOW A-3's health check failed travels with the outcome, so the control plane can tell
	// "could not reach" from "seen unhealthy" across an estate
	if h, d := readProgressHealth(filepath.Join(a.StageDir, "progress.json"), "A-3"); h != "" && m.LastOutcome != nil {
		m.LastOutcome.Health, m.LastOutcome.Detail = h, d
	}

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return emitErr(exitErr, "update rehash: %v", err)
	}
	out := filepath.Join(a.StageDir, "manifest.next.json")
	if err := os.WriteFile(out, b, 0o600); err != nil {
		return emitErr(exitErr, "update rehash: %v", err)
	}
	emit(map[string]any{"wrote": out, "version": m.Version, "artefacts": len(m.Artefacts), "outcome": a.Outcome})
	return exitOK
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s is not readable: %w", path, err)
	}
	return nil
}

// readProgress reads apply.sh's per-step record. It is JSON LINES, appended after every step, because
// a single document would have to be rewritten each time and a crash mid-rewrite loses the whole
// history — exactly when it matters most.
//
// ⛔ A MISSING FILE IS AN EMPTY RECORD, NOT AN ERROR, and empty means NOTHING APPLIED. A preflight
// abort leaves no progress at all, and the honest manifest for that is "nothing moved".
func readProgress(path string) (map[string]string, error) {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.ID == "" {
			continue
		}
		// LAST writing wins: a step applied then undone is UNDONE.
		out[e.ID] = e.State
	}
	return out, nil
}

// readProgressHealth is the LAST health verdict apply.sh recorded for step id — `unhealthy` or `unreachable`, and
// the one line that decided it — or "" when that step never failed a health check. Only the move's own failed
// check writes one (scripts_steps.go move_executor); its undo and its clean end write none, so the last record
// that carries a verdict is the move's, whatever came after it. Unreadable is "": this is evidence, never a gate.
func readProgressHealth(path, id string) (health, detail string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		var e struct {
			ID     string `json:"id"`
			Health string `json:"health"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) != nil || e.ID != id || e.Health == "" {
			continue
		}
		health, detail = e.Health, e.Detail
	}
	return health, detail
}

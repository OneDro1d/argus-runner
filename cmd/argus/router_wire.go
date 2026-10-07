package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/agentcfg"
	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
)

// routerServerKey is the ONE MCP server key an agent folder gets after M3-FX.
//
// The solution-architect document specifies `argus_<instance_id>` for the PRE-router shape, where
// each entry addressed one executor directly and a fixed key meant a second instance deleted the
// first. Behind the router that problem dissolves: one entry reaches every instance the folder is
// permitted to reach, so per-instance keys would produce N identical entries — same URL, same token,
// N copies of the same tool list — differing in nothing. §3.2 of that document anticipates this
// ("after the router, each folder's file holds one entry"); this constant is that sentence.
//
// VR-A2 is satisfied more strongly as a result: onboarding a second instance cannot delete the
// first, because the entry does not vary with the instance at all.
const routerServerKey = "argus"

// mcpJSONName is the file every agent folder holds. Named once, because two spellings of it in
// different code paths is how the pre-M3-FX shell ended up with two writers.
const mcpJSONName = ".mcp.json"

// settingsRelPath is the OTHER file an agent folder holds. `.mcp.json` registers the server;
// `.claude/settings.local.json` is what makes the client TRUST it, so writing one without the other
// leaves an agent that can see the server and refuses to use it.
var settingsRelPath = filepath.Join(".claude", "settings.local.json")

// settingsPathFor derives the settings file from wherever .mcp.json is being written, so the
// container's bind-mount path carries to both.
func settingsPathFor(mcpPath string) string {
	return filepath.Join(filepath.Dir(mcpPath), settingsRelPath)
}

// wireOutcome is what `router wire` reports. It is deliberately explicit about which half of the
// operation did what: an operator reading this after a failure needs to know whether the ROUTING
// exists (state) or only the CONFIG (file), because the two failure modes look identical from the
// agent and have opposite fixes.
type wireOutcome struct {
	Folder          string `json:"folder"`
	Hat             string `json:"hat"`
	Instance        string `json:"instance"`
	Port            int    `json:"port"`
	RouterURL       string `json:"router_url"`
	ServerKey       string `json:"server_key"`
	MCPJSON         string `json:"mcp_json"`
	Settings        string `json:"settings_json"`
	FolderCreated   bool   `json:"folder_created"`
	FileCreated     bool   `json:"file_created"`
	SettingsCreated bool   `json:"settings_created"`
	Instances       int    `json:"instances_in_folder"`
	CloudWired      bool   `json:"cloud_wired"`
	// RouterToken is returned so a caller can verify the wiring end-to-end. It is NOT printed by
	// `router wire` — see cmdRouterWire.
	RouterToken string `json:"-"`
}

// mcpJSONPathFor decides WHERE to write, which is not always the same question as which folder is
// being recorded. Onboarding runs this command in a container with the agent folder bind-mounted at
// a container path, while the folder's IDENTITY must stay the host path the router will match on.
// The override carries the write target; the spec carries the identity.
func mcpJSONPathFor(folderPath, override string) string {
	if override != "" {
		return override
	}
	return filepath.Join(folderPath, mcpJSONName)
}

// wireFolder is the whole join between onboarding and the router: it records the route, then writes
// the agent's config to match. Both halves, one call, one place — because the shell doing them as two
// steps is precisely how a half-wired folder happens.
//
// ORDER IS LOAD-BEARING, and it is state-then-file:
//
//   - state first, file second → a failure leaves a routing entry nothing uses. The agent's existing
//     config is untouched and keeps working. Harmless, and re-running fixes it.
//   - file first, state second → a failure leaves the agent pointing at a router token that
//     authenticates nobody. Every tool call returns -32001 and the folder is stranded.
//
// The same asymmetry governs the cutover as a whole (see M3-FX-CUTOVER-RUNBOOK.md), and it is worth
// stating twice: recovery is always FORWARD, so the step that can be re-run must be the one that
// fails.
func wireFolderAt(stateDir string, spec router.FolderSpec, serverKey, mcpJSONOverride string) (wireOutcome, error) {
	st, err := router.LoadState(stateDir)
	if err != nil {
		return wireOutcome{}, err
	}
	port, err := router.PortForWiring(st.Port)
	if err != nil {
		return wireOutcome{}, err
	}
	st.Port = port

	st, folder, folderCreated, err := router.UpsertFolder(st, spec)
	if err != nil {
		return wireOutcome{}, err
	}
	if err := router.SaveState(stateDir, st); err != nil {
		return wireOutcome{}, fmt.Errorf("router: could not persist the routing table: %w", err)
	}

	routerURL := fmt.Sprintf("http://%s/mcp", router.ListenAddr(port))
	entry, err := json.Marshal(map[string]any{
		"type":    "http",
		"url":     routerURL,
		"headers": map[string]string{"Authorization": "Bearer " + folder.Token},
	})
	if err != nil {
		return wireOutcome{}, err
	}

	mcpPath := mcpJSONPathFor(folder.Path, mcpJSONOverride)
	fileCreated, err := agentcfg.Merge(mcpPath, serverKey, entry)
	if err != nil {
		return wireOutcome{}, fmt.Errorf("router: the route is recorded but %s was not written (%w) — the folder is not stranded: its previous config is untouched, and re-running this command completes the wiring", mcpPath, err)
	}

	// Record the provenance only NOW, because only now is it known. A failure to persist this second
	// write costs the "we created it" bit, so teardown would later decline to DELETE the file and
	// merely empty it — the safe direction, and the reason this is not worth a rollback.
	if fileCreated {
		if f, _ := router.FindFolder(st, folder.Path); f != nil && !f.MCPJSONCreated {
			f.MCPJSONCreated = true
			if err := router.SaveState(stateDir, st); err != nil {
				return wireOutcome{}, fmt.Errorf("router: wired, but could not record that onboarding created %s (%w) — teardown will empty that file rather than delete it", mcpPath, err)
			}
		}
	}

	// The trust half, in the SAME command for the same reason the routing and the config are: an agent
	// folder holding a server it is not permitted to use is a failure that reads as a broken router.
	// Before M3-FX this file was written with a truncating `printf >`, which deleted whatever
	// permissions, hooks and env the user had put in it — the identical defect .mcp.json was rescued
	// from, on a file it is worse to destroy.
	settingsPath := settingsPathFor(mcpPath)
	settingsCreated, err := agentcfg.EnableServer(settingsPath, serverKey)
	if err != nil {
		return wireOutcome{}, fmt.Errorf("router: the route and %s are written, but %s was not (%w) — the agent will see the server and refuse to use it until this file lists %q", mcpPath, settingsPath, err, serverKey)
	}

	return wireOutcome{
		Folder: folder.Path, Hat: string(folder.Hat), Instance: spec.InstanceID,
		Port: port, RouterURL: routerURL, ServerKey: serverKey,
		MCPJSON: mcpPath, Settings: settingsPath,
		FolderCreated: folderCreated, FileCreated: fileCreated, SettingsCreated: settingsCreated,
		Instances: len(folder.Upstreams), CloudWired: folder.Cloud != nil,
		RouterToken: folder.Token,
	}, nil
}

// wireFolder is wireFolderAt with the write target defaulted to <folder>/.mcp.json — the host-side
// invocation, where the folder's path and its bind-mount path are the same thing.
func wireFolder(stateDir string, spec router.FolderSpec, serverKey string) (wireOutcome, error) {
	return wireFolderAt(stateDir, spec, serverKey, "")
}

type unwireOutcome struct {
	Folder          string `json:"folder"`
	Instance        string `json:"instance"`
	Removed         bool   `json:"removed"`
	FolderDropped   bool   `json:"folder_dropped"`
	EntryRemoved    bool   `json:"entry_removed"`
	FileDeleted     bool   `json:"file_deleted"`
	MCPJSON         string `json:"mcp_json"`
	SettingsUpdated bool   `json:"settings_updated"`
	SettingsDeleted bool   `json:"settings_deleted"`
	// VR-L4: what onboarding wrote into the folder and teardown took back out. Reported rather than
	// silent — this round exists because operations that did not say what they did were trusted.
	OnboardingFilesRemoved []string `json:"onboarding_files_removed,omitempty"`
	PurgeWarnings          []string `json:"purge_warnings,omitempty"`
	// PurgeRefused carries the REASON the file removal was declined, when the base directory
	// could not be established as this instance's agent folder (VR8-S1). The routing removal still
	// happened; only the deletion was withheld. Empty on every ordinary teardown.
	PurgeRefused string `json:"purge_refused,omitempty"`
}

// unwireFolder is teardown's half (VR-A5). The MCP entry is removed only when the folder's LAST
// instance leaves — while any instance remains, the entry is still the folder's route to it.
//
// Nothing here treats "it was not there" as a failure. Teardown is re-run after partial failures by
// design, and on machines where the instance was never wired at all.
// overrideShapeRefusal rejects a --mcp-json value that cannot name an agent folder's config.
//
// It is deliberately a SHAPE rule and nothing more, so it can run before the file is opened.
// Whether the file really belongs to a folder this router wired is a separate, stronger question,
// answered in purgeBase once the edit has told us whether our entry was actually in it.
//
// An empty override is not a refusal: it means the caller supplied no path, and the router's own
// record is used instead.
// containerFolderMount is the ONE path an agent folder is ever visible at inside the container.
//
// It is a CONTRACT the kit establishes on the very command line that names --folder:
//
//	docker run -v "$(hostpath "$RF")":/folder ... router unwire --folder "$RF" --mcp-json /folder/.mcp.json
//
// The docker run in onboarding/teardown.sh that binds `:/folder`, onboard.sh's argus_wire and its argus_agentcfg all mount the
// folder at /folder and mount NOTHING else. So the MOUNT is what ties the override to the folder,
// and the override string only names where that mount appears.
//
// ⚠ Tests point this at a temp dir. No production path writes it.
var containerFolderMount = "/folder"

func overrideShapeRefusal(override string) string {
	if override == "" {
		// No path supplied: the router's own record is used, and no caller string reaches the purge.
		return ""
	}
	if filepath.Base(override) != mcpJSONName {
		return fmt.Sprintf("refused to touch onboarding files: --mcp-json points at %q, which is not a %s, "+
			"so the directory holding it cannot be shown to be this instance agent folder", override, mcpJSONName)
	}
	// 🚨 THE COPY HOLE, found by an adversary and reproduced before this line existed.
	//
	// The check below this one proves the file carries THIS folder's router token. A token is
	// CONTENT, and content copies: `cp -r` of an agent folder produces a second directory whose
	// .mcp.json holds the same token. Demonstrated -- unwiring with --mcp-json pointing at the
	// operator's BACKUP purged the backup, left the real folder's skills in place, and reported
	// PurgeRefused empty. The token proved the file was a COPY OF ours, which is not the question.
	//
	// The question is WHICH DIRECTORY, and inside the container only one is reachable: the mount.
	// A path anywhere else is either a mistake or an attempt, and both are refused.
	if filepath.Clean(filepath.Dir(override)) != filepath.Clean(containerFolderMount) {
		return fmt.Sprintf("refused to touch onboarding files: --mcp-json points at %q, but an agent folder "+
			"is only ever visible at %s in here -- it is bind-mounted there by the same command that names "+
			"--folder. A path anywhere else names a directory this router cannot show belongs to the instance "+
			"being torn down, and a copy of the folder would carry its token too.", override, containerFolderMount)
	}
	return ""
}

// purgeBaseFor decides WHERE unwire may delete files, and REFUSES when it cannot prove the
// directory belongs to the instance being torn down.
//
// 🚨 WHAT THE PREVIOUS VERSION GOT WRONG, found by an adversary and reproduced as a test.
//
// It accepted an override on two conditions: the basename is `.mcp.json`, and our entry was
// found in it. Both are CALLER-SUPPLIED:
//
//   - the second was checked with `serverKey`, which `argus router unwire` exposes as
//     `--server-key`. So the caller named the file AND named the key whose presence authorised
//     deleting things next to it.
//   - and every folder this router ever wired contains a `argus` entry, so the check passed
//     for any of them — including another live instance's folder.
//
// Demonstrated: unwiring instance A with `--mcp-json <folder-B>/.mcp.json` deleted folder B's
// skills, its `.argus/instance.json` and its `.mcp.json`, leaving instance B unroutable,
// and reported PurgeRefused as empty. The comment claiming this was "a fact about the WORLD"
// was wrong, and no test exercised it — deleting both guards left the suite green.
//
// THE FIX: the proof is the ROUTER TOKEN this folder was wired with (RemoveResult.Token). It is
// a per-folder secret that only appears in that folder's own .mcp.json. A caller cannot name a
// different folder's file and have it match, and `--server-key` cannot defeat it because the
// token is checked, not the key's name.
//
// ⚠ Why not compare paths: teardown runs this INSIDE A CONTAINER where the folder is
// bind-mounted at a path that is legitimately not res.Path. A string comparison would refuse
// every real teardown while proving nothing a rename could not defeat. The token holds across
// the mount because it is content, not location.
func purgeBaseFor(res router.RemoveResult, override string, raw []byte) (base, refusal string) {
	if override == "" {
		// Router-derived: no caller-supplied string reaches this path.
		return res.Path, ""
	}
	if refusal := overrideShapeRefusal(override); refusal != "" {
		return "", refusal
	}
	if res.Token == "" {
		return "", fmt.Sprintf("refused to remove onboarding files beside %q: this router has no "+
			"recorded token for the folder being unwired, so the file cannot be shown to belong to it", override)
	}
	if !bytes.Contains(raw, []byte(res.Token)) {
		return "", fmt.Sprintf("refused to remove onboarding files beside %q: it does not carry the "+
			"router token this instance's folder was wired with, so it is a DIFFERENT folder", override)
	}
	return filepath.Dir(override), ""
}

func unwireFolder(stateDir, folderPath, instanceID, serverKey string) (unwireOutcome, error) {
	return unwireFolderAt(stateDir, folderPath, instanceID, serverKey, "", false)
}

func unwireFolderAt(stateDir, folderPath, instanceID, serverKey, mcpJSONOverride string, routingOnly bool) (unwireOutcome, error) {
	st, err := router.LoadState(stateDir)
	if err != nil {
		return unwireOutcome{}, err
	}
	st, res, err := router.RemoveInstance(st, folderPath, instanceID)
	if err != nil {
		return unwireOutcome{}, err
	}
	out := unwireOutcome{
		Folder: res.Path, Instance: instanceID,
		Removed: res.Removed, FolderDropped: res.FolderDropped,
		MCPJSON: mcpJSONPathFor(res.Path, mcpJSONOverride),
	}
	if !res.Removed {
		return out, nil
	}
	if err := router.SaveState(stateDir, st); err != nil {
		return out, fmt.Errorf("router: could not persist the routing table: %w", err)
	}
	if !res.FolderDropped {
		return out, nil
	}
	// routingOnly is teardown's case for a folder the operator has already DELETED from disk. The
	// routing entry must still go — a folder record left behind keeps a router token authenticating
	// for a directory nobody has — but there is no file to edit, and reporting a failure to edit one
	// would make a clean teardown look broken.
	if routingOnly {
		return out, nil
	}
	// VR8-S1: the override's SHAPE is checked BEFORE anything is opened.
	//
	// Order matters and was got wrong once already: with the check placed after the edit, a
	// `--mcp-json .../notes.txt` made agentcfg parse the operator's own file as JSON and fail,
	// so the run touched a stranger's file and returned an error instead of declining cleanly.
	// Checking first means a path that is not a .mcp.json is never opened at all.
	if refusal := overrideShapeRefusal(mcpJSONOverride); refusal != "" {
		out.PurgeRefused = refusal
		return out, nil
	}
	// deleteIfEmpty carries the provenance: a file the USER wrote is emptied of our entry and kept,
	// a file ONBOARDING created is removed outright, because an abandoned {"mcpServers":{}} is the
	// exact trace VR-A5 exists to prevent.
	// VR8-S1 / gate F1: read the file BEFORE the edit, so the purge can prove whose it is.
	//
	// agentcfg.Remove takes our entry out. After it runs, the one piece of evidence that ties
	// this file to THIS folder — the router token it was written with — is gone. So it is read
	// here and handed to purgeBaseFor below. Nothing is deleted on the strength of it yet.
	rawMCPJSON, _ := os.ReadFile(out.MCPJSON)

	// The ownership decision is made HERE, BEFORE anything is written or deleted.
	//
	// It was first placed after the edit below. That protected the skills and the instance binding
	// but NOT the file itself: agentcfg.Remove deleted ANOTHER instance's .mcp.json outright, because
	// res.MCPJSONCreated is THIS folder's provenance bit applied to THAT folder's file. Tearing down
	// instance A left instance B unroutable — and the test said so, which is why the guard moved.
	//
	// Do not touch a file you cannot prove is yours.
	purgeBase, purgeRefusal := purgeBaseFor(res, mcpJSONOverride, rawMCPJSON)
	if purgeRefusal != "" {
		// REFUSED, NOT SILENTLY SKIPPED. A run that declined to do something and did not say so is
		// the exact under-reporting round 8 exists to fix. The ROUTE is already gone from the
		// router's table, which is the part that matters; nothing on disk is touched.
		out.PurgeRefused = purgeRefusal
		return out, nil
	}
	removed, fileDeleted, err := agentcfg.Remove(out.MCPJSON, serverKey, res.MCPJSONCreated)
	if err != nil {
		return out, fmt.Errorf("router: the route is gone but %s was not updated: %w", out.MCPJSON, err)
	}
	out.EntryRemoved, out.FileDeleted = removed, fileDeleted

	// The trust half comes out too. `DisableServer` deletes the settings file only when OUR entry was
	// the only element of the only member — i.e. when nothing of the user's is in it. There is
	// deliberately no second provenance bit for this file: a settings.local.json holding exactly
	// `{"enabledMcpjsonServers":["argus"]}` is functionally ours whoever typed it, and a second
	// recorded flag is a second thing that can drift from the truth on disk.
	sUpdated, sDeleted, serr := agentcfg.DisableServer(settingsPathFor(out.MCPJSON), serverKey, true)
	if serr != nil {
		return out, fmt.Errorf("router: the route is gone but %s was not updated: %w", settingsPathFor(out.MCPJSON), serr)
	}
	out.SettingsUpdated, out.SettingsDeleted = sUpdated, sDeleted

	// VR-L4 (V17-014): remove what ONBOARDING wrote, and nothing the user owns.
	//
	// Reached only when the folder's LAST instance has left, so this file no longer describes
	// anything. Left behind, `.argus/instance.json` names a DEREGISTERED instance — and the shipped
	// scenario-runner skill instructs the agent to read the instance id from exactly that file, so
	// teardown was leaving an instruction to address something that does not exist.
	//
	// 🚨 TEARDOWN DOES NOT REMOVE SKILLS. AT ALL. Owner ruling 2026-08-21, which takes the whole topic
	// out of this round:
	//
	//   "lets move it out from the current build scope with remark that this whole topic must be
	//    additionally analyzed because one test or prod agent folder can be used by multiple
	//    instances" ... "teardown do not remove any previously installed skills at all"
	//
	// It supersedes D7 in all three of its positions, and D2 from round 3 before them.
	//
	// ⚠ THIS IS NOT A REVERT, and reverting would be the dangerous move. The SHIPPED code (0.3.25, live
	// on 7 instances) does `RemoveAll(base/.claude/skills)` — the whole DIRECTORY, so a skill the USER
	// wrote goes with it. Proven, not inferred: the owner's product repo tracked four skills and exactly
	// the two Argus installs survived a teardown plus re-onboard, because a wipe-then-reinstall is
	// indistinguishable from a correct install and every automated signal stayed green through it.
	// Removing nothing is strictly safer than either the old behaviour or the by-name version built
	// this round, and it closes that data-loss path by DOING NOTHING rather than by doing something
	// clever.
	//
	// WHY THE TOPIC NEEDS ITS OWN ANALYSIS, in the owner's words: one product or test agent folder can
	// serve MULTIPLE instances. A last-instance guard (res.FolderDropped, still in force above) answers
	// "has everyone left?" — it does not answer "does the user still want these skills?", and those are
	// different questions. Skills are useful on their own, independently of any instance.
	//
	// ⛔ STILL OPEN AND NOT ADDRESSED HERE — onboarding/onboard.sh's `install_skills` does
	// `rm -rf "$dir/.claude/skills/$s"` then `cp -r`, so a user's OWN skill sharing a Argus name is
	// destroyed at INSTALL time, before teardown is ever involved. Live today. Same topic, deferred with
	// it — see the design notes
	//
	// `scenarios/` is NEVER touched either. That is the half of this rule that must not be got wrong:
	// they are the user's own work, and removing them would be far worse than the stale binding this
	// fixes. Nothing here treats absence as failure, for the same reason the rest of unwire does not:
	// teardown is re-run over partial state by design.
	base := purgeBase
	rels := []string{filepath.Join(".argus", "instance.json")}
	for _, rel := range rels {
		if err := os.RemoveAll(filepath.Join(base, rel)); err != nil {
			// Reported, never fatal: the ROUTE is already gone, which is the part that matters, and a
			// teardown that aborts here would leave the caller unable to finish for a file.
			out.PurgeWarnings = append(out.PurgeWarnings, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		out.OnboardingFilesRemoved = append(out.OnboardingFilesRemoved, rel)
	}

	// VR8-S2 (V26-009): the directories go once nothing is left in them.
	//
	// Teardown used to remove the CONTENTS it wrote and leave the empty parents, so a folder
	// restored from the pristine archive reported `DIRTY: .claude .argus` while holding zero
	// residue. That fails in the SAFE direction, unlike the rest of this round — but the cost is
	// trust: the SAME check has to catch a real stale .mcp.json, which still carries a live router
	// token, and an operator who learns to wave the false positive away waves that away too.
	//
	// EMPTY is the predicate, never EXISTS — and os.Remove IS the predicate. It refuses a
	// non-empty directory, so the safety property is enforced by the syscall rather than by a
	// check of ours that could be wrong. A provenance bit was considered (SA §0.7) and rejected:
	// an empty directory holds nothing of the user's BY CONSTRUCTION, so the risk it would guard
	// against is already zero, and a new state field would help no folder already onboarded.
	//
	// Deepest first. `.claude/skills` must go before `.claude` can be empty.
	//
	// A failure here is NOT reported, and that is deliberate rather than the under-reporting this
	// round is fixing: "directory not empty" is the CORRECT and expected outcome whenever the
	// user keeps anything of their own there. Nothing is claimed about it either way — the
	// directory only enters OnboardingFilesRemoved when it actually went.
	for _, rel := range []string{filepath.Join(".claude", "skills"), ".claude", ".argus"} {
		if err := os.Remove(filepath.Join(base, rel)); err == nil {
			out.OnboardingFilesRemoved = append(out.OnboardingFilesRemoved, rel)
		}
	}
	return out, nil
}

// ── CLI ──────────────────────────────────────────────────────────────────────────────────────────

func cmdRouterWire(args []string) int {
	fs := flag.NewFlagSet("router wire", flag.ContinueOnError)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table + port")
	folder := fs.String("folder", "", "the agent folder to wire (its .mcp.json is written)")
	hat := fs.String("hat", "", "product | test — the folder's hat, which decides its tool set")
	instance := fs.String("instance", "", "the instance id this folder may reach")
	execURL := fs.String("executor-url", "", "the instance's in-env executor MCP URL")
	// ── SEC-4: BOTH CREDENTIALS MAY ARRIVE BY ENVIRONMENT (gate 3) ───────────────────────────────
	//
	// `ps` shows argv to every user on the machine and `docker inspect` replays it for the
	// container's lifetime, so a token passed as `--executor-token <value>` is recorded, not merely
	// momentarily visible. onboarding/onboard.sh passes both of these into `argus_wire`, which is
	// a `docker run` — so the runner token and the author PAT are on the host's argv on every
	// router-path onboard. `--token` below has had an env default since CP-M3-III-206 for exactly
	// this reason; these two were missed.
	//
	// ⚠ THE FLAG STILL WINS WHEN GIVEN. That is what makes this safe to ship on its own: a kit that
	// still passes the flag behaves identically, so this binary is compatible with every kit, old or
	// new. The reverse is NOT true — a kit that drops the flag needs a binary with this defaulting,
	// which is why the kit-side change is gated on the image that carries it (see the onboarding
	// note at the wire_folder call site).
	execToken := fs.String("executor-token", os.Getenv("ARGUS_WIRE_EXECUTOR_TOKEN"), "the executor token for THIS hat (else ARGUS_WIRE_EXECUTOR_TOKEN)")
	cloudURL := fs.String("cloud-url", "", "control-plane MCP URL (test folders only)")
	cloudUser := fs.String("cloud-user", "", "the account subject whose record this folder uses (test folders only; omit on a single-user machine)")
	serverKey := fs.String("server-key", routerServerKey, "the .mcp.json server key to write")
	mcpJSON := fs.String("mcp-json", "", "write the entry HERE instead of <folder>/.mcp.json (for a container whose bind-mount path differs from the host path being recorded)")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}
	// SECRETS OFF ARGV. A flag value is visible in `ps` and is recorded verbatim in `docker inspect`
	// for the life of the container, so onboarding passes these through the environment instead. The
	// flags remain for interactive use, where the shell history is the operator's own problem.
	if *execToken == "" {
		*execToken = os.Getenv("ARGUS_WIRE_EXECUTOR_TOKEN")
	}
	if *folder == "" || *hat == "" || *instance == "" || *execURL == "" || *execToken == "" {
		return emitErr(exitUsage, "usage: argus router wire --folder <dir> --hat product|test --instance <id> --executor-url <url> --executor-token <tok> [--cloud-url <url> [--cloud-user <subject>]]")
	}
	h, err := role.Parse(*hat)
	if err != nil {
		return emitErr(exitUsage, "router wire: %v", err)
	}
	spec := router.FolderSpec{
		Path:       *folder,
		Hat:        h,
		InstanceID: *instance,
		Executor:   router.Upstream{URL: *execURL, Token: *execToken},
	}
	// An author credential supplied for a PRODUCT folder is refused rather than dropped. Silently
	// ignoring it would be the friendlier-looking behaviour and the wrong one: the caller believes it
	// wired the author plane, and nothing would ever say otherwise.
	//
	// V27-009 (0.3.29): a folder carries a REFERENCE {url, user} to this machine's record; the token lives
	// on the record only and never passes through a flag, a shell variable or a command line. --cloud-url
	// alone resolves the sole record for that control plane; --cloud-user names one when there are several.
	//
	// The reverse is still refused: a --cloud-user with no URL names a record for nowhere.
	//
	// This ordering is FORCED, and getting it backwards costs the token. On a first onboard there is
	// no cloud entry yet, so the mint would have nothing to write into and would report zero folders
	// updated — leaving a freshly minted credential live at the control plane with no holder on this
	// machine and no file to fall back on. Wire first (URL only), mint second (fills the token).
	if *cloudURL != "" || *cloudUser != "" {
		if *cloudURL == "" {
			return emitErr(exitUsage, "router wire: --cloud-user needs --cloud-url — a user names a record, and a record is per control plane")
		}
		// V27-009 redesign: the folder REFERS to the machine's record for (control plane, user); the token lives
		// on the record only. `router register` (onboarding step 8a) creates the record.
		spec.Cloud = &router.CloudRef{URL: *cloudURL, User: *cloudUser}
	}
	out, err := wireFolderAt(*stateDir, spec, *serverKey, *mcpJSON)
	if err != nil {
		return emitErr(exitErr, "router wire: %v", err)
	}
	// The router token is NOT printed. It is the folder's whole identity, it is already written where
	// it is needed, and onboarding logs are pasted into chats and tickets.
	emit(out)
	return exitOK
}

func cmdRouterUnwire(args []string) int {
	fs := flag.NewFlagSet("router unwire", flag.ContinueOnError)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table + port")
	folder := fs.String("folder", "", "the agent folder to unwire")
	instance := fs.String("instance", "", "the instance id to remove from that folder")
	serverKey := fs.String("server-key", routerServerKey, "the .mcp.json server key to remove")
	mcpJSON := fs.String("mcp-json", "", "act on THIS file instead of <folder>/.mcp.json (container bind-mount path)")
	routingOnly := fs.Bool("routing-only", false, "update the routing table only — for a folder already deleted from disk")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}
	if *folder == "" || *instance == "" {
		return emitErr(exitUsage, "usage: argus router unwire --folder <dir> --instance <id>")
	}
	out, err := unwireFolderAt(*stateDir, *folder, *instance, *serverKey, *mcpJSON, *routingOnly)
	if err != nil {
		return emitErr(exitErr, "router unwire: %v", err)
	}
	emit(out)
	return exitOK
}

// cmdRouterRegister posts this machine's router identity to the control plane (VR-R10).
//
// It runs at ONBOARDING time, holding the author session, because the router itself never holds one:
// a machine with only product folders has no author credential by construction. See
// CloudClient.RegisterRouter.
func cmdRouterRegister(args []string) int {
	fs := flag.NewFlagSet("router register", flag.ContinueOnError)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its identity")
	cp := fs.String("control-plane", os.Getenv("ARGUS_CP_URL"), "control-plane base URL")
	// ⛔ NO DEFAULT HERE (#44, matching up.go): a default of os.Getenv on either token name would put
	// the credential in --help's rendered usage text. Read AFTER Parse instead, below.
	token := fs.String("token", "", "an author/session token (else ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}
	if *token == "" {
		*token = envname.Lookup(envname.CPAuthorToken, envname.CPAuthorTokenDeprecated)
	}
	if *cp == "" || *token == "" {
		return emitErr(exitUsage, "usage: argus router register --control-plane <url> --token <tok>")
	}
	// LoadOrCreate, not Load: registration is usually the FIRST thing that touches the identity, and
	// requiring `router serve` to have run once would make onboarding order-dependent for no reason.
	// The id is derived from the public key, so creating it here and serving later cannot drift.
	//
	// VR9-I1 rule 3 — AND IT REPORTS WHICH IT DID. Minting is ordinary on a first onboard: this runs
	// BEFORE the router container starts, so the container simply loads what was just written. It is
	// dangerous when a router is ALREADY RUNNING — a wiped or replaced state dir — because that process
	// is then signing with a key the control plane no longer has. Rule 1 lets a current router notice
	// within a poll interval; one on an older image cannot, and 401s silently forever. Onboarding can
	// only act on that if this command says which happened.
	id, created, err := router.LoadOrCreateIdentityCreated(*stateDir)
	if err != nil {
		return emitErr(exitErr, "router register: %v", err)
	}
	host := router.HostName()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, err := onboard.NewCloudClient(*cp).RegisterRouter(ctx, *token, id.RouterID, host, id.PublicKeyB64())
	if err != nil {
		return emitErr(exitErr, "router register: %v", err)
	}
	// V27-009 redesign: the registration creates (or refreshes) this machine's record for (control plane, user).
	// The token comes later (cloud-mint-token, step 8b) and lands on this record — the ONE place it lives.
	st, lerr := router.LoadState(*stateDir)
	if lerr != nil {
		return emitErr(exitErr, "router register: %v", lerr)
	}
	replaced := st.PutRecord(router.CloudRecord{URL: recordURL(*cp), User: owner, RouterID: id.RouterID})
	if serr := router.SaveState(*stateDir, st); serr != nil {
		return emitErr(exitErr, "router register: registered with the control plane but could not record it locally: %v", serr)
	}
	emit(map[string]any{"registered": true, "router_id": id.RouterID, "host": host, "control_plane": *cp,
		"owner": owner, "record_replaced": replaced, "identity_created": created})
	return exitOK
}

// cmdRouterStatus prints the REDACTED routing table. It is the operator's answer to "what does this
// machine route, and where does the state actually live" — the question the cutover runbook asks
// before and after every step.
func cmdRouterStatus(args []string) int {
	fs := flag.NewFlagSet("router status", flag.ContinueOnError)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table + port")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}
	st, err := router.LoadState(*stateDir)
	if err != nil {
		return emitErr(exitErr, "router status: %v", err)
	}
	// VR8-K2 (V26-003): SAY WHETHER THE STATE COULD BE READ AT ALL.
	//
	// LoadState returns an EMPTY state, not an error, when state.json is absent (runtime.go:386) —
	// which is correct for a first run and catastrophic for a router whose directory was deleted
	// underneath it. Both produced `{"folders": []}`, so this command could not tell a brand-new
	// router from one that had lost everything it was serving. It answered that for 13 hours while
	// every agent on the machine was unroutable.
	//
	// The directory and the file are reported SEPARATELY because they fail for different reasons: a
	// missing directory is a mount that vanished, a missing file inside a present directory is a
	// first run OR a wipe. The caller can now tell; before, nothing could.
	stateDirPresent := false
	if fi, serr := os.Stat(*stateDir); serr == nil && fi.IsDir() {
		stateDirPresent = true
	}
	stateFilePresent := false
	if _, serr := os.Stat(filepath.Join(*stateDir, "state.json")); serr == nil {
		stateFilePresent = true
	}
	folders := make([]map[string]any, 0, len(st.Folders))
	for _, f := range st.Folders {
		ids := make([]string, 0, len(f.Upstreams))
		for id := range f.Upstreams {
			ids = append(ids, id)
		}
		fm := map[string]any{
			"path": f.Path, "hat": string(f.Hat), "instances": ids,
			"cloud_plane": f.Cloud != nil, "mcp_json_created_by_onboarding": f.MCPJSONCreated,
		}
		if f.Cloud != nil {
			fm["cloud_url"], fm["cloud_user"] = f.Cloud.URL, f.Cloud.User
		}
		folders = append(folders, fm)
	}
	// V27-009 redesign: the records — url, user, whether a token is held. NEVER the token.
	clouds := make([]map[string]any, 0, len(st.Clouds))
	for _, c := range st.Clouds {
		clouds = append(clouds, map[string]any{"url": c.URL, "user": c.User, "router_id": c.RouterID,
			"holds": strings.TrimSpace(c.Token) != "", "folders_using": st.FoldersUsing(c.URL, c.User)})
	}
	out := map[string]any{"state_dir": *stateDir, "port": st.Port, "folders": folders, "clouds": clouds,
		"state_dir_present": stateDirPresent, "state_file_present": stateFilePresent}

	// VR9-I1 rule 4 — CARRY THE DAEMON'S HEARTBEAT VERDICT ACROSS THE PROCESS BOUNDARY.
	//
	// `router serve` is long-lived; this command is a separate, short-lived `docker run` that cannot
	// see its memory. Without the sidecar, a machine the control plane has been refusing every 60
	// seconds looks — from here — exactly like a healthy one.
	//
	// ⛔ AND AN ABSENT RECORD IS SAID OUT LOUD, NOT LEFT BLANK. Printing nothing reads as "fine", and a
	// router that has been refused since boot has no record either. Those are different facts and this
	// command must not merge them. (VR8-K2 made the same distinction for state.json itself.)
	// V27-009: several records → several sidecars; the worst one drives the fields below, the list names them all.
	if all := router.LoadAllHeartbeatHealth(*stateDir); len(all) > 1 {
		recs := make([]map[string]any, 0, len(all))
		for _, h := range all {
			m := map[string]any{"url": h.RecordURL, "user": h.RecordUser, "heartbeat_401_streak": h.Consecutive401}
			if h.LastReason != "" {
				m["reason"] = h.LastReason
			}
			recs = append(recs, m)
		}
		out["heartbeat_records"] = recs
	}
	if health, present, _ := router.LoadHeartbeatHealth(*stateDir); present {
		out["heartbeat_401_streak"] = health.Consecutive401
		if health.LastReason != "" {
			out["heartbeat_401_reason"] = health.LastReason
		}
		if !health.Since.IsZero() {
			out["heartbeat_401_since"] = health.Since.UTC().Format(time.RFC3339)
		}
		// The remedy appears only at the threshold. Printing it after one 401 would train operators to
		// re-onboard on any blip, which is the alarm-fatigue failure the threshold exists to avoid.
		if msg := health.EscalationMessage(); msg != "" {
			out["heartbeat_note"] = msg
		}
	} else {
		out["heartbeat_health"] = "no beat outcome recorded yet — this says nothing about whether the " +
			"control plane accepts this machine"
	}
	switch {
	case !stateDirPresent:
		// The mount is gone. Everything below reads empty because there is nothing to read, NOT
		// because nothing was wired — and that difference is the whole of V26-003.
		out["note"] = "the state directory does not exist — this router cannot resolve ANY folder. " +
			"On a bind mount this usually means the host directory was deleted underneath the container."
	case !stateFilePresent && st.Port == 0:
		out["note"] = "no port recorded — nothing has been wired on this machine yet"
	case !stateFilePresent:
		// A port without a state file: something was wired once and the file is gone.
		out["note"] = "a port is recorded but state.json is missing — folders wired earlier cannot be resolved"
	case st.Port == 0:
		out["note"] = "no port recorded — nothing has been wired on this machine yet"
	}
	// VR7-T1 (V24-003): THE ID, so teardown can name the registration it is removing.
	//
	// teardown.sh:934 greps this key. It was never emitted, so RT_ID was always empty and VR6-T3 could
	// never remove a control-plane registration — on any machine, in any tier. The ordering around it
	// was already correct; the read returned nothing.
	//
	// ⚠ LoadIdentity, NOT LoadOrCreateIdentity. `status` is a read. Teardown runs it against a state
	// dir it is dismantling, and minting a private key there would leave a credential behind on the
	// machine it just cleaned. Absent identity → the key is OMITTED, never emitted empty: a shell doing
	// `cut -d'"' -f4` cannot tell "" from "no router here".
	if id, ierr := router.LoadIdentity(*stateDir); ierr == nil {
		out["router_id"] = id.RouterID
	}
	emit(out)
	return exitOK
}

// cmdRouterFolders prints, one per line, the agent folders that route a given instance.
//
// It exists because TEARDOWN does not know them. Onboarding was told --product-dir and --test-dir;
// teardown is given an instance id and nothing else, and the folders an instance was wired into are
// recorded here and nowhere else on the machine.
//
// ⚠ VR8-K4 (V26-006) — WHAT THE CONTROL PLANE ACTUALLY HOLDS, because this comment used to get it
// wrong. It said "Migration 016 gives the CONTROL PLANE the same answer for VR-A6". It does not:
// NOTHING writes instance_folders. The migration backfilled it once and no runtime code has
// touched it since — a grep of the tree finds it only in a cascade-delete test.
//
// The conclusion was right and the mechanism was wrong. The control plane DOES hold folder paths,
// in instances.product_dir / test_dir (migration 015), written at
// internal/control/store/access.go:495-503 and :1836, and rendered as the TEST/PROD chips at
// ui/src/pages/Environments.jsx:70-71.
//
// ⛔ THEY ANSWER DIFFERENT QUESTIONS, and that is why BOTH exist:
//
//	"which folders does THIS INSTANCE use?"  -> the control plane, product_dir / test_dir.
//	"which instances share THIS FOLDER?"     -> the ROUTER, and only the router.
//
// The second is the direction teardown needs, which is why the purge reads router state and not
// the control plane. instance_folders exists for that question and is deliberately NOT dropped:
// the columns cannot express it (migration 016 says the upsert "keeps a single value"), and one
// product folder legitimately serves several instances at once.
//
// And teardown must work with the control plane unreachable regardless.
//
// PLAIN LINES, not the JSON every other command emits. The consumer is a `while read` loop in bash,
// and a shell parsing indented JSON with sed is how a path containing a space silently becomes two
// folders. Deliberate exception, stated so it is not "fixed" later.
func cmdRouterFolders(args []string) int {
	fs := flag.NewFlagSet("router folders", flag.ContinueOnError)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table")
	instance := fs.String("instance", "", "list only folders that route this instance")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}
	st, err := router.LoadState(*stateDir)
	if err != nil {
		return emitErr(exitErr, "router folders: %v", err)
	}
	for _, f := range st.Folders {
		if *instance != "" {
			if _, ok := f.Upstreams[*instance]; !ok {
				continue
			}
		}
		fmt.Println(f.Path)
	}
	return exitOK
}

// cmdMCPJSON exposes internal/agentcfg directly, for the cutover and for repair. onboard.sh uses
// `router wire`, which does the routing and the config together; this is the escape hatch for the
// cases that are only about the file.
const mcpjsonUsage = "usage: argus mcpjson merge|remove|list|enable|disable|enabled --file <path> [--key <k>] [--server <json>|-]"

func cmdMCPJSON(args []string) int {
	if len(args) == 0 {
		return emitErr(exitUsage, mcpjsonUsage)
	}
	// F-CLI-HELP-1: checked BEFORE --file is required — otherwise `argus mcpjson --help` was judged
	// against the same required-flag refusal every other missing-flag call gets, and the caller never
	// learned the subcommand list at all.
	if isHelpToken(args[0]) {
		fmt.Println(mcpjsonUsage)
		return exitOK
	}
	fs := flag.NewFlagSet("mcpjson", flag.ContinueOnError)
	file := fs.String("file", "", "path to a .mcp.json")
	key := fs.String("key", "", "the server key")
	server := fs.String("server", "", "the server entry as JSON (merge)")
	deleteIfEmpty := fs.Bool("delete-if-empty", false, "remove: delete the FILE if no servers remain")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return emitErr(exitUsage, "flag error: %v", err)
	}
	if *file == "" {
		return emitErr(exitUsage, "mcpjson: --file is required")
	}
	switch args[0] {
	case "merge":
		if *key == "" || *server == "" {
			return emitErr(exitUsage, "mcpjson merge: --key and --server are required (use --server - to read it from stdin)")
		}
		entry := []byte(*server)
		// `--server -` reads the entry from STDIN. A server entry carries a bearer token, and a token
		// on argv is visible in `ps` and preserved in `docker inspect` long after the command exits.
		if *server == "-" {
			blob, rerr := io.ReadAll(os.Stdin)
			if rerr != nil {
				return emitErr(exitErr, "mcpjson merge: reading the entry from stdin: %v", rerr)
			}
			entry = bytes.TrimSpace(blob)
		}
		created, err := agentcfg.Merge(*file, *key, entry)
		if err != nil {
			return emitErr(exitErr, "mcpjson merge (%s): %v", *file, err)
		}
		emit(map[string]any{"merged": true, "file": *file, "key": *key, "file_created": created})
	case "remove":
		if *key == "" {
			return emitErr(exitUsage, "mcpjson remove: --key is required")
		}
		removed, deleted, err := agentcfg.Remove(*file, *key, *deleteIfEmpty)
		if err != nil {
			return emitErr(exitErr, "mcpjson remove (%s): %v", *file, err)
		}
		emit(map[string]any{"removed": removed, "file": *file, "key": *key, "file_deleted": deleted})
	case "list":
		names, err := agentcfg.Servers(*file)
		if err != nil {
			return emitErr(exitErr, "mcpjson list (%s): %v", *file, err)
		}
		emit(map[string]any{"file": *file, "servers": names})
	// The three below act on `.claude/settings.local.json`, the file that makes the client TRUST a
	// registered server. They live under the same command because they are the same job — an agent
	// folder's JSON — and because splitting them invites a second, sloppier writer for the second file,
	// which is exactly the history this package exists to end.
	case "enable":
		if *key == "" {
			return emitErr(exitUsage, "mcpjson enable: --key is required")
		}
		created, err := agentcfg.EnableServer(*file, *key)
		if err != nil {
			return emitErr(exitErr, "mcpjson enable (%s): %v", *file, err)
		}
		emit(map[string]any{"enabled": true, "file": *file, "key": *key, "file_created": created})
	case "disable":
		if *key == "" {
			return emitErr(exitUsage, "mcpjson disable: --key is required")
		}
		removed, deleted, err := agentcfg.DisableServer(*file, *key, *deleteIfEmpty)
		if err != nil {
			return emitErr(exitErr, "mcpjson disable (%s): %v", *file, err)
		}
		emit(map[string]any{"disabled": removed, "file": *file, "key": *key, "file_deleted": deleted})
	case "enabled":
		names, err := agentcfg.EnabledServers(*file)
		if err != nil {
			return emitErr(exitErr, "mcpjson enabled (%s): %v", *file, err)
		}
		emit(map[string]any{"file": *file, "enabled": names})
	default:
		return emitErr(exitUsage, "mcpjson: unknown subcommand %q (merge|remove|list on .mcp.json; enable|disable|enabled on settings.local.json)", args[0])
	}
	return exitOK
}

// recordURL is the record key for a control plane: its MCP URL — the form `--cloud-url` and the lifted 0.3.28
// folder entries already use, so a base URL and its /mcp form name the same record.
func recordURL(cp string) string {
	cp = strings.TrimRight(strings.TrimSpace(cp), "/")
	if strings.HasSuffix(cp, "/mcp") {
		return cp
	}
	return cp + "/mcp"
}

// cmdRouterUnrecord removes this machine's record for (control plane, user) — teardown runs it only after the
// control plane answered 200 to the registration delete (V29-02 §0.2): first on the CP, then locally.
func cmdRouterUnrecord(args []string) int {
	fs := flag.NewFlagSet("router unrecord", flag.ContinueOnError)
	stateDir := fs.String("state", router.StateDir(), "where the router keeps its folder table")
	cp := fs.String("control-plane", os.Getenv("ARGUS_CP_URL"), "the control plane whose record to remove")
	user := fs.String("user", "", "the account subject the record belongs to (omit on a single-user machine)")
	if err := fs.Parse(args); err != nil {
		return emitErr(exitUsage, "flag error: %v", err)
	}
	if *cp == "" {
		return emitErr(exitUsage, "usage: argus router unrecord --control-plane <url> [--user <subject>] [--state <dir>]")
	}
	st, err := router.LoadState(*stateDir)
	if err != nil {
		return emitErr(exitErr, "router unrecord: %v", err)
	}
	removed, cleared := st.RemoveRecord(recordURL(*cp), *user)
	if removed {
		if err := router.SaveState(*stateDir, st); err != nil {
			return emitErr(exitErr, "router unrecord: %v", err)
		}
	}
	emit(map[string]any{"removed": removed, "folder_refs_cleared": cleared, "records_left": len(st.Clouds), "control_plane": recordURL(*cp), "user": *user})
	return exitOK
}

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/agentcfg"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
)

func wireSpec(t *testing.T, folder string, hat role.Role, instance string) router.FolderSpec {
	t.Helper()
	return router.FolderSpec{
		Path:       folder,
		Hat:        hat,
		InstanceID: instance,
		Executor:   router.Upstream{URL: "http://localhost:8765/sse", Token: "exec-" + instance},
	}
}

func readMCP(t *testing.T, path string) map[string]any {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, blob)
	}
	return doc
}

func servers(t *testing.T, path string) map[string]any {
	t.Helper()
	doc := readMCP(t, path)
	srv, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no mcpServers object: %v", path, doc)
	}
	return srv
}

func TestWireFolder_WritesBothTheRouteAndTheConfig(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	out, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if !out.FolderCreated || !out.FileCreated {
		t.Fatalf("first wire should create both: %+v", out)
	}

	st, err := router.LoadState(state)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(st.Folders) != 1 || st.Port == 0 {
		t.Fatalf("state not recorded: %+v", st)
	}

	entry, ok := servers(t, out.MCPJSON)[routerServerKey].(map[string]any)
	if !ok {
		t.Fatalf("no %q server in %s", routerServerKey, out.MCPJSON)
	}
	// The URL must name the port the STATE holds. A .mcp.json pointing at a port the router will not
	// bind is the single most consequential thing this command can get wrong.
	wantURL := "http://127.0.0.1:" + itoa(st.Port) + "/mcp"
	if entry["url"] != wantURL {
		t.Fatalf("mcp.json url %v does not match the recorded port (want %s)", entry["url"], wantURL)
	}
	hdrs, _ := entry["headers"].(map[string]any)
	auth, _ := hdrs["Authorization"].(string)
	if !strings.HasPrefix(auth, "Bearer "+router.RouterTokenPrefix) {
		t.Fatalf("the folder must hold a ROUTER token, got %q", auth)
	}
	if auth != "Bearer "+st.Folders[0].Token {
		t.Fatal("the token in .mcp.json is not the token in the routing table — the agent would authenticate to nobody")
	}
}

func TestWireFolder_IsIdempotent(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	first, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	second, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("re-wire: %v", err)
	}
	if second.FolderCreated || second.FileCreated {
		t.Fatalf("a re-wire created something: %+v", second)
	}
	if second.RouterToken != first.RouterToken {
		t.Fatal("re-wiring rotated the folder's router token")
	}
	st, _ := router.LoadState(state)
	if len(st.Folders) != 1 {
		t.Fatalf("re-wire duplicated the folder: %d", len(st.Folders))
	}
	if got := servers(t, first.MCPJSON); len(got) != 1 {
		t.Fatalf("re-wire duplicated the server entry: %v", got)
	}
}

// VR-A1, end to end through the command onboarding actually calls. The pre-M3-FX heredoc destroyed
// this file; the whole point of the package underneath is that it no longer can.
func TestWireFolder_LeavesAForeignMCPServerAlone(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	path := filepath.Join(folder, mcpJSONName)
	original := `{
  "mcpServers": {
    "someone-elses": { "command": "node", "args": ["server.js"] }
  },
  "unrelatedTopLevel": 42
}
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if out.FileCreated {
		t.Fatal("the file already existed — reporting it as created would let teardown DELETE a user's file")
	}
	srv := servers(t, path)
	if _, still := srv["someone-elses"]; !still {
		t.Fatalf("the foreign server was destroyed: %v", srv)
	}
	if _, ours := srv[routerServerKey]; !ours {
		t.Fatalf("our entry was not added: %v", srv)
	}
	doc := readMCP(t, path)
	if doc["unrelatedTopLevel"] != float64(42) {
		t.Fatalf("an unrelated top-level key was lost: %v", doc)
	}
}

func TestWireFolder_TwoInstancesShareOneEntry(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	if _, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey); err != nil {
		t.Fatalf("wire a: %v", err)
	}
	out, err := wireFolder(state, wireSpec(t, folder, role.Test, "sutb"), routerServerKey)
	if err != nil {
		t.Fatalf("wire b: %v", err)
	}
	if out.Instances != 2 {
		t.Fatalf("want 2 instances in the folder, got %d", out.Instances)
	}
	// One entry, both instances. This is VR-A2 satisfied structurally rather than by careful keying:
	// the entry does not vary with the instance, so a second onboard cannot delete the first.
	if got := servers(t, out.MCPJSON); len(got) != 1 {
		t.Fatalf("want exactly one router entry, got %v", got)
	}
}

// The refusal must leave BOTH sides untouched. A refusal that had already written the state would be
// worse than no check: the operator is told it failed, and the machine says otherwise.
func TestWireFolder_ProductFolderWithCloudRefWritesNothing(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	// The machine holds the record (router register, step 8a), so the refusal below is about the HAT,
	// not about a missing record.
	seed := router.State{Port: 9765}
	seed.PutRecord(router.CloudRecord{URL: "https://cp/mcp", User: "user_a", Token: "odts_secret"})
	if err := router.SaveState(state, seed); err != nil {
		t.Fatal(err)
	}
	spec := wireSpec(t, folder, role.Product, "suta")
	spec.Cloud = &router.CloudRef{URL: "https://cp/mcp", User: "user_a"}
	if _, err := wireFolder(state, spec, routerServerKey); err == nil {
		t.Fatal("VR-R4: wiring an author-plane reference into a product folder must be refused")
	}
	if _, err := os.Stat(filepath.Join(folder, mcpJSONName)); !os.IsNotExist(err) {
		t.Fatal("the refused wire wrote a .mcp.json")
	}
	st, _ := router.LoadState(state)
	if len(st.Folders) != 0 {
		t.Fatalf("the refused wire recorded a route: %+v", st.Folders)
	}
	// And the record is untouched: a refusal must not cost the machine its token.
	if len(st.Clouds) != 1 || st.Clouds[0].Token != "odts_secret" {
		t.Fatalf("the refused wire changed the record: %+v", st.Clouds)
	}
}

// State-then-file, asserted rather than assumed. When the config write fails, the ROUTE must already
// be recorded — that is the recoverable direction, and re-running completes it.
func TestWireFolder_ConfigWriteFailureStillLeavesTheRouteRecorded(t *testing.T) {
	state := t.TempDir()
	// A folder path that is a FILE: agentcfg cannot create .mcp.json underneath it.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := wireFolder(state, wireSpec(t, blocker, role.Test, "suta"), routerServerKey)
	if err == nil {
		t.Skip("this platform allowed a .mcp.json under a regular file; the ordering assertion needs a different lever here")
	}
	if !strings.Contains(err.Error(), "not stranded") {
		t.Fatalf("the error must tell the operator the folder still works, got %q", err)
	}
	st, lerr := router.LoadState(state)
	if lerr != nil {
		t.Fatalf("load state: %v", lerr)
	}
	if len(st.Folders) != 1 {
		t.Fatalf("the route was NOT recorded before the config write — a failure here is unrecoverable by re-running: %+v", st)
	}
}

func TestUnwireFolder_KeepsTheEntryWhileAnotherInstanceRemains(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	out, _ := wireFolder(state, wireSpec(t, folder, role.Test, "sutb"), routerServerKey)

	res, err := unwireFolder(state, folder, "suta", routerServerKey)
	if err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if !res.Removed || res.FolderDropped || res.EntryRemoved {
		t.Fatalf("removing one of two instances must not touch the config: %+v", res)
	}
	if _, ours := servers(t, out.MCPJSON)[routerServerKey]; !ours {
		t.Fatal("the folder's route to its remaining instance was removed")
	}
}

func TestUnwireFolder_LastInstanceDeletesTheFileOnboardingCreated(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	out, _ := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)

	res, err := unwireFolder(state, folder, "suta", routerServerKey)
	if err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if !res.FolderDropped || !res.EntryRemoved || !res.FileDeleted {
		t.Fatalf("the last instance must take the folder AND the file onboarding created: %+v", res)
	}
	if _, err := os.Stat(out.MCPJSON); !os.IsNotExist(err) {
		t.Fatal("an empty .mcp.json was left behind — exactly the trace VR-A5 exists to prevent")
	}
}

// The mirror case, and the one that protects the user: a file we did NOT create is emptied of our
// entry and KEPT, whatever else it holds.
func TestUnwireFolder_AUserOwnedFileIsKept(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	path := filepath.Join(folder, mcpJSONName)
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey); err != nil {
		t.Fatalf("wire: %v", err)
	}
	res, err := unwireFolder(state, folder, "suta", routerServerKey)
	if err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if res.FileDeleted {
		t.Fatal("teardown deleted a file the user owned")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the user's file is gone: %v", err)
	}
	if got := servers(t, path); len(got) != 0 {
		t.Fatalf("our entry survived teardown: %v", got)
	}
}

func TestUnwireFolder_UnknownIsNotAnError(t *testing.T) {
	state := t.TempDir()
	res, err := unwireFolder(state, t.TempDir(), "never-wired", routerServerKey)
	if err != nil || res.Removed {
		t.Fatalf("tearing down what was never wired must be a no-op: err=%v %+v", err, res)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// `.mcp.json` registers the server; settings.local.json is what makes the client TRUST it. One
// command writes both, because a folder holding a server it is not permitted to use fails in a way
// that reads as a broken router.
func TestWireFolder_WritesTheTrustFileToo(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	out, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if !out.SettingsCreated {
		t.Fatalf("settings file not created: %+v", out)
	}
	names, err := agentcfg.EnabledServers(out.Settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if len(names) != 1 || names[0] != routerServerKey {
		t.Fatalf("want [%s] enabled, got %v", routerServerKey, names)
	}
}

// The regression that motivated it: onboarding used to overwrite this file wholesale, deleting the
// user's own permissions and hooks on every onboard.
func TestWireFolder_KeepsTheUsersOwnClaudeSettings(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	sp := filepath.Join(folder, ".claude", "settings.local.json")
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		t.Fatal(err)
	}
	original := `{"permissions":{"allow":["Bash(git status)"]},"enabledMcpjsonServers":["someone-elses"]}`
	if err := os.WriteFile(sp, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if out.SettingsCreated {
		t.Fatal("an existing settings file must not be reported as created — teardown could then DELETE it")
	}
	blob, err := os.ReadFile(sp)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, blob)
	}
	if doc["permissions"] == nil {
		t.Fatalf("the user's permissions were destroyed:\n%s", blob)
	}
	names, _ := agentcfg.EnabledServers(sp)
	if len(names) != 2 {
		t.Fatalf("want both servers enabled, got %v", names)
	}
}

func TestUnwireFolder_TakesTheTrustEntryWithTheRoute(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	out, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	res, err := unwireFolder(state, folder, "suta", routerServerKey)
	if err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if !res.SettingsUpdated || !res.SettingsDeleted {
		t.Fatalf("the settings file onboarding created must go with the route: %+v", res)
	}
	if _, err := os.Stat(out.Settings); !os.IsNotExist(err) {
		t.Fatal("an empty settings.local.json was left behind")
	}
}

// ── VR-L4 (V17-014): teardown removes what ONBOARDING wrote, and nothing the user owns ──────────
//
// THE DEFECT, observed on the owner's estate 2026-08-13: after five teardowns, every torn-down
// test-agent folder was still BOUND to a DEREGISTERED instance — its `.mcp.json` still carried the live
// router entry, and beside it `.argus/instance.json` still named the dead instance, which the
// scenario-runner skill of the day told the agent to read. So teardown left an instruction to address
// something that no longer existed.
//
// ⚠ RE-POINTED 2026-09-05 (VR10-O4 / V28-017). Onboarding no longer writes `.argus/instance.json`
// and no skill reads it: the OPERATOR supplies instance_id, always, because one folder can be wired to
// several instances and a file names only the one that onboarded LAST. The file is now LITTER a
// pre-0.3.29 onboard left behind, and the owner's decision (2026-09-04) is to keep the existing purge —
// the litter goes when the folder's last instance leaves; no new deletion logic. So these tests still
// SEED the litter (it is the one thing the purge still removes, which makes it honest PATH-TAKEN
// evidence), but the BINDING they guard is the real one — assertFolderUnbound: the router's route for
// the folder and the router entry in its `.mcp.json`. A folder left with either after its last
// instance left is the stale-binding defect, whatever any file beside it says.
//
// Owner decision D7 (the THIRD and final position, superseding round 3's D2): the INSTALLED
// skills go, by name, and only from the folders onboarding selected. The counter-argument (a user may have edited them)
// was raised and accepted — onboarding reinstalls them on the next onboard, so an edit is lost either
// way once you re-onboard.
//
// `scenarios/` is the user's own work and is NEVER touched. That is the half of this rule that must
// not be got wrong: 46 files were correctly preserved in the observed case, and a fix that took them
// with it would be far worse than the bug.
// assertFolderUnbound is the stale-binding check VR10-O4 re-pointed the VR-L4 tests onto. Before
// 0.3.29 they watched `.argus/instance.json`, which onboarding wrote and a skill read; that file is
// out of the design, so the marker is the binding itself — what an agent's next call would actually
// follow: the router's route for the folder, and the router entry in the folder's own .mcp.json.
func assertFolderUnbound(t *testing.T, stateDir, folder, instance, mcpJSON string) {
	t.Helper()
	st, err := router.LoadState(stateDir)
	if err != nil {
		t.Fatalf("load router state: %v", err)
	}
	norm, err := router.NormalizeFolderPath(folder)
	if err != nil {
		t.Fatalf("normalise %s: %v", folder, err)
	}
	if f, _ := router.FindFolder(st, norm); f != nil {
		if _, still := f.Upstreams[instance]; still {
			t.Errorf("⛔ the router still routes %s to the deregistered instance %q — the folder is bound "+
				"to something that does not exist", folder, instance)
		} else if len(f.Upstreams) == 0 {
			t.Errorf("⛔ the router keeps an empty record for %s: its token still authenticates a folder "+
				"that reaches nothing", folder)
		}
	}
	if _, err := os.Stat(mcpJSON); err == nil {
		if _, ours := servers(t, mcpJSON)[routerServerKey]; ours {
			t.Errorf("⛔ %s still carries the router entry after the folder's last instance left — the "+
				"agent's next call would follow a route that is gone", mcpJSON)
		}
	}
}

func TestUnwireFolder_LastInstanceRemovesWhatOnboardingWrote(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	out0, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}

	// Litter a pre-0.3.29 onboard left beside the MCP config (nothing writes or reads it now — VR10-O4)…
	mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)
	mustWrite(t, filepath.Join(folder, ".claude", "skills", "scenario-runner", "SKILL.md"), "# runner")
	mustWrite(t, filepath.Join(folder, ".claude", "skills", "failure-triage", "SKILL.md"), "# triage")
	// …and what the USER owns.
	mustWrite(t, filepath.Join(folder, "scenarios", "ORDE-001.md"), "# a scenario the user wrote")

	if _, err := unwireFolder(state, folder, "suta", routerServerKey); err != nil {
		t.Fatalf("unwire: %v", err)
	}

	// ⛔ THE BINDING, re-pointed (VR10-O4): the folder must no longer be bound to the deregistered
	// instance anywhere an agent's call could follow — not in the router's table, not in its .mcp.json.
	assertFolderUnbound(t, state, folder, "suta", out0.MCPJSON)
	// The litter goes with it — the purge the owner chose to keep (2026-09-04), and the PATH-TAKEN
	// evidence that it ran.
	if _, err := os.Stat(filepath.Join(folder, ".argus", "instance.json")); !os.IsNotExist(err) {
		t.Error("the pre-0.3.29 instance file survived the teardown — it names a deregistered instance, " +
			"and the purge that removes it was kept on purpose")
	}
	// ⛔ THE SKILLS STAY. Owner ruling 2026-08-21: teardown removes no previously installed skills
	// at all, and the whole topic is deferred for analysis -- one agent folder can serve several
	// instances, and "everyone has left" is not the same question as "do you still want these".
	if _, err := os.Stat(filepath.Join(folder, ".claude", "skills", "scenario-runner")); err != nil {
		t.Errorf("teardown removed an installed skill: %v", err)
	}
	// The one that must NOT be got wrong.
	if _, err := os.Stat(filepath.Join(folder, "scenarios", "ORDE-001.md")); err != nil {
		t.Fatalf("the user's scenarios were touched — this is the half of the rule that matters most: %v", err)
	}
}

// While ANOTHER instance still uses the folder, nothing is removed: the folder is still live — its
// route to the remaining instance and its .mcp.json entry stay — and the litter beside them is not
// touched either, because the purge runs only when the LAST instance leaves.
func TestUnwireFolder_KeepsOnboardingFilesWhileAnotherInstanceRemains(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	_, _ = wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	out, _ := wireFolder(state, wireSpec(t, folder, role.Test, "sutb"), routerServerKey)
	mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"sutb"}`)

	if _, err := unwireFolder(state, folder, "suta", routerServerKey); err != nil {
		t.Fatalf("unwire: %v", err)
	}
	// The binding that must SURVIVE (VR10-O4 re-point): the folder still reaches sutb, in the router's
	// table and in its own .mcp.json.
	st, err := router.LoadState(state)
	if err != nil {
		t.Fatalf("load router state: %v", err)
	}
	norm, _ := router.NormalizeFolderPath(folder)
	f, _ := router.FindFolder(st, norm)
	if f == nil {
		t.Fatalf("the folder's record went while sutb still uses it")
	}
	if _, ok := f.Upstreams["sutb"]; !ok {
		t.Errorf("the route to the remaining instance was removed: %v", f.Upstreams)
	}
	if _, ours := servers(t, out.MCPJSON)[routerServerKey]; !ours {
		t.Error("the folder's .mcp.json lost its router entry while another instance still uses it")
	}
	if _, err := os.Stat(filepath.Join(folder, ".argus", "instance.json")); err != nil {
		t.Errorf("the purge ran while another instance still uses this folder: %v", err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// ── VR8-S1 (V26-010): the purge is scoped by NAME and by FOLDER ──────────────────────────────────
//
// The defect these encode: unwireFolderAt removed `.claude/skills` WHOLESALE, so a skill the user
// wrote was destroyed by a teardown whose own comment promised to remove "nothing the user owns".
// Proven live — the owner's product repo tracked four skills and only Argus's two survived.
//
// Every assertion below is on the FILESYSTEM, never on the outcome struct's summary fields. Eight of
// round 8's ten rows are summaries that disagreed with the world, so a test that reads the summary
// passes identically before and after a bad fix.

// installUserSkill writes a skill Argus never installs. It must survive every teardown.
func installUserSkill(t *testing.T, folder string) string {
	t.Helper()
	p := filepath.Join(folder, ".claude", "skills", "my-skill", "SKILL.md")
	mustWrite(t, p, "# the user's own skill")
	return p
}

// installArgusSkills writes exactly what onboarding installs for a hat, plus scenario-author for
// the product hat — which onboarding NEVER installs there, so it stands in for a user file that
// happens to share a name with a test-hat skill.
func installArgusSkills(t *testing.T, folder string, names ...string) {
	t.Helper()
	for _, n := range names {
		mustWrite(t, filepath.Join(folder, ".claude", "skills", n, "SKILL.md"), "# "+n)
	}
}

func skillExists(t *testing.T, folder, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(folder, ".claude", "skills", name))
	return err == nil
}

// 🚨 TEARDOWN REMOVES NO SKILLS. Owner ruling 2026-08-21, superseding D7 in all three of its
// positions and D2 from round 3 before them:
//
//	"lets move it out from the current build scope with remark that this whole topic must be
//	 additionally analyzed because one test or prod agent folder can be used by multiple
//	 instances" ... "teardown do not remove any previously installed skills at all"
//
// ⚠ THIS TEST IS THE INVERSE OF THE ONE IT REPLACES, which asserted that Argus's own skills were
// removed by name. Nothing under .claude/skills is touched now -- not the user's, and not ours.
//
// The pre-0.3.29 instance file IS still removed by the purge the owner kept (2026-09-04), and this
// asserts that too. Without it the test would pass on a teardown that did nothing whatsoever, which is
// not what is being promised.
func TestUnwireFolder_RemovesNoSkillsAtAll(t *testing.T) {
	for _, hat := range []role.Role{role.Product, role.Test} {
		t.Run(string(hat), func(t *testing.T) {
			state, folder := t.TempDir(), t.TempDir()
			if _, err := wireFolder(state, wireSpec(t, folder, hat, "suta"), routerServerKey); err != nil {
				t.Fatalf("wire: %v", err)
			}
			installArgusSkills(t, folder, "scenario-runner", "failure-triage", "scenario-author")
			installUserSkill(t, folder)
			mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)

			if _, err := unwireFolder(state, folder, "suta", routerServerKey); err != nil {
				t.Fatalf("unwire: %v", err)
			}

			// ⛔ PATH TAKEN: the purge really ran. Otherwise every assertion below is trivially true.
			if _, err := os.Stat(filepath.Join(folder, ".argus", "instance.json")); !os.IsNotExist(err) {
				t.Fatalf("the pre-0.3.29 instance file survived, so the purge never ran and this proves nothing")
			}
			for _, name := range []string{"scenario-runner", "failure-triage", "scenario-author", "my-skill"} {
				if !skillExists(t, folder, name) {
					t.Errorf("%s: teardown removed %q. It removes NO skills now — neither the user's nor "+
						"the ones onboarding installed.", hat, name)
				}
			}
		})
	}
}

// The shared folder is the reason the topic was deferred, in the owner's own words: one product or
// test agent folder can serve MULTIPLE instances. The last-instance guard answers "has everyone
// left?" — it does not answer "do you still want these skills?", and those are different questions.
//
// The owner's Social product folder served three instances at once, so this is the normal case.
func TestUnwireFolder_ASharedFolderKeepsItsSkillsAfterTheLastInstanceLeaves(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	for _, id := range []string{"suta", "sutb"} {
		if _, err := wireFolder(state, wireSpec(t, folder, role.Product, id), routerServerKey); err != nil {
			t.Fatalf("wire %s: %v", id, err)
		}
	}
	installArgusSkills(t, folder, "scenario-runner", "failure-triage")
	installUserSkill(t, folder)
	mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)

	if _, err := unwireFolder(state, folder, "suta", routerServerKey); err != nil {
		t.Fatalf("unwire first: %v", err)
	}
	for _, name := range []string{"scenario-runner", "failure-triage", "my-skill"} {
		if !skillExists(t, folder, name) {
			t.Fatalf("%q went while another instance still uses the folder — that instance is now broken", name)
		}
	}

	if _, err := unwireFolder(state, folder, "sutb", routerServerKey); err != nil {
		t.Fatalf("unwire last: %v", err)
	}
	// ⛔ PATH TAKEN: the LAST instance leaving is what reaches the purge at all.
	if _, err := os.Stat(filepath.Join(folder, ".argus", "instance.json")); !os.IsNotExist(err) {
		t.Fatalf("the last instance left and the pre-0.3.29 instance file stayed, so the purge never ran")
	}
	for _, name := range []string{"scenario-runner", "failure-triage", "my-skill"} {
		if !skillExists(t, folder, name) {
			t.Errorf("%q was removed when the last instance left. The folder may still be in daily "+
				"use for work that has nothing to do with Argus.", name)
		}
	}
}

// withFolderMount points containerFolderMount at a real directory for one test.
//
// In production it is /folder, where the kit bind-mounts the agent folder -- see the docker run in
// the docker run in onboarding/teardown.sh that binds `:/folder`. A unit test has no container, so it names the directory that WOULD
// be mounted. Setting it to the folder under test is the honest translation of `-v $RF:/folder`;
// leaving it at /folder is the honest translation of a call made from somewhere else entirely.
func withFolderMount(t *testing.T, dir string) {
	t.Helper()
	prev := containerFolderMount
	containerFolderMount = dir
	t.Cleanup(func() { containerFolderMount = prev })
}

// S-S3 — 🚨 the FOLDER guard. `--mcp-json` exists for the container bind-mount and drives the FILE
// EDIT legitimately; it must not be able to move the PURGE. Today it can: mcpJSONPathFor returns the
// override verbatim and the purge base is filepath.Dir of it.
func TestUnwireFolderAt_RefusesToPurgeAnUnrelatedDirectory(t *testing.T) {
	state, folder, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey); err != nil {
		t.Fatalf("wire: %v", err)
	}
	// A directory that has nothing to do with this instance, holding a same-named skill.
	installArgusSkills(t, elsewhere, "scenario-runner")
	victim := filepath.Join(elsewhere, "notes.txt")
	mustWrite(t, victim, "the operator's own file")
	// ⚠ AND A BINDING, because the skills are no longer evidence of anything: teardown does not
	// remove them from ANY directory now, so `the skill survived` is true whether the guard held or
	// not. instance.json is what a mis-aimed purge would actually destroy.
	strayBinding := filepath.Join(elsewhere, ".argus", "instance.json")
	mustWrite(t, strayBinding, `{"instance_id":"somebody-else"}`)

	// The override names a file that is NOT a folder's .mcp.json.
	out, err := unwireFolderAt(state, folder, "suta", routerServerKey, victim, false)
	if err != nil {
		t.Fatalf("unwire must still complete the routing removal: %v", err)
	}
	if _, serr := os.Stat(strayBinding); serr != nil {
		t.Fatalf("🚨 the purge ran against a directory the router never recorded for this instance "+
			"and destroyed a binding in it — this is the disaster the folder guard exists to prevent: %v", serr)
	}
	if _, serr := os.Stat(victim); serr != nil {
		t.Fatalf("an unrelated file was destroyed: %v", serr)
	}
	// And it must SAY it refused. A silent skip is this round's own defect.
	if out.PurgeRefused == "" {
		t.Error("the purge was refused and the run did not say so — a silent skip is exactly the " +
			"under-reporting this round exists to fix")
	}
}

// ── VR8-S2 (V26-009): a directory onboarding created and then emptied goes too ────────────────────
//
// Measured on memstore-test-agent-compose, 2026-08-20: after teardown the folder held `.claude` (0
// entries) and `.argus` (0 entries), and the operator's freshness check — which tests EXISTENCE —
// reported `DIRTY: .claude .argus` on a folder holding zero residue.
//
// SA §0.7: the predicate is EMPTINESS ALONE, enforced by os.Remove, which FAILS on a non-empty
// directory. That is stronger than a provenance flag we would have to maintain, and it helps every
// folder already onboarded rather than only ones wired after the fix.

func dirExists(t *testing.T, parts ...string) bool {
	t.Helper()
	fi, err := os.Stat(filepath.Join(parts...))
	return err == nil && fi.IsDir()
}

func TestUnwireFolder_RemovesTheDirectoriesItEmptied(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	if _, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey); err != nil {
		t.Fatalf("wire: %v", err)
	}
	mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)
	installArgusSkills(t, folder, "scenario-runner", "failure-triage", "scenario-author")

	if _, err := unwireFolder(state, folder, "suta", routerServerKey); err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if dirExists(t, folder, ".argus") {
		t.Error(".argus survived as an empty directory — the freshness check reads that as DIRTY " +
			"on a folder holding zero residue, and an operator who learns to wave that away will also " +
			"wave away a real stale .mcp.json")
	}
	// ⚠ AND .claude STAYS, which is the OPPOSITE of what this test used to assert.
	//
	// It held that .claude/skills and .claude were emptied by the purge and therefore removed. They
	// are not emptied any more -- teardown removes no skills -- so the directory is not empty and
	// os.Remove correctly refuses it. The predicate did not change; what changed is what is left in
	// there.
	if !dirExists(t, folder, ".claude", "skills") {
		t.Error(".claude/skills was removed — it still holds the installed skills, which teardown no " +
			"longer touches")
	}
}

// The mechanism above still has to WORK when a directory genuinely is empty, or removing the skill
// purge would have quietly retired V26-009 along with it. A folder wired without skills is the case
// that still exercises it.
func TestUnwireFolder_RemovesAnEmptyClaudeDirectory(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	if _, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey); err != nil {
		t.Fatalf("wire: %v", err)
	}
	mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)
	// The directories exist and hold nothing -- no skill was ever copied in.
	if err := os.MkdirAll(filepath.Join(folder, ".claude", "skills"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := unwireFolder(state, folder, "suta", routerServerKey); err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if dirExists(t, folder, ".claude", "skills") {
		t.Error("an EMPTY .claude/skills survived — the freshness check reads that as DIRTY on a " +
			"folder holding zero residue (V26-009)")
	}
	if dirExists(t, folder, ".claude") {
		t.Error("an EMPTY .claude survived")
	}
}

// The half that must not be got wrong: EMPTY is the predicate, never EXISTS.
func TestUnwireFolder_KeepsADirectoryThatStillHoldsSomethingOfTheUsers(t *testing.T) {
	for _, tc := range []struct {
		name string
		keep []string // path parts, relative to the folder
	}{
		{"a user-authored skill", []string{".claude", "skills", "my-skill", "SKILL.md"}},
		{"a settings file the user owns", []string{".claude", "notes.md"}},
		{"something of theirs under .argus", []string{".argus", "my-notes.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, folder := t.TempDir(), t.TempDir()
			if _, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey); err != nil {
				t.Fatalf("wire: %v", err)
			}
			mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)
			installArgusSkills(t, folder, "scenario-runner")
			mustWrite(t, filepath.Join(append([]string{folder}, tc.keep...)...), "the user's own")

			if _, err := unwireFolder(state, folder, "suta", routerServerKey); err != nil {
				t.Fatalf("unwire: %v", err)
			}
			// ⛔ PATH TAKEN. Without this the case passes on a teardown that did NOTHING AT ALL — an
			// adversary neutered the entire purge and, of the five tests that touch skills, this was the
			// only one still green. Every assertion below is "this file still exists", which a run that
			// never acted satisfies perfectly.
			if _, serr := os.Stat(filepath.Join(folder, ".argus", "instance.json")); !os.IsNotExist(serr) {
				t.Fatalf("the pre-0.3.29 instance file survived, so the purge never ran and this proves nothing")
			}
			p := filepath.Join(append([]string{folder}, tc.keep...)...)
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("teardown destroyed %s: %v", filepath.Join(tc.keep...), err)
			}
		})
	}
}

// ── VR8-S1 / gate F1: the purge base must be PROVEN, not claimed ─────────────────────────────────
//
// The first version of this guard accepted an override when the basename was `.mcp.json` and "our
// entry was found in it". Both halves are caller-supplied — the key searched for comes from
// `--server-key` — and EVERY folder this router ever wired contains a `argus` entry. So unwiring
// instance A with `--mcp-json <folder-B>/.mcp.json` purged folder B: its skills, its
// `.argus/instance.json`, and its `.mcp.json` outright, leaving instance B unroutable. It
// reported PurgeRefused as empty. Deleting BOTH guards left the whole suite green.
//
// The proof is now the ROUTER TOKEN the folder was wired with — a per-folder secret that appears
// only in that folder's own .mcp.json.

func TestUnwireFolderAt_RefusesAnotherLiveFoldersMcpJson(t *testing.T) {
	state, folderA, folderB := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := wireFolder(state, wireSpec(t, folderA, role.Test, "inst-a"), routerServerKey); err != nil {
		t.Fatalf("wire A: %v", err)
	}
	outB, err := wireFolder(state, wireSpec(t, folderB, role.Test, "inst-b"), routerServerKey)
	if err != nil {
		t.Fatalf("wire B: %v", err)
	}
	// Folder B is a DIFFERENT live instance's folder, with its own real .mcp.json.
	installArgusSkills(t, folderB, "scenario-runner", "failure-triage", "scenario-author")
	installUserSkill(t, folderB)
	// ⚠ FOLDER B IS THE ONE MOUNTED, deliberately. That makes the mount check PASS, so what refuses
	// here is the token -- the second barrier, exercised on its own. A caller who mounts the wrong
	// folder gets past the first check and must still be stopped.
	withFolderMount(t, folderB)
	mustWrite(t, filepath.Join(folderB, ".argus", "instance.json"), `{"instance_id":"inst-b"}`)

	out, err := unwireFolderAt(state, folderA, "inst-a", routerServerKey, outB.MCPJSON, false)
	if err != nil {
		t.Fatalf("unwire must still complete the routing removal: %v", err)
	}

	if out.PurgeRefused == "" {
		t.Error("⛔ the purge was NOT refused for another instance's folder, and said nothing about it")
	}
	for _, name := range []string{"scenario-runner", "failure-triage", "scenario-author", "my-skill"} {
		if !skillExists(t, folderB, name) {
			t.Errorf("⛔ %q was deleted from folder B — a DIFFERENT live instance's agent folder", name)
		}
	}
	if _, serr := os.Stat(filepath.Join(folderB, ".argus", "instance.json")); serr != nil {
		t.Error("⛔ folder B's instance binding was deleted")
	}
	if _, serr := os.Stat(outB.MCPJSON); serr != nil {
		t.Error("⛔ folder B's .mcp.json was deleted — instance B is now unroutable")
	}
}

// The legitimate case the override exists for: teardown runs INSIDE A CONTAINER, where the folder is
// bind-mounted at a path that is not res.Path. The file is the folder's OWN .mcp.json, so it carries
// the folder's token and the purge proceeds. Without this, the fix would be "refuse everything".
func TestUnwireFolderAt_AcceptsTheFoldersOwnMcpJsonUnderAnOverride(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	out0, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	installArgusSkills(t, folder, "scenario-runner", "failure-triage", "scenario-author")
	installUserSkill(t, folder)
	// The instance file a pre-0.3.29 onboard wrote (litter now — VR10-O4; the purge still removes it).
	// Without it the purge assertion below would be checking that an absent file is absent -- which is
	// true of every run, including one where nothing happened.
	mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)
	// The folder IS the mount here, which is what `-v $RF:/folder` means.
	withFolderMount(t, folder)

	// Same file, named explicitly — what --mcp-json is legitimately for.
	out, err := unwireFolderAt(state, folder, "suta", routerServerKey, out0.MCPJSON, false)
	if err != nil {
		t.Fatalf("unwire: %v", err)
	}
	if out.PurgeRefused != "" {
		t.Fatalf("the folder's OWN .mcp.json was refused, so the container bind-mount path is broken: %s",
			out.PurgeRefused)
	}
	// ⚠ RE-ANCHORED. This used to assert that an installed skill was GONE, which was the evidence
	// that a legitimate purge had run. Teardown no longer removes skills, so that assertion would
	// now pass whether the purge ran or not. The pre-0.3.29 instance file is what is still removed, so
	// it is what proves the purge happened.
	if _, err := os.Stat(filepath.Join(folder, ".argus", "instance.json")); !os.IsNotExist(err) {
		t.Error("the pre-0.3.29 instance file survived a legitimate purge, so the purge did not run")
	}
	for _, name := range []string{"scenario-runner", "my-skill"} {
		if !skillExists(t, folder, name) {
			t.Errorf("teardown removed %q — it removes no skills at all now", name)
		}
	}
}

// 🚨 THE COPY HOLE. Reproduced against the token-only guard, kept here as its regression.
//
// The token proof asks "does this file carry OUR token?". `cp -r` answers YES for a directory that
// is not ours, because a token is CONTENT and content copies. Measured before the mount check
// existed: unwiring with --mcp-json pointing at the operator's BACKUP purged the backup, left the
// real folder's skills in place, and reported PurgeRefused as empty -- the run deleted the wrong
// directory and said nothing at all.
//
// A refusal purges NOTHING, so BOTH directories must come out intact.
func TestUnwireFolderAt_RefusesACopyOfTheFolderThatCarriesItsToken(t *testing.T) {
	state, folder := t.TempDir(), t.TempDir()
	out0, err := wireFolder(state, wireSpec(t, folder, role.Test, "suta"), routerServerKey)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	installArgusSkills(t, folder, "scenario-runner", "failure-triage", "scenario-author")
	installUserSkill(t, folder)

	// The pre-0.3.29 instance file (litter now — VR10-O4), written BEFORE the copy so the backup carries
	// one too. The skills are no longer evidence -- nothing removes them from anywhere now -- so this is
	// what a mis-aimed purge would actually destroy, and it is what the assertions below watch.
	mustWrite(t, filepath.Join(folder, ".argus", "instance.json"), `{"instance_id":"suta"}`)

	// The operator's backup, taken before a risky change. Byte for byte, token and all.
	backup := t.TempDir()
	// copyTreeForTest is the harness's own helper — one copy of `cp -r` in this package.
	if err := copyTreeForTest(folder, backup); err != nil {
		t.Fatalf("copy the folder: %v", err)
	}
	copyMCP := filepath.Join(backup, filepath.Base(out0.MCPJSON))

	// ⚠ THE TEST WOULD BE VACUOUS IF THE COPY WERE NOT A COPY. This asserts the backup carries
	// EXACTLY the bytes the token check reads, so we know that check alone would have accepted it
	// and that the refusal below comes from somewhere else.
	orig, err := os.ReadFile(out0.MCPJSON)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	dup, err := os.ReadFile(copyMCP)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if !bytes.Equal(orig, dup) {
		t.Fatalf("the backup is not a byte-identical copy, so this test proves nothing")
	}

	// The kit would pass /folder/.mcp.json with the folder mounted there. This names somewhere else
	// entirely, which is exactly the call that was demonstrated to destroy a backup.
	out, err := unwireFolderAt(state, folder, "suta", routerServerKey, copyMCP, false)
	if err != nil {
		t.Fatalf("unwire must still complete the routing removal: %v", err)
	}
	if out.PurgeRefused == "" {
		t.Error("⛔ a copy of the folder was accepted as the folder — and the run said nothing")
	}
	// A refusal purges NOTHING, so BOTH bindings survive. The backup's is the one the reproduced
	// defect destroyed; the real folder's proves the run did not silently half-act instead.
	for label, dir := range map[string]string{"the operator's BACKUP": backup, "the real folder": folder} {
		if _, serr := os.Stat(filepath.Join(dir, ".argus", "instance.json")); serr != nil {
			t.Errorf("⛔ the binding in %s was destroyed: %v", label, serr)
		}
	}
}

// The shape guard, tested DIRECTLY -- because through unwireFolderAt it cannot be seen.
//
// An adversary deleted BOTH call sites of overrideShapeRefusal and the whole suite stayed green:
// every case that reaches the guard is ALSO caught by the token check downstream, so the token
// check was silently standing in for it. Two guards, one of them provably load-bearing and the
// other only assumed to be. This exercises the predicate itself, where nothing can mask it.
func TestOverrideShapeRefusal(t *testing.T) {
	for _, c := range []struct {
		name, override string
		wantRefusal    bool
		wantMentions   string
	}{
		{"empty means the router's own record, not a refusal", "", false, ""},
		{"the mount itself is what the kit passes", "/folder/.mcp.json", false, ""},
		{"a file that is not a .mcp.json", "/folder/notes.txt", true, "not a"},
		{"a .mcp.json somewhere else entirely", "/elsewhere/.mcp.json", true, "only ever visible"},
		{"a subdirectory of the mount is still not the mount", "/folder/sub/.mcp.json", true, "only ever visible"},
		{"a relative path", ".mcp.json", true, "only ever visible"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := overrideShapeRefusal(c.override)
			if c.wantRefusal && got == "" {
				t.Fatalf("⛔ %q was accepted — the purge would run beside a path the router cannot show "+
					"belongs to this instance", c.override)
			}
			if !c.wantRefusal && got != "" {
				t.Fatalf("⛔ %q was refused, which breaks the real call sites: %s", c.override, got)
			}
			if c.wantMentions != "" && !strings.Contains(got, c.wantMentions) {
				t.Errorf("the refusal does not say WHY (%q missing): %s", c.wantMentions, got)
			}
		})
	}
}

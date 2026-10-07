package router

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// The three R-block specs the 2026-08-09 coverage audit found ABSENT inside a delivered phase.
// Each one is written against the behaviour the spec names, not against the implementation.

// TS-R3 (the half that is testable without a live cluster): a router RESTART must reclaim the SAME
// port, so every .mcp.json naming it stays valid. The agent-facing consequence is the point — a port
// that moved on restart would strand every folder on the machine at once.
func TestTSR3_ARestartReclaimsTheSamePortSoAgentConfigStaysValid(t *testing.T) {
	dir := t.TempDir()
	st, _, _, err := UpsertFolder(State{}, spec("/w/a", role.Test, "suta"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := PortForWiring(st.Port)
	if err != nil {
		t.Fatal(err)
	}
	st.Port = port
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	// What onboarding wrote into .mcp.json.
	agentURL := "http://" + ListenAddr(port) + "/mcp"

	// RESTART: a fresh process reads the state it left behind.
	reloaded, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := PickPort(reloaded.Port, func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if again != port {
		t.Fatalf("the restart moved the port %d -> %d; every .mcp.json naming it would be stranded", port, again)
	}
	if got := "http://" + ListenAddr(again) + "/mcp"; got != agentURL {
		t.Fatalf("the agent's recorded URL no longer matches the router: %s vs %s", agentURL, got)
	}
}

// TS-R4 — a k3d tunnel re-binds on a new port. The agent must keep working WITHOUT its .mcp.json
// being touched: the folder names the router, the router names the upstream, and only the second
// needs updating.
//
// This is the spec VR-R8's deferral leaves exposed in production: nothing re-wires automatically,
// so a re-bound forward stales the router's upstream until someone runs `router wire`. What is
// asserted here is the property that makes that fix cheap — the agent's config is not involved.
func TestTSR4_ARebindChangesOnlyTheROUTERSUpstream(t *testing.T) {
	dir := t.TempDir()
	sp := spec("/w/test", role.Test, "suta")
	sp.Executor = Upstream{URL: "http://localhost:53905/mcp", Token: "tok"}
	st, folder, _, err := UpsertFolder(State{Port: PreferredPort}, sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	before := folder.Token
	agentURL := "http://" + ListenAddr(st.Port) + "/mcp"

	// The tunnel comes back on a different port; onboarding re-wires that ONE fact.
	sp2 := spec("/w/test", role.Test, "suta")
	sp2.Executor = Upstream{URL: "http://localhost:61111/mcp", Token: "tok"}
	st2, folder2, created, err := UpsertFolder(st, sp2)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("a re-bind created a SECOND folder record")
	}
	if folder2.Token != before {
		t.Fatal("a re-bind rotated the folder's router token — the agent's .mcp.json would be stale")
	}
	if st2.Port != st.Port {
		t.Fatalf("a re-bind moved the ROUTER's port %d -> %d", st.Port, st2.Port)
	}
	if got := "http://" + ListenAddr(st2.Port) + "/mcp"; got != agentURL {
		t.Fatalf("the agent's URL changed on a re-bind: %s -> %s", agentURL, got)
	}
	if got := st2.Folders[0].Upstreams["suta"].URL; got != "http://localhost:61111/mcp" {
		t.Fatalf("the upstream did not follow the re-bind: %q", got)
	}
}

// TS-R6 — two instances driven from ONE folder. The router's half of the spec: each instance
// resolves to its OWN upstream and its OWN per-hat credential, so two concurrent runs cannot be
// confused for one. (The ledger/`busy` half belongs to the executor and the store, which own run
// state; this asserts the routing property the multi-instance feature rests on.)
func TestTSR6_TwoInstancesFromOneFolderResolveIndependently(t *testing.T) {
	st, folder, _, err := UpsertFolder(State{}, spec("/w/test", role.Test, "suta"))
	if err != nil {
		t.Fatal(err)
	}
	s2 := spec("/w/test", role.Test, "sutb")
	s2.Executor = Upstream{URL: "http://localhost:8767/mcp", Token: "exec-sutb"}
	st, _, _, err = UpsertFolder(st, s2)
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := TableFrom(st)
	if err != nil {
		t.Fatal(err)
	}
	f, rerr := tbl.Resolve(folder.Token)
	if rerr != nil {
		t.Fatal(rerr)
	}
	a, err := f.Route("runner__run", "suta")
	if err != nil {
		t.Fatalf("suta: %v", err)
	}
	b, err := f.Route("runner__run", "sutb")
	if err != nil {
		t.Fatalf("sutb: %v", err)
	}
	if a.URL == b.URL {
		t.Fatalf("both instances routed to the SAME upstream (%s) — one folder could not drive two SUTs", a.URL)
	}
	if a.Token == b.Token {
		t.Fatal("both instances forwarded the SAME credential — the per-instance token binding is gone")
	}
}

// TS-A2's untested clause: teardown must remove the instance from EVERY folder that references it,
// not just one. `router folders` is what teardown enumerates with, so the enumeration is the thing
// to assert.
func TestTSA2_AnInstanceIsFoundInEveryFolderThatReferencesIt(t *testing.T) {
	dir := t.TempDir()
	st := State{}
	for _, f := range []string{"/w/one", "/w/two", "/w/three"} {
		var err error
		st, _, _, err = UpsertFolder(st, spec(f, role.Test, "shared"))
		if err != nil {
			t.Fatal(err)
		}
	}
	// a fourth folder that does NOT hold it
	var err error
	st, _, _, err = UpsertFolder(st, spec("/w/other", role.Test, "elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}

	var found []string
	for _, f := range st.Folders {
		if _, ok := f.Upstreams["shared"]; ok {
			found = append(found, f.Path)
		}
	}
	if len(found) != 3 {
		t.Fatalf("want the instance found in all THREE referencing folders, got %v", found)
	}

	// Removing it from one must not remove it from the others — teardown iterates.
	st, res, err := RemoveInstance(st, "/w/one", "shared")
	if err != nil || !res.Removed {
		t.Fatalf("remove: %v %+v", err, res)
	}
	still := 0
	for _, f := range st.Folders {
		if _, ok := f.Upstreams["shared"]; ok {
			still++
		}
	}
	if still != 2 {
		t.Fatalf("after removing ONE reference, %d remain; want 2", still)
	}
}

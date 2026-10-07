package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
)

// V16 (VR3-10), under the V27-009 redesign: a folder is wired to the author plane with the URL alone —
// no credential ever passes through `router wire`. The folder REFERS to the machine's record for that
// control plane; the token is minted afterwards (`cloud-mint-token --router-state`, in-process) and lands
// on the record, which the folder resolves at call time.
//
// THE ORDERING THIS PROTECTS, because getting it backwards silently loses a live credential:
//
//	register (router register)      -> the machine's record for (control plane, user) exists
//	wire     (--cloud-url, no user) -> the folder declares it uses that record
//	mint     (--router-state)       -> fills the record's token in-process, never through a shell variable
//
// A wire that names no record is REFUSED naming step 8a: a ref to nothing would route the author plane
// nowhere, and a mint after it would have no record to land on — a freshly minted token live at the
// control plane with no holder on the machine, unrecoverable (VR-B7: the plaintext existed only in that
// one response).

const soleCP = "https://cp.example/mcp"

// registeredState leaves what `router register` leaves: one token-less record per user for the control plane.
func registeredState(t *testing.T, dir string, users ...string) {
	t.Helper()
	st := router.State{Port: 9765}
	for _, u := range users {
		st.PutRecord(router.CloudRecord{URL: soleCP, User: u, RouterID: "rtr_x"})
	}
	if err := router.SaveState(dir, st); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func stateFile(t *testing.T, dir string) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	return string(blob)
}

// --cloud-url alone (no user) is allowed when the machine holds exactly one record for that URL: the ref
// resolves to the sole record, and the mint's half lands on that record — once — where the folder finds it.
func TestWireFolder_CloudURLAloneUsesTheSoleRecord(t *testing.T) {
	dir := t.TempDir()
	registeredState(t, dir, "user_a")
	spec := wireSpec(t, t.TempDir(), role.Test, "inst-a")
	spec.Cloud = &router.CloudRef{URL: soleCP} // no user, no token — the record supplies both

	out, err := wireFolder(dir, spec, routerServerKey)
	if err != nil {
		t.Fatalf("wire with a URL-only cloud ref against the sole record: %v", err)
	}
	if !out.CloudWired {
		t.Fatal("the outcome does not say the cloud was wired")
	}
	st, err := router.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Folders) != 1 || st.Folders[0].Cloud == nil {
		t.Fatal("the cloud ref was not recorded — the folder would never reach the author plane")
	}
	if st.Folders[0].Cloud.URL != soleCP {
		t.Fatalf("cloud URL = %q", st.Folders[0].Cloud.URL)
	}
	if len(st.Clouds) != 1 {
		t.Fatalf("the wire changed the records (%d) — it may only REFER to one", len(st.Clouds))
	}
	if strings.Contains(stateFile(t, dir), "odts_") {
		t.Fatal("a token appeared from nowhere")
	}

	// The mint's half: the token lands on the record, and the folder resolves it through its ref.
	if err := router.SetRecordToken(dir, soleCP, "", "odts_minted_now", time.Time{}); err != nil {
		t.Fatalf("SetRecordToken on the sole record: %v", err)
	}
	got, err := router.ReadRecordToken(dir, soleCP, "")
	if err != nil || got != "odts_minted_now" {
		t.Fatalf("read back %q err %v", got, err)
	}
	after, err := router.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	tgt, err := after.Folders[0].Credential(&after)
	if err != nil || tgt.Token != "odts_minted_now" || tgt.URL != soleCP {
		t.Fatalf("the folder's author target = %+v, %v; want the record's token and URL", tgt, err)
	}
	if n := strings.Count(stateFile(t, dir), "odts_minted_now"); n != 1 {
		t.Fatalf("the token appears %d times in state.json, want exactly 1 — a folder never carries a copy", n)
	}
}

// No record: refused, naming the step that creates one, and NOTHING written on either side — a refusal
// that had already recorded the route would tell the operator it failed while the machine says otherwise.
func TestWireFolder_CloudURLWithoutARecordIsRefused(t *testing.T) {
	dir, folder := t.TempDir(), t.TempDir()
	spec := wireSpec(t, folder, role.Test, "inst-a")
	spec.Cloud = &router.CloudRef{URL: soleCP}

	_, err := wireFolder(dir, spec, routerServerKey)
	if err == nil {
		t.Fatal("a cloud ref to a record this machine does not hold was accepted — the folder would route " +
			"the author plane nowhere and say so only at the agent's first tool call")
	}
	if !strings.Contains(err.Error(), "router register") {
		t.Fatalf("the refusal does not name `router register` (onboarding step 8a): %v", err)
	}
	if _, serr := os.Stat(filepath.Join(folder, mcpJSONName)); !os.IsNotExist(serr) {
		t.Fatal("the refused wire wrote a .mcp.json")
	}
	st, err := router.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Folders) != 0 {
		t.Fatalf("the refused wire recorded a route: %+v", st.Folders)
	}
	if len(st.Clouds) != 0 {
		t.Fatalf("the refused wire invented a record: %+v", st.Clouds)
	}

	// The same through the flag, which is how onboard.sh calls it.
	var rc int
	out := captureEmit(t, func() {
		rc = cmdRouterWire([]string{"--state", dir, "--folder", folder, "--hat", "test", "--instance", "inst-a",
			"--executor-url", "http://localhost:8765/sse", "--executor-token", "exec-a", "--cloud-url", soleCP})
	})
	if rc == exitOK {
		t.Fatal("`router wire --cloud-url` succeeded with no record on the machine")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "router register") {
		t.Fatalf("the CLI refusal does not name `router register`: %v", out)
	}
}

// "Exactly one" is the condition: with two records for the URL a nameless ref cannot resolve, so the wire is
// refused and writes nothing; naming the user with --cloud-user resolves it.
func TestWireFolder_CloudURLAloneIsRefusedWhenTwoRecordsShareTheURL(t *testing.T) {
	dir, folder := t.TempDir(), t.TempDir()
	registeredState(t, dir, "user_a", "user_b")
	spec := wireSpec(t, folder, role.Test, "inst-a")
	spec.Cloud = &router.CloudRef{URL: soleCP}

	if _, err := wireFolder(dir, spec, routerServerKey); err == nil {
		t.Fatal("a nameless ref was accepted although two records share the URL — the router would have to guess whose token to use")
	}
	if _, serr := os.Stat(filepath.Join(folder, mcpJSONName)); !os.IsNotExist(serr) {
		t.Fatal("the refused wire wrote a .mcp.json")
	}
	st, _ := router.LoadState(dir)
	if len(st.Folders) != 0 || len(st.Clouds) != 2 {
		t.Fatalf("the refused wire changed the state: folders=%d clouds=%d", len(st.Folders), len(st.Clouds))
	}

	spec.Cloud = &router.CloudRef{URL: soleCP, User: "user_b"}
	if _, err := wireFolder(dir, spec, routerServerKey); err != nil {
		t.Fatalf("naming the user must resolve it: %v", err)
	}
	st, _ = router.LoadState(dir)
	if len(st.Folders) != 1 || st.Folders[0].Cloud == nil || st.Folders[0].Cloud.User != "user_b" {
		t.Fatalf("the folder does not refer to user_b's record: %+v", st.Folders)
	}
}

// A PRODUCT folder still cannot take a cloud ref (VR-R4/VR-P7), record or no record: the product hat must be
// structurally incapable of reaching the author plane, and moving the token onto a record must not have
// widened that door.
func TestWireFolder_ProductHatStillCannotHoldACloudRef(t *testing.T) {
	dir, folder := t.TempDir(), t.TempDir()
	registeredState(t, dir, "user_a")
	spec := wireSpec(t, folder, role.Product, "inst-p")
	spec.Cloud = &router.CloudRef{URL: soleCP, User: "user_a"}

	if _, err := wireFolder(dir, spec, routerServerKey); err == nil {
		t.Fatal("a PRODUCT folder was given a cloud ref")
	}
	if _, serr := os.Stat(filepath.Join(folder, mcpJSONName)); !os.IsNotExist(serr) {
		t.Fatal("the refused wire wrote a .mcp.json")
	}
	st, err := router.LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	for _, f := range st.Folders {
		if f.Hat == role.Product && f.Cloud != nil {
			t.Fatal("a PRODUCT folder carries a cloud ref in the state")
		}
	}
	if len(st.Folders) != 0 {
		t.Fatalf("the refused wire recorded a route: %+v", st.Folders)
	}
}

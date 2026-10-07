package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
)

// VR5-C2 (V19-002) — a rotation that stores the replacement NOWHERE must fail loudly.
//
// The chain this guards, and why "nil" is not a free choice:
//
//	heartbeat sees a rotated token -> OnRotatedToken(tok) -> nil ? -> ACK -> the CP REVOKES the old one
//
// So returning nil is a promise that the replacement is durably held by this machine. Under the V27-009
// redesign "held" means ONE thing: it is on the machine's CloudRecord for (control plane, user). A machine
// with no such record — never registered, or `router unrecord`ed by a teardown — has nowhere to put it,
// and acknowledging would revoke the only credential the account still has.
//
// Measured consequence (V19-002): old token dead, new token stored nowhere, every later onboard dies at
// step 8b, and the only trace is `folders_updated: 0` in the router log.

const rotCP = "https://cp.example/mcp"

func rotState(t *testing.T, dir string, recs ...router.CloudRecord) {
	t.Helper()
	st := router.State{Port: 9765}
	for _, r := range recs {
		st.PutRecord(r)
	}
	if err := router.SaveState(dir, st); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func rotStateJSON(t *testing.T, dir string) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	return string(blob)
}

// The no-record case: this is the one that ships the outage.
func TestApplyRotatedToken_NoRecordIsAnError(t *testing.T) {
	dir := t.TempDir()
	// Folders are wired, but the record is gone (or was never created): what a teardown's
	// `router unrecord` leaves, and what a state that skipped step 8a looks like.
	if err := router.SaveState(dir, router.State{Port: 9765, Folders: []router.Folder{
		{Path: `C:\kit\product-agent`, Hat: role.Product, Token: "odtr_p"},
		{Path: `C:\kit\test-agent`, Hat: role.Test, Token: "odtr_t"},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var emitted []any
	err := applyRotatedToken(dir, rotCP, "user_a", "odts_replacement", func(v any) { emitted = append(emitted, v) })
	if err == nil {
		t.Fatal("rotation reported SUCCESS with no record.\n" +
			"  Returning nil here acknowledges the rotation, the control plane then REVOKES the old\n" +
			"  token, and the replacement is on no disk anywhere. The account ends up holding a token\n" +
			"  this machine cannot produce, and every later onboard dies at step 8b (V19-001/002).")
	}
	// The message has to name the state AND the way out, because the operator's next move depends on
	// knowing that nothing holds the token — not merely that "rotation failed".
	if !strings.Contains(strings.ToLower(err.Error()), "no record") {
		t.Errorf("error does not say WHAT was wrong (no record on this machine): %q", err.Error())
	}
	if !strings.Contains(err.Error(), "router register") {
		t.Errorf("error does not name `router register` (onboarding step 8a), the step that creates the record: %q", err.Error())
	}
	if len(emitted) != 0 {
		t.Errorf("a REFUSED rotation reported an update: %v", emitted)
	}
	if strings.Contains(rotStateJSON(t, dir), "odts_replacement") {
		t.Fatal("the replacement was written to state.json although there is no record to hold it")
	}
	st, err := router.LoadState(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(st.Clouds) != 0 {
		t.Fatalf("a refused rotation created a record from nothing: %+v", st.Clouds)
	}
}

// The ordinary case must keep working: a rotation with a record updates THAT record, once.
func TestApplyRotatedToken_WithARecordUpdatesIt(t *testing.T) {
	dir := t.TempDir()
	st := router.State{Port: 9765}
	st.PutRecord(router.CloudRecord{URL: rotCP, User: "user_a", RouterID: "rtr_x", Token: "odts_old"})
	var err error
	st, _, _, err = router.UpsertFolder(st, router.FolderSpec{Path: t.TempDir(), Hat: role.Test, InstanceID: "inst-a",
		Executor: router.Upstream{URL: "http://exec", Token: "rt"}, Cloud: &router.CloudRef{URL: rotCP, User: "user_a"}})
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if err := router.SaveState(dir, st); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var emitted []any
	if err := applyRotatedToken(dir, rotCP, "user_a", "odts_new", func(v any) { emitted = append(emitted, v) }); err != nil {
		t.Fatalf("rotation with a record must succeed, got: %v", err)
	}
	if len(emitted) == 0 {
		t.Error("a successful rotation emitted nothing — records_updated is the only trace this path leaves")
	}

	tok, err := router.ReadRecordToken(dir, rotCP, "user_a")
	if err != nil || tok != "odts_new" {
		t.Errorf("the rotation was acknowledged but the record reads back %q, %v", tok, err)
	}
	blob := rotStateJSON(t, dir)
	if n := strings.Count(blob, "odts_new"); n != 1 {
		t.Errorf("the replacement appears %d times in state.json, want exactly 1", n)
	}
	if strings.Contains(blob, "odts_old") {
		t.Error("the revoked token is still on disk somewhere")
	}
	// The folder routes with the replacement through its ref — there was nothing per-folder to update.
	after, err := router.LoadState(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	tgt, err := after.Folders[0].Credential(&after)
	if err != nil || tgt.Token != "odts_new" {
		t.Errorf("the folder's author target = %+v, %v; want the rotated token via its ref", tgt, err)
	}
}

// heartbeatFor calls this with the record's OWN user, which is "" for a lifted 0.3.28 record until the
// first accepted beat names it. "" must resolve the SOLE record for the URL — and refuse, not guess, when
// there are two.
func TestApplyRotatedToken_ANamelessCallResolvesTheSoleRecordOnly(t *testing.T) {
	dir := t.TempDir()
	rotState(t, dir, router.CloudRecord{URL: rotCP, Token: "odts_old"}) // lifted: no user yet
	if err := applyRotatedToken(dir, rotCP, "", "odts_new", func(any) {}); err != nil {
		t.Fatalf("a nameless call against the sole record must succeed: %v", err)
	}
	if tok, _ := router.ReadRecordToken(dir, rotCP, ""); tok != "odts_new" {
		t.Errorf("the sole record reads back %q, want the replacement", tok)
	}

	two := t.TempDir()
	rotState(t, two, router.CloudRecord{URL: rotCP, User: "user_a", Token: "odts_a"},
		router.CloudRecord{URL: rotCP, User: "user_b", Token: "odts_b"})
	if err := applyRotatedToken(two, rotCP, "", "odts_new", func(any) {}); err == nil {
		t.Fatal("with two records for one control plane a nameless rotation cannot know whose token this is, " +
			"and must refuse rather than pick one")
	}
	blob := rotStateJSON(t, two)
	if strings.Contains(blob, "odts_new") || !strings.Contains(blob, "odts_a") || !strings.Contains(blob, "odts_b") {
		t.Fatalf("a refused nameless rotation changed the file: %s", blob)
	}
}

// An empty token must never be stored, and must never be acknowledged.
func TestApplyRotatedToken_EmptyTokenIsAnError(t *testing.T) {
	dir := t.TempDir()
	rotState(t, dir, router.CloudRecord{URL: rotCP, User: "user_a", Token: "odts_old"})
	var emitted []any
	if err := applyRotatedToken(dir, rotCP, "user_a", "   ", func(v any) { emitted = append(emitted, v) }); err == nil {
		t.Error("an empty replacement was acknowledged — the old token would be revoked for nothing")
	}
	if len(emitted) != 0 {
		t.Errorf("a REFUSED rotation reported an update: %v", emitted)
	}
	// And the record must be untouched by a refused rotation.
	if tok, _ := router.ReadRecordToken(dir, rotCP, "user_a"); tok != "odts_old" {
		t.Errorf("a REFUSED rotation still modified the record: %q", tok)
	}
}

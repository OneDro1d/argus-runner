package router

// V29 ADVERSARY GATE — router-side attacks on the record model (VR10-T1, T3, T7 / SA §1.4.T1, §1.4.T7).
// Written blind from the specs; no builder test was read first.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/role"
)

const gateTok = "odts_gate0000000000000000000000000000000000000000"

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(a) — a 0.3.28 state.json with FOLDER-level cloud {URL, Token} objects.
func TestGate_LegacyLift_OneRecordPerURL_TokenOnDiskExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "port": 8765,
  "folders": [
    {"Path":"C:/t/one","Hat":"test","Token":"rt-one","Upstreams":{},"Cloud":{"URL":"https://cp.example/mcp","Token":"` + gateTok + `"},"MCPJSONCreated":true},
    {"Path":"C:/t/two","Hat":"test","Token":"rt-two","Upstreams":{},"Cloud":{"URL":"https://cp.example/mcp","Token":"` + gateTok + `"},"MCPJSONCreated":true},
    {"Path":"C:/p/prod","Hat":"product","Token":"rt-prod","Upstreams":{},"MCPJSONCreated":true}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Clouds) != 1 {
		t.Fatalf("VR10-T1-6: want ONE record per distinct URL, got %d: %+v", len(st.Clouds), st.Clouds)
	}
	if st.Clouds[0].Token != gateTok {
		t.Errorf("the lifted record must carry the token: %+v", st.Clouds[0])
	}
	if st.Clouds[0].User != "" {
		t.Errorf("VR10-T1-6: the lifted record's User must be \"\" until the first heartbeat; got %q", st.Clouds[0].User)
	}
	for i, f := range st.Folders {
		switch f.Hat {
		case role.Test:
			if f.Cloud == nil {
				t.Errorf("folder %d (%s): the legacy entry must become a CloudRef", i, f.Path)
			}
		default:
			if f.Cloud != nil {
				t.Errorf("VR-P7: product folder %d (%s) must never carry a ref", i, f.Path)
			}
		}
	}
	// THE TOKEN APPEARS EXACTLY ONCE ON DISK, whatever the folder count (PO §T1 acceptance 1 + 2).
	blob, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(blob), gateTok); n != 1 {
		t.Errorf("VR10-T1-1: the token string appears %d times in state.json after the lift; want exactly 1\n%s", n, blob)
	}
	// Idempotent: a second load changes nothing.
	st2, err := LoadState(dir)
	if err != nil {
		t.Fatalf("second LoadState: %v", err)
	}
	if len(st2.Clouds) != 1 || st2.Clouds[0].Token != gateTok {
		t.Errorf("the lift is not idempotent: %+v", st2.Clouds)
	}
}

// Two legacy folder entries for the SAME URL carrying DIFFERENT tokens (SA §0.14 T1-a: the LAST wins).
func TestGate_LegacyLift_SameURLDifferentTokens_LastWins(t *testing.T) {
	dir := t.TempDir()
	older, newer := gateTok+"-OLD", gateTok+"-NEW"
	legacy := `{"port":1,"folders":[
    {"Path":"C:/t/one","Hat":"test","Token":"rt1","Upstreams":{},"Cloud":{"URL":"https://cp/mcp","Token":"` + older + `"}},
    {"Path":"C:/t/two","Hat":"test","Token":"rt2","Upstreams":{},"Cloud":{"URL":"https://cp/mcp","Token":"` + newer + `"}}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Clouds) != 1 {
		t.Fatalf("want one record, got %+v", st.Clouds)
	}
	if st.Clouds[0].Token != newer {
		t.Errorf("SA §0.14 T1-a: the LAST-written folder entry's token must win; got %q want %q", st.Clouds[0].Token, newer)
	}
	blob, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(blob), older) {
		t.Errorf("the discarded token is still on disk after the lift:\n%s", blob)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(b) — wire / unwire / remove-instance must NEVER drop State.Clouds (V27-009's cause).
func TestGate_ConstructorsNeverDropClouds(t *testing.T) {
	base := State{
		Port: 8765,
		Clouds: []CloudRecord{
			{URL: "https://cp-a/mcp", User: "userA", RouterID: "rtr1", Token: gateTok, Expires: time.Unix(1780000000, 0).UTC()},
			{URL: "https://cp-b/mcp", User: "userB", RouterID: "rtr1", Token: gateTok + "-b"},
		},
		Folders: []Folder{{
			Path: "C:/t/one", Hat: role.Test, Token: "rt1",
			Upstreams: map[string]Upstream{"inst1": {URL: "http://localhost:1", Token: "u1"}},
			Cloud:     &CloudRef{URL: "https://cp-a/mcp", User: "userA"},
		}},
	}
	snapshot, _ := json.Marshal(base.Clouds)

	assertClouds := func(t *testing.T, what string, got State) {
		t.Helper()
		now, _ := json.Marshal(got.Clouds)
		if string(now) != string(snapshot) {
			t.Errorf("VR10-T1-4: %s changed State.Clouds — this is exactly V27-009's cause\n  before: %s\n  after : %s",
				what, snapshot, now)
		}
	}

	// wire a SECOND folder
	next, _, _, err := UpsertFolder(base, FolderSpec{Path: "C:/t/two", Hat: role.Test, InstanceID: "inst2",
		Executor: Upstream{URL: "http://localhost:2", Token: "u2"}})
	if err != nil {
		t.Fatalf("UpsertFolder: %v", err)
	}
	assertClouds(t, "UpsertFolder", next)

	// remove one instance from a folder
	next2, _, err := RemoveInstance(next, "C:/t/two", "inst2")
	if err != nil {
		t.Fatalf("RemoveInstance: %v", err)
	}
	assertClouds(t, "RemoveInstance", next2)

	// unwire the folder that CARRIES the ref — the V27-009 shape exactly
	next3, removed, err := RemoveFolder(next2, "C:/t/one")
	if err != nil {
		t.Fatalf("RemoveFolder: %v", err)
	}
	if !removed {
		t.Fatalf("RemoveFolder reported nothing removed (folders now: %+v)", next2.Folders)
	}
	assertClouds(t, "RemoveFolder", next3)

	// and the round trip through the file keeps them
	dir := t.TempDir()
	if err := SaveState(dir, next3); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	back, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	assertClouds(t, "SaveState+LoadState", back)
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(e) — RemoveRecord clears the refs of THAT record and leaves the other records alone.
func TestGate_RemoveRecord_ClearsOnlyItsOwnRefs(t *testing.T) {
	st := State{
		Clouds: []CloudRecord{
			{URL: "https://cp/mcp", User: "userA", Token: gateTok + "-a"},
			{URL: "https://cp/mcp", User: "userB", Token: gateTok + "-b"},
			{URL: "https://other/mcp", User: "userA", Token: gateTok + "-o"},
		},
		Folders: []Folder{
			{Path: "C:/t/a1", Hat: role.Test, Token: "1", Cloud: &CloudRef{URL: "https://cp/mcp", User: "userA"}},
			{Path: "C:/t/a2", Hat: role.Test, Token: "2", Cloud: &CloudRef{URL: "https://cp/mcp", User: "userA"}},
			{Path: "C:/t/b1", Hat: role.Test, Token: "3", Cloud: &CloudRef{URL: "https://cp/mcp", User: "userB"}},
			{Path: "C:/t/o1", Hat: role.Test, Token: "4", Cloud: &CloudRef{URL: "https://other/mcp", User: "userA"}},
		},
	}
	if got := st.FoldersUsing("https://cp/mcp", "userA"); got != 2 {
		t.Errorf("FoldersUsing(cp,userA) = %d, want 2", got)
	}
	if got := st.Holds("https://cp/mcp", "userB"); got != 1 {
		t.Errorf("Holds(cp,userB) = %d, want 1", got)
	}
	removed, cleared := st.RemoveRecord("https://cp/mcp", "userA")
	if !removed || cleared != 2 {
		t.Errorf("RemoveRecord(cp,userA): removed=%v cleared=%d, want true/2", removed, cleared)
	}
	if len(st.Clouds) != 2 {
		t.Errorf("the other two records must survive: %+v", st.Clouds)
	}
	if st.RecordFor("https://cp/mcp", "userB") == nil {
		t.Errorf("HARM: removing user A's record on the shared machine took user B's record with it")
	}
	if st.RecordFor("https://other/mcp", "userA") == nil {
		t.Errorf("HARM: removing one control plane's record took the other control plane's with it")
	}
	if st.Folders[2].Cloud == nil || st.Folders[3].Cloud == nil {
		t.Errorf("HARM: refs belonging to OTHER records were cleared: %+v", st.Folders)
	}
	if st.Folders[0].Cloud != nil || st.Folders[1].Cloud != nil {
		t.Errorf("the removed record's own refs must be cleared: %+v", st.Folders[:2])
	}
	// A folder whose ref names no record refuses by name (VR10-T1-5).
	if _, err := st.Folders[0].Credential(&st); err == nil || !strings.Contains(err.Error(), "no control plane is configured for this folder") {
		t.Errorf("VR10-T1-5: want the 'no control plane is configured for this folder' refusal, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(f) — the token never appears in anything the router prints.
func TestGate_TokenNeverAppearsInRedactedState(t *testing.T) {
	st := State{
		Port:   8765,
		Clouds: []CloudRecord{{URL: "https://cp/mcp", User: "u", RouterID: "rtr", Token: gateTok}},
		Folders: []Folder{{
			Path: "C:/t/one", Hat: role.Test, Token: "rt-folder-secret",
			Upstreams: map[string]Upstream{"i": {URL: "http://x", Token: "upstream-secret"}},
			Cloud:     &CloudRef{URL: "https://cp/mcp", User: "u"},
		}},
	}
	out := Redacted(st)
	for _, secret := range []string{gateTok, "rt-folder-secret", "upstream-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("VR-R12: Redacted() leaked %q:\n%s", secret, out)
		}
	}
}

// VR10-T1-3 / VR-P7 — a product folder carrying a ref is refused on read, never honoured.
func TestGate_ProductFolderWithARefIsRefused(t *testing.T) {
	dir := t.TempDir()
	st := State{
		Clouds: []CloudRecord{{URL: "https://cp/mcp", User: "u", Token: gateTok}},
		Folders: []Folder{
			{Path: "C:/p/prod", Hat: role.Product, Token: "rt-prod", Cloud: &CloudRef{URL: "https://cp/mcp", User: "u"}},
		},
	}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecordToken(dir, "https://cp/mcp", "u"); err == nil {
		t.Errorf("VR-P7: a product folder carrying a cloud ref must be refused on read, got nil error")
	} else if !strings.Contains(err.Error(), "VR-P7") {
		t.Errorf("VR-P7: the refusal must name the rule; got %v", err)
	}
	if _, err := TableFrom(st); err == nil {
		t.Errorf("VR-P7: TableFrom must refuse a product folder with a ref, got nil error")
	}
}

// SetRecordToken writes ONE place and refuses when there is no record (the VR-B7 guard).
func TestGate_SetRecordToken_OnePlaceAndRefusesWithoutARecord(t *testing.T) {
	dir := t.TempDir()
	st := State{Clouds: []CloudRecord{
		{URL: "https://cp/mcp", User: "userA", RouterID: "rtr"},
		{URL: "https://cp/mcp", User: "userB", RouterID: "rtr", Token: gateTok + "-b"},
	}, Folders: []Folder{
		{Path: "C:/t/a", Hat: role.Test, Token: "1", Cloud: &CloudRef{URL: "https://cp/mcp", User: "userA"}},
	}}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := SetRecordToken(dir, "https://cp/mcp", "userA", gateTok+"-a", time.Now().Add(90*24*time.Hour)); err != nil {
		t.Fatalf("SetRecordToken: %v", err)
	}
	blob, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if n := strings.Count(string(blob), gateTok+"-a"); n != 1 {
		t.Errorf("VR10-T1-1: userA's token appears %d times on disk, want 1:\n%s", n, blob)
	}
	if !strings.Contains(string(blob), gateTok+"-b") {
		t.Errorf("HARM: writing userA's token destroyed userB's record token:\n%s", blob)
	}
	if err := SetRecordToken(dir, "https://cp/mcp", "userC", gateTok+"-c", time.Time{}); err == nil {
		t.Errorf("SetRecordToken for a user with NO record must refuse; got nil")
	}
	if err := SetRecordToken(dir, "https://cp/mcp", "userA", "  ", time.Time{}); err == nil {
		t.Errorf("SetRecordToken must refuse an EMPTY token; got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(d), the source of the defect — TWO records means TWO WRITERS of one state.json.
//
// SA §1.4.T3: "RunHeartbeat … is started ONCE PER RECORD by `router serve`". Each loop's
// OnRotatedToken calls SetRecordToken, which is load → mutate → SaveState over the WHOLE file, with
// no lock. Two records rotating at once therefore either lose one write (last writer wins with a
// stale snapshot) or fail the atomic rename outright. The loops are started in the same reconcile
// pass, so they beat at the same instant every interval — this is not an exotic interleaving.
func TestGate_ConcurrentSetRecordToken_TwoRecordsOneFile(t *testing.T) {
	lost, renameErr := 0, 0
	const rounds = 40
	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		st := State{Clouds: []CloudRecord{
			{URL: "https://cp/mcp", User: "userA", RouterID: "rtr", Token: "old-a"},
			{URL: "https://cp/mcp", User: "userB", RouterID: "rtr", Token: "old-b"},
		}}
		if err := SaveState(dir, st); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		for _, u := range []string{"userA", "userB"} {
			go func(user string) {
				<-start
				errs <- SetRecordToken(dir, "https://cp/mcp", user, "new-"+user, time.Time{})
			}(u)
		}
		close(start)
		wrote := map[string]bool{}
		for i := 0; i < 2; i++ {
			if err := <-errs; err != nil {
				renameErr++
			}
		}
		back, err := LoadState(dir)
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		for _, u := range []string{"userA", "userB"} {
			if rec := back.RecordFor("https://cp/mcp", u); rec != nil && rec.Token == "new-"+u {
				wrote[u] = true
			}
		}
		if len(wrote) < 2 {
			lost++
		}
	}
	if lost > 0 || renameErr > 0 {
		t.Errorf("HARM (VR10-T3-4 / SA §1.4.T3): two per-record heartbeat loops share ONE state.json with no lock.\n"+
			"  in %d rounds of two concurrent SetRecordToken calls (one per record):\n"+
			"    %d rounds ended with one record's NEW token missing from disk (lost update)\n"+
			"    %d SetRecordToken calls failed outright (the atomic rename collided)\n"+
			"  A lost update is an OUTAGE, not a cosmetic race: SetRecordToken returned nil, so\n"+
			"  Heartbeat.Send sets pendingAck and the next beat ACKNOWLEDGES the rotation — the control\n"+
			"  plane then revokes the old token (store.CompleteAutoRotation) while the machine still\n"+
			"  holds it. That is the exact trap cmd/argus/rotateack.go:15-18 says it prevents.",
			rounds, lost, renameErr)
	}
}

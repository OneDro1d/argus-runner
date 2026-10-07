package router

// V29 unit 2 — the author token minted during onboarding has ONE place on a machine: a CloudRecord per
// (control plane, user). Folders refer to it. Every constructor preserves it (V27-009's cause), and a 0.3.28
// state file is lifted into the new shape on load.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/role"
)

const cpURL = "https://cp.example/mcp"

func stateWithRecord(t *testing.T) State {
	t.Helper()
	st := State{Port: 9765}
	st.PutRecord(CloudRecord{URL: cpURL, User: "user_a", RouterID: "rtr_x", Token: "odts_aaaa", Expires: time.Now().Add(80 * 24 * time.Hour)})
	return st
}

func TestRecords_TheConstructorsPreserveTheRecord(t *testing.T) {
	st := stateWithRecord(t)
	spec := FolderSpec{Path: t.TempDir(), Hat: role.Test, InstanceID: "inst-1", Executor: Upstream{URL: "http://x", Token: "rt"}, Cloud: &CloudRef{URL: cpURL, User: "user_a"}}
	st2, f, _, err := UpsertFolder(st, spec)
	if err != nil {
		t.Fatalf("UpsertFolder: %v", err)
	}
	if len(st2.Clouds) != 1 || st2.Clouds[0].Token != "odts_aaaa" {
		t.Fatalf("UpsertFolder dropped the record (V27-009's cause): %+v", st2.Clouds)
	}
	st3, _, err := RemoveInstance(st2, f.Path, "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(st3.Clouds) != 1 {
		t.Fatalf("RemoveInstance dropped the record: %+v", st3.Clouds)
	}
	st4, _, err := RemoveFolder(st2, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st4.Clouds) != 1 {
		t.Fatalf("RemoveFolder dropped the record: %+v", st4.Clouds)
	}
}

func TestRecords_AFolderRefersToARecordAndRoutesWithItsToken(t *testing.T) {
	st := stateWithRecord(t)
	spec := FolderSpec{Path: t.TempDir(), Hat: role.Test, InstanceID: "inst-1", Executor: Upstream{URL: "http://exec", Token: "rt"}, Cloud: &CloudRef{URL: cpURL, User: "user_a"}}
	st, _, _, err := UpsertFolder(st, spec)
	if err != nil {
		t.Fatal(err)
	}
	if st.FoldersUsing(cpURL, "user_a") != 1 || st.Holds(cpURL, "user_a") != 1 {
		t.Fatalf("FoldersUsing=%d Holds=%d, want 1/1", st.FoldersUsing(cpURL, "user_a"), st.Holds(cpURL, "user_a"))
	}
	tbl, err := TableFrom(st)
	if err != nil {
		t.Fatal(err)
	}
	f, err := tbl.Resolve(st.Folders[0].Token)
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := f.Route("author__list_scenarios", "inst-1")
	if err != nil {
		t.Fatalf("Route(author): %v", err)
	}
	if tgt.Token != "odts_aaaa" || tgt.URL != cpURL || tgt.Plane != "author" {
		t.Fatalf("author target = %+v, want the record's token and URL", tgt)
	}
	// the file holds the token string exactly once, whatever the folder count
	dir := t.TempDir()
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	blob, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if n := strings.Count(string(blob), "odts_aaaa"); n != 1 {
		t.Fatalf("the token appears %d times in state.json, want exactly 1", n)
	}
}

func TestRecords_WiringTheCloudNeedsARecord_AndNeverAProductFolder(t *testing.T) {
	st := State{Port: 9765}
	_, _, _, err := UpsertFolder(st, FolderSpec{Path: t.TempDir(), Hat: role.Test, InstanceID: "i", Executor: Upstream{URL: "http://e", Token: "rt"}, Cloud: &CloudRef{URL: cpURL, User: "user_a"}})
	if err == nil || !strings.Contains(err.Error(), "router register") {
		t.Fatalf("a ref to a record the machine does not hold must be refused naming step 8a; got %v", err)
	}
	st = stateWithRecord(t)
	_, _, _, err = UpsertFolder(st, FolderSpec{Path: t.TempDir(), Hat: role.Product, InstanceID: "i", Executor: Upstream{URL: "http://e", Token: "rt"}, Cloud: &CloudRef{URL: cpURL, User: "user_a"}})
	if err == nil {
		t.Fatal("a PRODUCT folder must never be given a cloud reference (VR-P7)")
	}
}

func TestRecords_TwoUsersOneMachine_EachFolderUsesItsOwnRecord(t *testing.T) {
	st := stateWithRecord(t)
	st.PutRecord(CloudRecord{URL: cpURL, User: "user_b", RouterID: "rtr_x", Token: "odts_bbbb"})
	var err error
	st, _, _, err = UpsertFolder(st, FolderSpec{Path: t.TempDir(), Hat: role.Test, InstanceID: "ia", Executor: Upstream{URL: "http://e", Token: "rta"}, Cloud: &CloudRef{URL: cpURL, User: "user_a"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _, _, err = UpsertFolder(st, FolderSpec{Path: t.TempDir(), Hat: role.Test, InstanceID: "ib", Executor: Upstream{URL: "http://e", Token: "rtb"}, Cloud: &CloudRef{URL: cpURL, User: "user_b"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.RecordFor(cpURL, "") != nil {
		t.Fatal("with two records for one URL a nameless lookup must match nothing")
	}
	tbl, _ := TableFrom(st)
	fb, err := tbl.Resolve(st.Folders[1].Token) // the folder's ROUTER token, minted by UpsertFolder (not the executor token)
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := fb.Route("author__list_scenarios", "ib")
	if err != nil || tgt.Token != "odts_bbbb" {
		t.Fatalf("user_b's folder must route with user_b's token: %+v %v", tgt, err)
	}
	// removing user_a's record clears only user_a's ref
	removed, cleared := st.RemoveRecord(cpURL, "user_a")
	if !removed || cleared != 1 || len(st.Clouds) != 1 || st.Clouds[0].User != "user_b" {
		t.Fatalf("RemoveRecord: removed=%v cleared=%d clouds=%+v", removed, cleared, st.Clouds)
	}
}

func TestRecords_SetAndReadRecordToken(t *testing.T) {
	dir := t.TempDir()
	st := State{Port: 1}
	st.PutRecord(CloudRecord{URL: cpURL, User: "user_a", RouterID: "rtr_x"}) // registered, no token yet (step 8a)
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := SetRecordToken(dir, cpURL, "user_a", "", time.Time{}); err == nil {
		t.Fatal("an empty token must be refused")
	}
	if err := SetRecordToken(dir, cpURL, "nobody", "odts_x", time.Time{}); err == nil {
		t.Fatal("a token for a record the machine does not hold must be refused")
	}
	if err := SetRecordToken(dir, cpURL, "user_a", "odts_cccc", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	tok, err := ReadRecordToken(dir, cpURL, "user_a")
	if err != nil || tok != "odts_cccc" {
		t.Fatalf("ReadRecordToken = %q, %v", tok, err)
	}
	tok, err = ReadRecordToken(dir, cpURL, "") // the sole record for the URL
	if err != nil || tok != "odts_cccc" {
		t.Fatalf("nameless read of the sole record = %q, %v", tok, err)
	}
}

// A 0.3.28 state file: two test folders each carrying {URL, Token}, no machine-level entry (the measured estate).
func TestRecords_LegacyStateIsLiftedOnLoad(t *testing.T) {
	dir := t.TempDir()
	legacy := map[string]any{
		"port": 9765,
		"folders": []map[string]any{
			{"Path": "/c/tmp/test-a", "Hat": "test", "Token": "rt-a", "Upstreams": map[string]any{"ia": map[string]any{"URL": "http://e", "Token": "x"}},
				"Cloud": map[string]any{"URL": cpURL, "Token": "odts_old1"}},
			{"Path": "/c/tmp/prod", "Hat": "product", "Token": "rt-p", "Upstreams": map[string]any{"ia": map[string]any{"URL": "http://e", "Token": "y"}}},
			{"Path": "/c/tmp/test-b", "Hat": "test", "Token": "rt-b", "Upstreams": map[string]any{"ib": map[string]any{"URL": "http://e", "Token": "z"}},
				"Cloud": map[string]any{"URL": cpURL, "Token": "odts_old2"}},
		},
	}
	blob, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "state.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Clouds) != 1 || st.Clouds[0].URL != cpURL || st.Clouds[0].Token != "odts_old2" || st.Clouds[0].User != "" {
		t.Fatalf("lift: want ONE nameless record for the URL holding the LAST folder's token; got %+v", st.Clouds)
	}
	if st.Folders[0].Cloud == nil || st.Folders[2].Cloud == nil || st.Folders[1].Cloud != nil {
		t.Fatalf("lift: test folders must carry a ref and the product folder none: %+v", st.Folders)
	}
	if st.FoldersUsing(cpURL, "") != 2 || st.Holds(cpURL, "") != 1 {
		t.Fatalf("FoldersUsing=%d Holds=%d after lift, want 2/1", st.FoldersUsing(cpURL, ""), st.Holds(cpURL, ""))
	}
	// the lift is persisted: the file now holds the token ONCE and no legacy objects
	after, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Count(string(after), "odts_old") != 1 || strings.Contains(string(after), "odts_old1") {
		t.Fatalf("after the lift state.json must hold exactly one token (the last): %s", string(after))
	}
	// the first heartbeat names the owner: PutRecord fills the nameless record instead of adding a second
	replaced := st.PutRecord(CloudRecord{URL: cpURL, User: "user_a"})
	if !replaced || len(st.Clouds) != 1 || st.Clouds[0].User != "user_a" || st.Clouds[0].Token != "odts_old2" {
		t.Fatalf("filling the lifted record: replaced=%v clouds=%+v", replaced, st.Clouds)
	}
	tbl, err := TableFrom(st)
	if err != nil {
		t.Fatal(err)
	}
	fa, _ := tbl.Resolve("rt-a")
	if tgt, err := fa.Route("author__list_scenarios", "ia"); err != nil || tgt.Token != "odts_old2" {
		t.Fatalf("a lifted folder (nameless ref) must route with the named record's token: %+v %v", tgt, err)
	}
}

package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
)

// VR6-I1 (V23-008) — THE WIRING, not the writer. Rewritten for the V27-009 redesign (V29-02 §1.4.T1).
//
// 🚨 THIS IS THE TEST WHOSE ABSENCE COST SEVEN REPRODUCTIONS.
//
// internal/router covers the RECORD thoroughly — the constructors keep it, SetRecordToken/ReadRecordToken
// round-trip it, a 0.3.28 state is lifted into it. All of that PRE-SEEDS the token and then asserts about
// it. Round 5 shipped green on exactly that basis and the machine-level holder was never created once in
// production, because nothing drove the path that was supposed to create it.
//
// So this drives the actual command, `cloud-mint-token`, against a fake control plane, and asks the only
// question that matters: after an onboard's mint, does THIS MACHINE hold the author token (minted during
// onboarding) — on the record `router register` (step 8a) created, and nowhere else?
//
// Every assertion is on the WORLD: router.ReadRecordToken, and the number of times the token string
// appears in state.json (exactly once — a folder carries a CloudRef, never a copy).

// sessionJWT is a session token whose subject claim is sub — the shape onboard.SubjectFromToken reads,
// and the fact cloud-mint-token uses to pick WHOSE record the token lands on.
func sessionJWT(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	return "eyJhbGciOiJub25lIn0." + payload + ".sig"
}

// mintCP answers the two calls MintOrReuseAuthorToken makes: the reuse probe (GET /api/routers, which
// must 401 an unknown token and 200 a live one) and the mint (POST /api/tokens). It keeps every mint
// body, so a test can see WHETHER a mint happened and which machine it named.
type mintCP struct {
	*httptest.Server
	mu    sync.Mutex
	mints []map[string]any
}

func (f *mintCP) mintBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.mints...)
}

func fakeCP(t *testing.T, liveToken, mintReturns string) *mintCP {
	t.Helper()
	f := &mintCP{}
	mux := http.NewServeMux()
	// TokenWorks probes this. It is owner-scoped and must 401 an unknown credential — V15-015 was
	// caused by probing an endpoint that answered 403 for a perfectly live token.
	mux.HandleFunc("/api/routers", func(w http.ResponseWriter, r *http.Request) {
		if liveToken == "" || r.Header.Get("Authorization") != "Bearer "+liveToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"routers": []any{}})
	})
	mux.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		blob, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(blob, &body)
		f.mu.Lock()
		f.mints = append(f.mints, body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"token": mintReturns, "id": "tok_1"})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// readStateJSON returns the raw state file — "" when there is none, which is what a virgin router has.
func readStateJSON(t *testing.T, dir string) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read state.json: %v", err)
	}
	return string(blob)
}

// seedRegistered leaves what `router register` (onboarding step 8a) leaves: an identity and one
// token-less record per user for the control plane. Returns the identity, so a test can check that the
// mint named this machine.
func seedRegistered(t *testing.T, stateDir, recURL string, users ...string) router.Identity {
	t.Helper()
	ident, err := router.LoadOrCreateIdentity(stateDir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	st := router.State{Port: 9765}
	for _, u := range users {
		st.PutRecord(router.CloudRecord{URL: recURL, User: u, RouterID: ident.RouterID})
	}
	if err := router.SaveState(stateDir, st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return ident
}

// 🚩 A FRESH MINT. The record exists from step 8a and holds no token; the mint must land it there — on the
// SESSION's record when the machine holds records for two users — and the mint must name this machine.
func TestCloudMintToken_FreshMintWritesTheRecord(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "router")
	cp := fakeCP(t, "", "odts_fresh") // no live token → reuse probe fails → mint
	rec := recordURL(cp.URL)
	ident := seedRegistered(t, stateDir, rec, "user_a", "user_b")

	cf := &commonFlags{controlPlane: cp.URL, token: sessionJWT("user_a"), routerState: stateDir, name: "onboarding"}
	var rc int
	out := captureEmit(t, func() { rc = cmdCloudMintToken(cf) })
	if rc != exitOK {
		t.Fatalf("cloud-mint-token exit=%d, want %d: %v", rc, exitOK, out)
	}
	if out["reused"] != false || out["router_folders_updated"] != float64(1) {
		t.Errorf("a fresh mint must report reused=false and ONE store, got %v", out)
	}

	tok, err := router.ReadRecordToken(stateDir, rec, "user_a")
	if err != nil {
		t.Fatalf("ReadRecordToken: %v", err)
	}
	if tok == "" {
		t.Fatal("the mint completed and this machine holds NOTHING. That is V23-008 verbatim: a " +
			"working instance, and `has cloud key: FALSE`, so the next full teardown strands the account")
	}
	if tok != "odts_fresh" {
		t.Errorf("record token = %q, want the freshly minted one", tok)
	}
	if other, _ := router.ReadRecordToken(stateDir, rec, "user_b"); other != "" {
		t.Errorf("user_b's record gained user_a's token (%q) — the store must follow the session's subject", other)
	}
	if n := strings.Count(readStateJSON(t, stateDir), "odts_fresh"); n != 1 {
		t.Errorf("the token appears %d times in state.json, want exactly 1 (the record, and no copies)", n)
	}
	mints := cp.mintBodies()
	if len(mints) != 1 || mints[0]["router_id"] != ident.RouterID {
		t.Errorf("the auto mint must name THIS machine's router id %q once, got %v", ident.RouterID, mints)
	}
}

// A VIRGIN ROUTER — the shape measured seven times under the old design. There is no record to write to
// now, so the command must refuse, name the step that creates one, and store the token NOWHERE — not on a
// folder, not in a holder of its own making.
func TestCloudMintToken_NoRecordIsRefusedAndStoresNothing(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "router")
	cp := fakeCP(t, "", "odts_fresh")

	cf := &commonFlags{controlPlane: cp.URL, token: sessionJWT("user_a"), routerState: stateDir, name: "onboarding"}
	var rc int
	out := captureEmit(t, func() { rc = cmdCloudMintToken(cf) })
	if rc == exitOK {
		t.Fatal("cloud-mint-token succeeded on a machine with no record — the token has no place to live and " +
			"the run would report a working onboard")
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "router register") {
		t.Errorf("the refusal does not name `router register` (onboarding step 8a), the step that creates the record: %q", msg)
	}
	if strings.Contains(readStateJSON(t, stateDir), "odts_fresh") {
		t.Fatal("the token was written to state.json without a record to hold it")
	}
	if tok, _ := router.ReadRecordToken(stateDir, recordURL(cp.URL), ""); tok != "" {
		t.Fatalf("a record holding %q was invented by the mint — only `router register` creates records", tok)
	}
	if n := len(cp.mintBodies()); n != 0 {
		t.Fatalf("the control plane minted %d token(s) for a machine with no record — a live token no machine holds (VR-B7)", n)
	}
}

// The REUSE branch: the record already holds a live token, so the command must reuse it, mint nothing,
// and leave the record and the routing table exactly as they were.
func TestCloudMintToken_ReuseLeavesTheRecordAsItIs(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "router")
	cp := fakeCP(t, "odts_existing", "odts_should_not_be_minted")
	rec := recordURL(cp.URL)
	seedRegistered(t, stateDir, rec, "user_a")
	if err := router.SetRecordToken(stateDir, rec, "user_a", "odts_existing", time.Time{}); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	// A wired test folder refers to the record — routing must come out untouched.
	spec := wireSpec(t, t.TempDir(), role.Test, "inst-a")
	spec.Cloud = &router.CloudRef{URL: rec, User: "user_a"}
	if _, err := wireFolder(stateDir, spec, routerServerKey); err != nil {
		t.Fatalf("wire: %v", err)
	}

	cf := &commonFlags{controlPlane: cp.URL, token: sessionJWT("user_a"), routerState: stateDir, name: "onboarding"}
	var rc int
	out := captureEmit(t, func() { rc = cmdCloudMintToken(cf) })
	if rc != exitOK {
		t.Fatalf("cloud-mint-token exit=%d, want %d: %v", rc, exitOK, out)
	}
	if out["reused"] != true {
		t.Fatalf("the record holds a live token and the command did not reuse it: %v", out)
	}
	if n := len(cp.mintBodies()); n != 0 {
		t.Errorf("a reuse minted %d token(s) anyway — that is how 195 of them accumulated (VR-B3)", n)
	}

	tok, err := router.ReadRecordToken(stateDir, rec, "user_a")
	if err != nil || tok != "odts_existing" {
		t.Fatalf("record token after reuse = %q, %v; want the reused token, untouched", tok, err)
	}
	blob := readStateJSON(t, stateDir)
	if n := strings.Count(blob, "odts_existing"); n != 1 {
		t.Errorf("the token appears %d times in state.json, want exactly 1", n)
	}
	if strings.Contains(blob, "odts_should_not_be_minted") {
		t.Error("a token that must not have been minted is on disk")
	}
	st, err := router.LoadState(stateDir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(st.Folders) != 1 || st.Folders[0].Cloud == nil || st.Folders[0].Cloud.User != "user_a" {
		t.Errorf("the routing table was disturbed by a reuse: %+v", st.Folders)
	}
}

// The token survives what actually strands a machine: every folder unwired — by the command teardown
// runs, not by editing the state. The token is on the record, and RemoveInstance keeps the records.
func TestCloudMintToken_TheTokenSurvivesUnwiringEveryFolder(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "router")
	cp := fakeCP(t, "", "odts_fresh")
	rec := recordURL(cp.URL)
	seedRegistered(t, stateDir, rec, "user_a")
	folder := t.TempDir()
	spec := wireSpec(t, folder, role.Test, "inst-a")
	spec.Cloud = &router.CloudRef{URL: rec, User: "user_a"}
	if _, err := wireFolder(stateDir, spec, routerServerKey); err != nil {
		t.Fatalf("wire: %v", err)
	}
	cf := &commonFlags{controlPlane: cp.URL, token: sessionJWT("user_a"), routerState: stateDir, name: "onboarding"}
	if rc := cmdCloudMintToken(cf); rc != exitOK {
		t.Fatalf("mint exit=%d", rc)
	}
	before, err := router.LoadState(stateDir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if before.Holds(rec, "user_a") != 1 || before.FoldersUsing(rec, "user_a") != 1 {
		t.Fatalf("fixture: Holds=%d FoldersUsing=%d before the unwire, want 1/1",
			before.Holds(rec, "user_a"), before.FoldersUsing(rec, "user_a"))
	}

	// Teardown's effect: the folder's last instance goes, and the folder with it.
	res, err := unwireFolder(stateDir, folder, "inst-a", routerServerKey)
	if err != nil || !res.FolderDropped {
		t.Fatalf("unwire: %v %+v", err, res)
	}

	tok, err := router.ReadRecordToken(stateDir, rec, "user_a")
	if err != nil {
		t.Fatalf("ReadRecordToken: %v", err)
	}
	if tok != "odts_fresh" {
		t.Errorf("token after unwiring everything = %q, want it still held. This is the whole point of "+
			"a RECORD: teardown removes folders, and the account must not be stranded", tok)
	}
	after, err := router.LoadState(stateDir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(after.Folders) != 0 || after.Holds(rec, "user_a") != 1 || after.FoldersUsing(rec, "user_a") != 0 {
		t.Errorf("after the unwire: folders=%d Holds=%d FoldersUsing=%d, want 0/1/0",
			len(after.Folders), after.Holds(rec, "user_a"), after.FoldersUsing(rec, "user_a"))
	}
	if n := strings.Count(readStateJSON(t, stateDir), "odts_fresh"); n != 1 {
		t.Errorf("the token appears %d times in state.json after the unwire, want exactly 1", n)
	}
}

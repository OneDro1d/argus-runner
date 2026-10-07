package main

// V29 ADVERSARY GATE — CLI-side attacks on the per-machine token model (VR10-T1/T3/T4, SA §1.4.T4).
// Written blind from the specs; no builder test in this package was read first.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/router"
)

const gateCLITok = "odts_gatecli00000000000000000000000000000000000000"

// gateCapture runs fn with os.Stdout redirected and returns everything it printed.
func gateCapture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(c) — `cloud-mint-token` on a machine with NO record must mint NOTHING at the control plane.
//
// The fake control plane counts POST /api/tokens. VR-B7 / §1.4.T4: a mint that cannot be stored
// locally would leave a LIVE token no machine holds — the orphan this whole round exists to remove.
func TestGate_CloudMintToken_NoRecordMintsNothingAtTheControlPlane(t *testing.T) {
	var mints int64
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/api/tokens"):
			atomic.AddInt64(&mints, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"tok_fake","token":"` + gateCLITok + `","endpoint":"x","sse_endpoint":"y"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cp.Close()

	stateDir := t.TempDir() // a state dir with NO record at all
	if _, err := router.LoadOrCreateIdentity(stateDir); err != nil {
		t.Fatalf("identity: %v", err)
	}
	cf := &commonFlags{controlPlane: cp.URL, token: "session-token", name: "onboarding", routerState: stateDir}
	var rc int
	out := gateCapture(t, func() { rc = cmdCloudMintToken(cf) })
	if rc == exitOK {
		t.Errorf("VR-B7 / §1.4.T4: cloud-mint-token must REFUSE on a machine that holds no record; rc=%d out=%s", rc, out)
	}
	if n := atomic.LoadInt64(&mints); n != 0 {
		t.Errorf("HARM: %d POST /api/tokens were issued from a machine with no record — that is a LIVE author token nobody holds.\n"+
			"  output: %s", n, out)
	}
	if !strings.Contains(out, "nothing was minted") {
		t.Errorf("the refusal must say that nothing was minted (so the operator does not go revoking): %s", out)
	}
	if strings.Contains(out, gateCLITok) {
		t.Errorf("VR-R12: the CLI printed a token: %s", out)
	}

	// With a record present, the same call mints exactly once and writes the token to that record only.
	st := router.State{Clouds: []router.CloudRecord{{URL: recordURL(cp.URL), User: "", RouterID: "rtr"}}}
	if err := router.SaveState(stateDir, st); err != nil {
		t.Fatal(err)
	}
	out = gateCapture(t, func() { rc = cmdCloudMintToken(cf) })
	if rc != exitOK {
		t.Fatalf("with a record present the mint should succeed: rc=%d out=%s", rc, out)
	}
	if n := atomic.LoadInt64(&mints); n != 1 {
		t.Errorf("want exactly ONE mint, got %d", n)
	}
	blob, _ := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if n := strings.Count(string(blob), gateCLITok); n != 1 {
		t.Errorf("VR10-T1-1: the token appears %d times in state.json, want 1:\n%s", n, blob)
	}
	if strings.Contains(out, gateCLITok) {
		t.Errorf("VR-R12: the CLI printed the minted token on stdout: %s", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(d) — TWO records ⇒ two heartbeat loops; each acknowledges ONLY its own rotation and
// writes its own health sidecar (VR10-T3-4, VR10-T3-9, SA §1.4.T3).
func TestGate_TwoRecords_TwoLoops_EachAcksItsOwnRotation(t *testing.T) {
	type beat struct {
		RouterID    string `json:"router_id"`
		Owner       string `json:"owner"`
		RotationAck string `json:"rotation_ack"`
		Holds       int    `json:"holder_count"`
		Folders     int    `json:"test_folder_count"`
	}
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	seen := map[string][]beat{}
	offered := map[string]bool{}

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b beat
		_ = json.NewDecoder(r.Body).Decode(&b)
		<-mu
		seen[b.Owner] = append(seen[b.Owner], b)
		first := !offered[b.Owner]
		offered[b.Owner] = true
		mu <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"ok": true, "owner": b.Owner}
		if first {
			// Offer each record its OWN rotation, with an old-token-id naming the owner.
			resp["rotate_author_token"] = map[string]any{
				"token":        gateCLITok + "-" + b.Owner,
				"old_token_id": "old-" + b.Owner,
				"new_token_id": "new-" + b.Owner,
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer cp.Close()

	stateDir := t.TempDir()
	ident, err := router.LoadOrCreateIdentity(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	recURL := recordURL(cp.URL)
	st := router.State{
		Port: 8765,
		Clouds: []router.CloudRecord{
			{URL: recURL, User: "userA", RouterID: ident.RouterID, Token: gateCLITok + "-a"},
			{URL: recURL, User: "userB", RouterID: ident.RouterID, Token: gateCLITok + "-b"},
		},
		Folders: []router.Folder{
			{Path: "C:/t/a", Hat: role.Test, Token: "rt-a", Cloud: &router.CloudRef{URL: recURL, User: "userA"}},
		},
	}
	if err := router.SaveState(stateDir, st); err != nil {
		t.Fatal(err)
	}

	holder := router.NewIdentityHolder(ident)
	w := routerWiring{CP: cp.URL, Host: "gate-host", Version: "0.3.29", Port: 8765, StateDir: stateDir,
		Emit: func(any) {}, Boot: ident}

	// Drive ONE beat per record through the same assembly `router serve` uses (heartbeatFor), twice —
	// the second beat is where the acknowledgement rides (SA §1.4.T3: "the ack rides the next beat").
	stops := map[string]chan struct{}{}
	for _, user := range []string{"userA", "userB"} {
		hb := heartbeatFor(w, holder, recURL, user)
		hb.Interval = 10 * time.Millisecond
		stop := make(chan struct{})
		stops[user] = stop
		go router.RunHeartbeat(hb, stop, func(e error) { t.Logf("beat error for %s: %v", user, e) })
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		<-mu
		enough := len(seen["userA"]) >= 2 && len(seen["userB"]) >= 2
		mu <- struct{}{}
		if enough || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, stop := range stops {
		close(stop)
	}
	time.Sleep(50 * time.Millisecond)
	<-mu
	seen = map[string][]beat{"userA": append([]beat(nil), seen["userA"]...), "userB": append([]beat(nil), seen["userB"]...)}
	mu <- struct{}{}

	for _, user := range []string{"userA", "userB"} {
		beats := seen[user]
		if len(beats) < 2 {
			t.Fatalf("record %s: want at least 2 beats, got %d (%+v)", user, len(beats), beats)
		}
		if beats[0].Owner != user {
			t.Errorf("VR10-T3-3: the beat must carry owner=%q; got %q", user, beats[0].Owner)
		}
		acked := ""
		for _, b := range beats {
			if b.RotationAck != "" {
				acked = b.RotationAck
				break
			}
		}
		if acked != "old-"+user {
			t.Errorf("VR10-T3-9: record %s must acknowledge its OWN outgoing token id on a later beat; got %q (all beats: %+v)",
				user, acked, beats)
		}
	}
	// holder_count is per record; test_folder_count is FoldersUsing for THAT record.
	if seen["userA"][0].Holds != 1 || seen["userB"][0].Holds != 1 {
		t.Errorf("VR10-T3-4: holder_count must be 1 per record that holds a token: A=%d B=%d",
			seen["userA"][0].Holds, seen["userB"][0].Holds)
	}
	if seen["userA"][0].Folders != 1 || seen["userB"][0].Folders != 0 {
		t.Errorf("VR10-T3-4: test_folder_count must be FoldersUsing(record): A=%d (want 1) B=%d (want 0)",
			seen["userA"][0].Folders, seen["userB"][0].Folders)
	}

	// Each record's rotated token landed on ITS OWN record, and only there.
	back, err := router.LoadState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"userA", "userB"} {
		rec := back.RecordFor(recURL, user)
		if rec == nil {
			t.Fatalf("record %s vanished", user)
		}
		if rec.Token != gateCLITok+"-"+user {
			t.Errorf("HARM: record %s holds %q, want the token rotated to IT (%q) — a rotation landed on the wrong record",
				user, rec.Token, gateCLITok+"-"+user)
		}
	}

	// ONE HEALTH SIDECAR PER RECORD (VR9-I1 rule 4, per record since V27-009).
	all := router.LoadAllHeartbeatHealth(stateDir)
	if len(all) != 2 {
		t.Errorf("want one heartbeat-health sidecar per record, got %d: %+v", len(all), all)
	}
	users := map[string]bool{}
	for _, h := range all {
		users[h.RecordUser] = true
	}
	if !users["userA"] || !users["userB"] {
		t.Errorf("the sidecars must be keyed per record; got %v", users)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(e) — `router unrecord` clears that record's folder refs and leaves the other records.
func TestGate_RouterUnrecord_ClearsRefsAndLeavesOtherRecords(t *testing.T) {
	stateDir := t.TempDir()
	st := router.State{
		Clouds: []router.CloudRecord{
			{URL: "https://cp/mcp", User: "userA", Token: gateCLITok + "-a"},
			{URL: "https://cp/mcp", User: "userB", Token: gateCLITok + "-b"},
		},
		Folders: []router.Folder{
			{Path: "C:/t/a1", Hat: role.Test, Token: "1", Cloud: &router.CloudRef{URL: "https://cp/mcp", User: "userA"}},
			{Path: "C:/t/b1", Hat: role.Test, Token: "2", Cloud: &router.CloudRef{URL: "https://cp/mcp", User: "userB"}},
		},
	}
	if err := router.SaveState(stateDir, st); err != nil {
		t.Fatal(err)
	}
	var rc int
	out := gateCapture(t, func() {
		rc = cmdRouterUnrecord([]string{"--state", stateDir, "--control-plane", "https://cp/mcp", "--user", "userA"})
	})
	if rc != exitOK {
		t.Fatalf("router unrecord rc=%d out=%s", rc, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if res["removed"] != true {
		t.Errorf("router unrecord must report removed=true: %v", res)
	}
	if res["folder_refs_cleared"] != float64(1) {
		t.Errorf("SA §1.4.T4: it must print how many folder refs it cleared; got %v", res["folder_refs_cleared"])
	}
	if res["records_left"] != float64(1) {
		t.Errorf("HARM: the OTHER user's record did not survive; records_left=%v", res["records_left"])
	}
	back, err := router.LoadState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if back.RecordFor("https://cp/mcp", "userB") == nil {
		t.Errorf("HARM: unrecording user A removed user B's record from this machine")
	}
	if back.Folders[0].Cloud != nil {
		t.Errorf("user A's folder ref was not cleared: %+v", back.Folders[0])
	}
	if back.Folders[1].Cloud == nil {
		t.Errorf("HARM: user B's folder ref was cleared by user A's unrecord")
	}
	blob, _ := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if strings.Contains(string(blob), gateCLITok+"-a") {
		t.Errorf("user A's token survived the unrecord on disk:\n%s", blob)
	}
	if !strings.Contains(string(blob), gateCLITok+"-b") {
		t.Errorf("HARM: user B's token was destroyed by user A's unrecord:\n%s", blob)
	}
	if strings.Contains(out, gateCLITok) {
		t.Errorf("VR-R12: `router unrecord` printed a token: %s", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ATTACK 3(f) — the token never appears in `router status` output.
func TestGate_RouterStatus_NeverPrintsTheToken(t *testing.T) {
	stateDir := t.TempDir()
	st := router.State{
		Port: 8765,
		Clouds: []router.CloudRecord{
			{URL: "https://cp/mcp", User: "userA", RouterID: "rtr", Token: gateCLITok + "-a", Expires: time.Now().Add(90 * 24 * time.Hour)},
			{URL: "https://other/mcp", User: "userB", RouterID: "rtr", Token: gateCLITok + "-b"},
		},
		Folders: []router.Folder{
			{Path: "C:/t/a1", Hat: role.Test, Token: "folder-router-token-secret",
				Upstreams: map[string]router.Upstream{"i1": {URL: "http://x", Token: "upstream-secret"}},
				Cloud:     &router.CloudRef{URL: "https://cp/mcp", User: "userA"}},
		},
	}
	if err := router.SaveState(stateDir, st); err != nil {
		t.Fatal(err)
	}
	var rc int
	out := gateCapture(t, func() { rc = cmdRouterStatus([]string{"--state", stateDir}) })
	if rc != exitOK {
		t.Fatalf("router status rc=%d out=%s", rc, out)
	}
	for _, secret := range []string{gateCLITok + "-a", gateCLITok + "-b", "folder-router-token-secret", "upstream-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("VR-R12: `router status` leaked %q:\n%s", secret, out)
		}
	}
	// …but it must SAY that a token is held, per record.
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	clouds, _ := res["clouds"].([]any)
	if len(clouds) != 2 {
		t.Fatalf("router status must list one entry per record; got %d: %v", len(clouds), res["clouds"])
	}
	for _, c := range clouds {
		m, _ := c.(map[string]any)
		if m["holds"] != true {
			t.Errorf("a record carrying a token must report holds=true: %v", m)
		}
		if _, ok := m["token"]; ok {
			t.Errorf("VR-R12: `router status` carries a `token` field: %v", m)
		}
	}
}

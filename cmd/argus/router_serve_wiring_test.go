package main

import (
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

// VR9-H1 — THE WIRING OF `router serve`, WHICH NOTHING TESTED AT ALL.
//
// ── THE MUTATION THAT SURVIVED ───────────────────────────────────────────────────────────────────
//
// An adversary gate made one change in cmd/argus/main.go: the HolderCount field's body was changed
// to report the TEST-FOLDER count. The router then published the wrong predicate as its `holder_count`
// and the full suite came back byte-identical to baseline. Every test named for holder counts passed.
//
// ── WHY EVERY ONE OF THOSE MISSED IT ─────────────────────────────────────────────────────────────
//
// They test the PREDICATE (internal/router), the WIRE FORMAT (internal/control) and the STORAGE — all
// correctly. What none of them touched is the ASSEMBLY: which provider is assigned to which field of
// the Heartbeat that `router serve` actually constructs. So the assembly is a function, heartbeatFor,
// and this test asserts on the struct it returns.
//
// ── UNDER THE V27-009 REDESIGN ───────────────────────────────────────────────────────────────────
//
// `router serve` runs ONE heartbeat loop PER RECORD (runRecordHeartbeats), each assembled by heartbeatFor
// for its (control plane, user). The two predicates are per record: HolderCount is State.Holds — 1 when
// the record carries the author token (minted during onboarding), else 0 — and TestFolderCount is
// State.FoldersUsing — the test folders whose ref resolves to that record. Same trap as before: a fixture
// where the two coincide cannot see a swap, so this one holds the token ONCE and is used by TWO folders.

const wiringCP = "https://cp.example/mcp"

// wiringState writes the fixture: two records for one control plane (user_a holds a token, user_b does
// not) and four folders — two test folders using user_a's record, one test folder using none, and a
// product folder. Holds(user_a)=1 and FoldersUsing(user_a)=2: THE TWO COUNTS DIFFER, or a swap is invisible.
func wiringState(t *testing.T, dir string) router.State {
	t.Helper()
	st := router.State{Port: 9765}
	st.PutRecord(router.CloudRecord{URL: wiringCP, User: "user_a", RouterID: "rtr_x", Token: "odts_notarealtoken"})
	st.PutRecord(router.CloudRecord{URL: wiringCP, User: "user_b", RouterID: "rtr_x"})
	add := func(hat role.Role, id string, ref *router.CloudRef) {
		var err error
		st, _, _, err = router.UpsertFolder(st, router.FolderSpec{Path: filepath.Join(dir, "f-"+id), Hat: hat, InstanceID: id,
			Executor: router.Upstream{URL: "http://exec", Token: "rt-" + id}, Cloud: ref})
		if err != nil {
			t.Fatalf("fixture folder %s: %v", id, err)
		}
	}
	add(role.Test, "one", &router.CloudRef{URL: wiringCP, User: "user_a"})
	add(role.Test, "two", &router.CloudRef{URL: wiringCP, User: "user_a"})
	add(role.Test, "three", nil)   // wired, no record — neither a holder nor a user of one
	add(role.Product, "prod", nil) // VR-P7: never carries a ref, never counted either way
	if err := router.SaveState(dir, st); err != nil {
		t.Fatalf("seed router state: %v", err)
	}
	return st
}

// beatSink is a fake control plane that keeps every heartbeat body it receives and answers 200 {}.
type beatSink struct {
	*httptest.Server
	mu    sync.Mutex
	beats []map[string]any
}

func newBeatSink(t *testing.T) *beatSink {
	t.Helper()
	s := &beatSink{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fed/router/heartbeat" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		blob, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(blob, &body)
		s.mu.Lock()
		s.beats = append(s.beats, body)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(s.Server.Close)
	return s
}

// firstBeatByOwner returns the first beat seen for each `owner` value.
func (s *beatSink) firstBeatByOwner() map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]any{}
	for _, b := range s.beats {
		owner, _ := b["owner"].(string)
		if _, seen := out[owner]; !seen {
			out[owner] = b
		}
	}
	return out
}

func TestRouterServe_TheHeartbeatIsWiredToTheRightPredicates(t *testing.T) {
	stateDir := t.TempDir()
	st := wiringState(t, stateDir)
	// Guard the fixture itself: if these two ever coincide, every assertion below goes quiet.
	if st.Holds(wiringCP, "user_a") == st.FoldersUsing(wiringCP, "user_a") {
		t.Fatalf("the fixture has Holds == FoldersUsing == %d, so a swapped assignment would be "+
			"invisible. This test asserts nothing until they differ.", st.Holds(wiringCP, "user_a"))
	}

	// ⚠ A REAL IDENTITY, NOT router.Identity{}. An empty one has no key, and PublicKeyB64 slices 32
	// bytes out of it — the first version of this fixture PANICKED rather than asserting.
	// ⚠ THE IDENTITY LIVES IN stateDir, NOT A SCRATCH DIR. A first version minted it in a separate
	// TempDir, so the watcher had nothing to watch and the control failed with "cannot find the
	// file" — a fixture that could not see the defect it was written for.
	seedID, err := router.LoadOrCreateIdentity(stateDir)
	if err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	holder := router.NewIdentityHolder(seedID)
	stopWatch := startIdentityWatcher(stateDir, holder, func(any) {})
	defer stopWatch()
	w := routerWiring{CP: "https://cp.example", Host: "host-a", Version: "0.3.29", Port: 9765,
		StateDir: stateDir, Emit: func(any) {}, Boot: seedID}
	hb := heartbeatFor(w, holder, wiringCP, "user_a")

	t.Run("HolderCount reports Holds — the record carries the token — not the folders using it", func(t *testing.T) {
		if hb.HolderCount == nil {
			t.Fatal("the heartbeat carries no HolderCount provider at all, so the control plane is " +
				"told nothing and VR9-H1's whole mechanism is absent")
		}
		got := hb.HolderCount()
		if got == st.FoldersUsing(wiringCP, "user_a") {
			t.Fatalf("HolderCount() returned %d, which is FoldersUsing. The router is publishing the wrong "+
				"predicate as holder_count — a folder that merely USES the record is not a place the token "+
				"lives, and the control plane uses this number to decide whether to replace that token.", got)
		}
		if got != st.Holds(wiringCP, "user_a") {
			t.Fatalf("HolderCount() = %d, want %d (State.Holds: the record carries a token)", got, st.Holds(wiringCP, "user_a"))
		}
	})

	t.Run("TestFolderCount reports FoldersUsing, not Holds", func(t *testing.T) {
		if hb.TestFolderCount == nil {
			t.Fatal("the heartbeat carries no TestFolderCount provider")
		}
		got := hb.TestFolderCount()
		if got == st.Holds(wiringCP, "user_a") {
			t.Fatalf("TestFolderCount() returned %d, which is Holds — the two providers are swapped", got)
		}
		if got != st.FoldersUsing(wiringCP, "user_a") {
			t.Fatalf("TestFolderCount() = %d, want %d (State.FoldersUsing)", got, st.FoldersUsing(wiringCP, "user_a"))
		}
	})

	// ⚠ CALLBACKS, NOT VALUES. RunHeartbeat takes its Heartbeat by value, so a count captured at start-up
	// would report what the machine had when the router booted — VR9-I1's own defect rebuilt.
	t.Run("the counts are PER RECORD and re-read the file on every call", func(t *testing.T) {
		hbB := heartbeatFor(w, holder, wiringCP, "user_b")
		if hbB.HolderCount() != 0 || hbB.TestFolderCount() != 0 {
			t.Fatalf("user_b's record holds no token and no folder uses it, yet its heartbeat reports %d/%d",
				hbB.HolderCount(), hbB.TestFolderCount())
		}
		if err := router.SetRecordToken(stateDir, wiringCP, "user_b", "odts_b", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if got := hbB.HolderCount(); got != 1 {
			t.Fatalf("HolderCount() = %d after user_b's record gained its token, want 1 — a value read once "+
				"at start-up is VR9-I1's own defect rebuilt", got)
		}
		if got := hb.HolderCount(); got != 1 {
			t.Fatalf("user_a's HolderCount() = %d after user_b's record changed, want 1 — the counts leak across records", got)
		}
	})

	// ⛔ "I CANNOT TELL" MUST NEVER ARRIVE AS "I HOLD NONE". Zero is the control plane's trigger to
	// replace the account's token; -1 is the sentinel that stops it. An unreadable state dir is the
	// case that distinguishes them, and it is the one a builder is most likely to "simplify".
	t.Run("an UNREADABLE state file reports -1, never 0", func(t *testing.T) {
		// ⚠ AN ABSENT STATE DIR IS NOT THE CASE THIS IS ABOUT. LoadState treats a missing file as an
		// empty state deliberately — a machine with nothing recorded genuinely holds the token in zero
		// places, and 0 is the honest answer. The sentinel is for "I could not find out": a state file
		// that EXISTS and cannot be parsed. Those two must not collapse into the same number.
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, "state.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		wb := w
		wb.StateDir = broken
		missing := heartbeatFor(wb, holder, wiringCP, "user_a")
		if got := missing.HolderCount(); got != -1 {
			t.Errorf("HolderCount() = %d for an unreadable state dir, want -1. Reporting 0 tells the "+
				"control plane this machine holds the token nowhere, which is its cue to revoke and "+
				"reissue — on a machine that may be holding it perfectly well.", got)
		}
		if got := missing.TestFolderCount(); got != -1 {
			t.Errorf("TestFolderCount() = %d for an unreadable state dir, want -1", got)
		}
	})

	// VR9-I1 rule 4 — the sidecar path. Without StateDir the beat outcome is recorded only in the
	// daemon's memory, and `router status` (a SEPARATE process) can never see the escalation.
	t.Run("StateDir is wired so router status can read the beat outcome", func(t *testing.T) {
		if hb.StateDir != stateDir {
			t.Errorf("StateDir = %q, want %q. An adversary gate deleted this one field and the suite "+
				"stayed green: heartbeat-health.json is never written, and a machine refused every 60 "+
				"seconds reports nothing forever.", hb.StateDir, stateDir)
		}
	})

	t.Run("the identity of the control plane, and whose record this is, are carried through", func(t *testing.T) {
		if hb.CPURL != "https://cp.example" {
			t.Errorf("CPURL = %q", hb.CPURL)
		}
		if hb.Host != "host-a" {
			t.Errorf("Host = %q", hb.Host)
		}
		if hb.Version != "0.3.29" {
			t.Errorf("Version = %q — the control plane uses this to decide what to recommend", hb.Version)
		}
		if hb.Port != 9765 {
			t.Errorf("Port = %d", hb.Port)
		}
		if hb.Owner != "user_a" {
			t.Errorf("Owner = %q, want the record's user — the beat must say whose record it reports on", hb.Owner)
		}
	})

	// Both sinks must exist and must write THE RECORD. OnRotatedToken refuses when there is no record;
	// OnOwner is how a lifted 0.3.28 record (no user yet) learns whose it is from the first accepted beat.
	t.Run("the token sinks are present and write the record", func(t *testing.T) {
		if hb.OnRotatedToken == nil {
			t.Fatal("OnRotatedToken is nil — a rotation would be acknowledged and then dropped")
		}
		if hb.OnOwner == nil {
			t.Fatal("OnOwner is nil — a lifted record would never learn whose it is, and stay nameless forever")
		}
		if err := hb.OnRotatedToken("odts_rotated"); err != nil {
			t.Fatalf("OnRotatedToken against a record: %v", err)
		}
		if tok, _ := router.ReadRecordToken(stateDir, wiringCP, "user_a"); tok != "odts_rotated" {
			t.Fatalf("after OnRotatedToken user_a's record reads %q, want the rotated token", tok)
		}
		blob, _ := os.ReadFile(filepath.Join(stateDir, "state.json"))
		if n := strings.Count(string(blob), "odts_rotated"); n != 1 {
			t.Fatalf("the rotated token appears %d times in state.json, want exactly 1", n)
		}

		lifted := t.TempDir()
		if err := router.SaveState(lifted, router.State{Port: 1, Clouds: []router.CloudRecord{{URL: wiringCP, Token: "odts_lifted"}}}); err != nil {
			t.Fatal(err)
		}
		wl := w
		wl.StateDir = lifted
		hbL := heartbeatFor(wl, holder, wiringCP, "")
		hbL.OnOwner("user_z")
		stL, err := router.LoadState(lifted)
		if err != nil {
			t.Fatal(err)
		}
		if len(stL.Clouds) != 1 || stL.Clouds[0].User != "user_z" || stL.Clouds[0].Token != "odts_lifted" {
			t.Fatalf("OnOwner must fill the nameless record IN THE FILE and keep its token, got %+v", stL.Clouds)
		}
	})

	// ⛔ A HOST/STATEDIR TRANSPOSITION IS NAMED, BECAUSE ITS HARM IS SILENT.
	//
	// An adversary gate swapped two plain strings in what was then a seven-argument positional call,
	// and 726 tests stayed green. In production that reports the state-dir PATH as the hostname: the
	// upsert's ON CONFLICT arbiter misses, the control plane answers 500 on every beat, and the UI
	// shows only "stale" — a machine can sit like that indefinitely with nothing naming the cause.
	//
	// The call site uses NAMED FIELDS, which makes the mistake visible; runRecordHeartbeats makes it AUDIBLE.
	t.Run("a host that looks like a path is called out", func(t *testing.T) {
		sink := newBeatSink(t) // so the loops this starts beat at a fake, never at the network
		var mu sync.Mutex
		var said []string
		stop := runRecordHeartbeats(routerWiring{
			CP: sink.URL, Host: stateDir, Version: "0.3.29", Port: 9765,
			StateDir: stateDir, Emit: func(v any) {
				if m, ok := v.(map[string]any); ok {
					if warn, ok := m["warn"].(string); ok {
						mu.Lock()
						said = append(said, warn)
						mu.Unlock()
					}
				}
			}, Boot: seedID,
		})
		stop()
		mu.Lock()
		joined := strings.Join(said, " | ")
		mu.Unlock()
		if !strings.Contains(joined, "transposed") {
			t.Errorf("a hostname that is plainly a filesystem path was accepted in silence. The control "+
				"plane will answer 500 on every beat and the operator will see only stale."+
				"\n\nwarnings: %q", joined)
		}
	})

	// ⛔ THE ASSEMBLY, OBSERVED ON THE WIRE. heartbeatFor is asserted on above; this drives what `router
	// serve` actually runs — runRecordHeartbeats — and reads the beats a fake control plane receives:
	// one loop per record, each naming its owner, each carrying ITS record's counts, all signed as this
	// machine. A build that ran one loop for the machine, or wired every loop to the same record, or
	// passed the wrong predicates through, is visible here and nowhere else.
	t.Run("router serve runs ONE loop PER RECORD, each beating as its owner with its own counts", func(t *testing.T) {
		dir := t.TempDir()
		wiringState(t, dir)
		id, err := router.LoadOrCreateIdentity(dir)
		if err != nil {
			t.Fatal(err)
		}
		sink := newBeatSink(t)
		stop := runRecordHeartbeats(routerWiring{CP: sink.URL, Host: "host-a", Version: "0.3.29", Port: 9765,
			StateDir: dir, Emit: func(any) {}, Boot: id})
		defer stop()

		deadline := time.After(5 * time.Second)
		var beats map[string]map[string]any
		for {
			beats = sink.firstBeatByOwner()
			if beats["user_a"] != nil && beats["user_b"] != nil {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("after 5s the control plane has seen beats for owners %v; want user_a AND user_b — "+
					"one loop per record is what lets each account's token be rotated independently", ownersOf(beats))
			case <-time.After(50 * time.Millisecond):
			}
		}
		a, b := beats["user_a"], beats["user_b"]
		if a["holder_count"] != float64(1) || a["test_folder_count"] != float64(2) {
			t.Errorf("user_a's beat carries holder_count=%v test_folder_count=%v, want 1/2 (Holds/FoldersUsing of ITS record)",
				a["holder_count"], a["test_folder_count"])
		}
		if b["holder_count"] != float64(0) || b["test_folder_count"] != float64(0) {
			t.Errorf("user_b's beat carries holder_count=%v test_folder_count=%v, want 0/0 — it is reporting another record's counts",
				b["holder_count"], b["test_folder_count"])
		}
		for owner, beat := range beats {
			if beat["router_id"] != id.RouterID {
				t.Errorf("%s's beat is signed as router %v, want this machine's %q", owner, beat["router_id"], id.RouterID)
			}
		}
	})

	// ⛔ THE IDENTITY PROVIDER MUST TRACK THE HOLDER, NOT A VALUE READ ONCE.
	//
	// An adversary gate replaced the holder's Load at the call site with a constant — V27-003
	// restored verbatim — and the ENTIRE suite stayed green. `router register` mints a new
	// identity.key under the running process; with a constant provider every later beat is signed with
	// the start-up key, the control plane answers 401 to all of them SILENTLY, and no token rotation
	// reaches the machine until someone restarts it.
	t.Run("the identity provider follows a key written under the running process", func(t *testing.T) {
		// THE WHOLE CHAIN: watcher -> holder -> provider, driven by the only thing production drives —
		// a new identity.key on disk, exactly what `router register` writes.
		if hb.Identity == nil {
			t.Fatal("the heartbeat carries no identity provider; RunHeartbeat would sign with nothing")
		}
		first := hb.Identity()
		if err := os.Remove(filepath.Join(stateDir, "identity.key")); err != nil {
			t.Fatalf("remove identity: %v", err)
		}
		next, err := router.LoadOrCreateIdentity(stateDir)
		if err != nil {
			t.Fatalf("mint a second identity: %v", err)
		}
		if next.PublicKeyB64() == first.PublicKeyB64() {
			t.Fatal("the two identities are identical, so this case cannot see a stale provider")
		}
		deadline := time.After(6 * router.ReloadInterval)
		for {
			if hb.Identity().PublicKeyB64() == next.PublicKeyB64() {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("the provider still returns the OLD key %v after identity.key was replaced. Either nothing is watching the file, or the provider is a CONSTANT — both mean every beat after a `router register` is signed with the start-up key and answered 401 in silence.", 6*router.ReloadInterval)
			case <-time.After(50 * time.Millisecond):
			}
		}
	})

	// ⛔ AND SOMETHING MUST ACTUALLY START THE WATCHER. An adversary gate deleted the whole
	// `go router.WatchIdentity(...)` goroutine from `router serve` and the suite stayed green —
	// zerointerval_test.go proves the interval FALLBACK works when WatchIdentity is called, and named
	// the call site only in a comment. The watcher is started by startIdentityWatcher, so the wiring
	// is a function that can be driven: write a new key, and the holder must follow.
	t.Run("the watcher republishes a key written under the running process", func(t *testing.T) {
		dir := t.TempDir()
		first, err := router.LoadOrCreateIdentity(dir)
		if err != nil {
			t.Fatalf("seed identity: %v", err)
		}
		h := router.NewIdentityHolder(first)
		stop := startIdentityWatcher(dir, h, func(any) {})
		defer stop()

		// `router register` mints a new identity.key under the running server. That is the real trigger.
		time.Sleep(router.ReloadInterval / 2)
		if err := os.Remove(filepath.Join(dir, "identity.key")); err != nil {
			t.Fatalf("remove identity: %v", err)
		}
		second, err := router.LoadOrCreateIdentity(dir)
		if err != nil {
			t.Fatalf("mint a second identity: %v", err)
		}
		if second.PublicKeyB64() == first.PublicKeyB64() {
			t.Fatal("the two identities are identical; this case cannot see a dead watcher")
		}
		deadline := time.After(6 * router.ReloadInterval)
		for {
			if h.Load().PublicKeyB64() == second.PublicKeyB64() {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("the holder still carries the OLD key %v after identity.key was replaced. Nothing is watching the file: every heartbeat after a `router register` is signed with the start-up key and answered 401 in silence — V27-003 exactly.", 6*router.ReloadInterval)
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
}

func ownersOf(beats map[string]map[string]any) []string {
	var out []string
	for owner := range beats {
		out = append(out, owner)
	}
	return out
}

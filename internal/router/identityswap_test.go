package router

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// VR9-I1 — A RUNNING ROUTER MUST SIGN WITH ITS CURRENT IDENTITY.
//
// ── THE DEFECT, AND WHY NOTHING CAUGHT IT ─────────────────────────────────────────────────────────
//
// `router serve` read the identity ONCE and RunHeartbeat took it BY VALUE, so when `router register`
// minted a new identity.key underneath a running server every later beat was still signed with the OLD
// key. The control plane answered 401 to all of them — silently, because a heartbeat that does not land
// is deliberately non-fatal (the router's job is routing). No rotation could ever reach that machine.
// Restarting the container fixed it instantly, which is why it survived so long.
//
// ⛔ SO THE ASSERTION IS *WHICH KEY SIGNED*, NEVER "a heartbeat happened". A test that reads the
// router's logs on a happy path proves nothing here: the entire symptom is silence.
func TestRunHeartbeat_SignsWithTheCurrentIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A second, DIFFERENT identity — what `router register` mints under a running server.
	second, err := LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first.RouterID == second.RouterID {
		t.Fatal("fixture is broken: the two identities must differ")
	}

	var mu sync.Mutex
	var signers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RouterID string `json:"router_id"`
			JWT      string `json:"jwt"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		// Verify against BOTH keys and record which one actually signed. Trusting router_id alone
		// would let a build that swapped the id but kept the key look correct.
		who := "unknown"
		if _, e := federation.VerifyJWT(body.JWT, first.Priv.Public().(ed25519.PublicKey), time.Now()); e == nil {
			who = "first"
		}
		if _, e := federation.VerifyJWT(body.JWT, second.Priv.Public().(ed25519.PublicKey), time.Now()); e == nil {
			who = "second"
		}
		mu.Lock()
		signers = append(signers, who)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The provider is what makes the swap possible: RunHeartbeat takes h BY VALUE, so anything captured
	// in the struct freezes at start-up.
	var idMu sync.Mutex
	cur := first
	provider := func() Identity {
		idMu.Lock()
		defer idMu.Unlock()
		return cur
	}

	hb := Heartbeat{CPURL: srv.URL, Host: "PC-1", Version: "0.3.28", Port: 9765}
	// Beat once with the first identity...
	if err := hb.Send(provider(), time.Now()); err != nil {
		t.Fatalf("first beat: %v", err)
	}
	// ...then the identity changes underneath, exactly as `router register` does...
	idMu.Lock()
	cur = second
	idMu.Unlock()
	// ...and the next beat must carry the NEW one.
	if err := hb.Send(provider(), time.Now()); err != nil {
		t.Fatalf("second beat: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(signers) != 2 {
		t.Fatalf("got %d beats, want 2", len(signers))
	}
	if signers[0] != "first" {
		t.Errorf("beat 1 was signed by %q, want the first identity", signers[0])
	}
	if signers[1] != "second" {
		t.Fatalf("beat 2 was signed by %q, want the SECOND identity. The router is still signing with "+
			"the key it read at start-up, so every beat after a re-register 401s — silently — and no "+
			"token rotation can reach this machine.", signers[1])
	}
}

// RunHeartbeat must take a PROVIDER, not a value. This is the compile-level half of the rule: if the
// signature still took an Identity, a swap would be impossible however the caller was written.
func TestRunHeartbeat_TakesAnIdentityProvider(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	var beats int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		beats++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stop := make(chan struct{})
	// ⚠ atomic, not a plain int. The provider runs on the BEAT goroutine and this assertion reads it
	// from the test goroutine — a plain counter is a data race, and -race cannot run on this host
	// (CGO_ENABLED=0), so it would never be reported.
	var calls atomic.Int64
	go RunHeartbeat(Heartbeat{CPURL: srv.URL, Host: "PC-1", Port: 9765,
		Identity: func() Identity { calls.Add(1); return id }}, stop, nil)
	// The first beat is immediate; give it a moment, then stop.
	time.Sleep(150 * time.Millisecond)
	close(stop)
	mu.Lock()
	defer mu.Unlock()
	if beats == 0 {
		t.Fatal("no beat landed")
	}
	if calls.Load() == 0 {
		t.Fatal("the provider was never called — the identity is still captured by value")
	}
}

// VR9-I1 rule 2 — the loader must say WHICH it did, so onboarding can act on a fresh mint.
func TestLoadOrCreateIdentity_ReportsWhetherItMinted(t *testing.T) {
	dir := t.TempDir()
	_, created, err := LoadOrCreateIdentityCreated(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("a fresh directory must report created=true")
	}
	_, created, err = LoadOrCreateIdentityCreated(dir)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("an existing identity must report created=false — the caller cannot otherwise tell a " +
			"fresh mint from a load, which is what decides whether a running router must be recreated")
	}
}

// VR9-I1 rule 1's watcher. It stats ONLY identity.key: folding it into the state watcher would make an
// identity change trigger a pointless table reload, and a table change re-read the identity.
func TestWatchIdentity_NoticesAChangedKey(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan Identity, 4)
	stop := make(chan struct{})
	defer close(stop)
	go WatchIdentity(dir, 10*time.Millisecond, stop, func(id Identity) { got <- id }, nil)

	// Replace the key the way `router register` does.
	otherDir := t.TempDir()
	other, err := LoadOrCreateIdentity(otherDir)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(filepath.Join(otherDir, identityFile))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the watcher take its baseline first
	if err := os.WriteFile(filepath.Join(dir, identityFile), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case id := <-got:
		if id.RouterID != other.RouterID {
			t.Fatalf("watcher reported %q, want the new identity %q", id.RouterID, other.RouterID)
		}
		if id.RouterID == first.RouterID {
			t.Fatal("the watcher handed back the OLD identity")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher never noticed identity.key changing — a fix built on the state watcher " +
			"cannot fire here, because in this reproduction only identity.key changes")
	}
}

// ⛔ THE COMPOSITION TEST, AND THE REASON IT EXISTS.
//
// The three tests above are each necessary and none of them is sufficient. Two of them fail against the
// original code only by NOT COMPILING, and a compile error is a weak positive control: it proves a
// signature changed, not that a key change reaches the wire. The marquee test drives Send directly, and
// Send ALWAYS took an identity parameter — so on the original code, had it compiled, it would have
// PASSED. That is precisely the "assertion that only restates the implementation" this project keeps
// getting caught by.
//
// The defect never lived in Send. It lived in the WIRING: `router serve` read identity.key once at
// start-up and handed that value to the beat loop, so a key minted underneath a running server was
// never picked up. That wiring had no test at all, because it lived in package main.
//
// So this test wires the real parts together the way `router serve` does — watcher goroutine, holder,
// beat loop — writes a new key to disk the way `router register` does, and asserts A LATER BEAT IS
// SIGNED BY THE NEW KEY. It fails against a constant provider, which IS the original wiring, without
// needing a compile error to do it.
func TestRouterServe_ANewKeyOnDiskReachesTheNextBeat(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	otherDir := t.TempDir()
	second, err := LoadOrCreateIdentity(otherDir)
	if err != nil {
		t.Fatal(err)
	}

	signed := make(chan string, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			JWT string `json:"jwt"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		who := "unknown"
		if _, e := federation.VerifyJWT(body.JWT, first.Priv.Public().(ed25519.PublicKey), time.Now()); e == nil {
			who = "first"
		}
		if _, e := federation.VerifyJWT(body.JWT, second.Priv.Public().(ed25519.PublicKey), time.Now()); e == nil {
			who = "second"
		}
		select {
		case signed <- who:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Exactly `router serve`'s composition: a holder seeded with what was on disk at boot, a watcher
	// republishing into it, and a beat loop reading it every beat.
	holder := NewIdentityHolder(first)
	stop := make(chan struct{})
	defer close(stop)
	go WatchIdentity(dir, 10*time.Millisecond, stop, holder.Store, nil)
	go RunHeartbeat(Heartbeat{CPURL: srv.URL, Host: "PC-1", Port: 9765, Interval: 10 * time.Millisecond,
		Identity: holder.Load}, stop, nil)

	waitFor := func(want string, within time.Duration) {
		t.Helper()
		deadline := time.After(within)
		var saw []string
		for {
			select {
			case who := <-signed:
				if who == want {
					return
				}
				saw = append(saw, who)
			case <-deadline:
				t.Fatalf("no beat signed by %q within %v; saw %v — the running router is still signing "+
					"with the key it read at start-up, so every beat after a re-register 401s silently "+
					"and no rotation can reach this machine", want, within, saw)
			}
		}
	}

	waitFor("first", 2*time.Second)

	// `router register` mints a new identity under the running server.
	blob, err := os.ReadFile(filepath.Join(otherDir, identityFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, identityFile), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	waitFor("second", 3*time.Second)
}

// The holder is the goroutine boundary itself, so its own contract is asserted rather than assumed:
// a zero holder must not hand out a torn value, and a Store must be visible to the next Load.
func TestIdentityHolder(t *testing.T) {
	id, err := LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewIdentityHolder(id)
	if h.Load().RouterID != id.RouterID {
		t.Fatalf("Load = %q, want the seeded %q", h.Load().RouterID, id.RouterID)
	}
	next, err := LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.Store(next)
	if h.Load().RouterID != next.RouterID {
		t.Fatalf("Load = %q after Store, want %q", h.Load().RouterID, next.RouterID)
	}
	var zero IdentityHolder
	if got := zero.Load(); got.RouterID != "" || got.Priv != nil {
		t.Fatalf("the zero holder handed out %+v, want an empty identity", got.RouterID)
	}
}

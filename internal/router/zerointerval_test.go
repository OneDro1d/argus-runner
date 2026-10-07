package router

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// VR9-I1 — THE ZERO-INTERVAL DEFAULT IS LOAD-BEARING, AND NOTHING EXERCISED IT.
//
// ── HOW THIS WAS FOUND ───────────────────────────────────────────────────────────────────────────
//
// Every test of WatchIdentity passed an explicit interval (10ms, so the test is quick). The ONLY
// production caller passes 0:
//
//	cmd/argus/main.go:1897   go router.WatchIdentity(*stateDir, 0, nil, func(next router.Identity){…}, …)
//
// So `if interval <= 0 { interval = ReloadInterval }` in reload.go was covered by no test at all. An
// adversary gate deleted those three lines: the full suite stayed green at 1889 PASS / 0 FAIL, while
// `time.NewTicker(0)` panics with "non-positive interval for NewTicker". The panic is in a goroutine
// with no recover, so it takes the process down — `router serve` would have crashed at start-up on
// every machine with ARGUS_CP_URL set, which is every onboarded machine.
//
// ⛔ A CONSTANT THAT NO TEST EXERCISES CAN BE ANYTHING. This one could be absent.
//
// ── WHY THE TEST IS SHAPED LIKE THIS ─────────────────────────────────────────────────────────────
//
// A panic in a watcher goroutine cannot be recovered from the test goroutine — which is exactly what
// makes this test work: if the default is removed, the test BINARY dies and the package fails. There
// is no way to make that outcome look like a pass.
//
// And it asserts more than "did not panic": it waits for a real change to be noticed, so a default of
// (say) 24 hours — which also would not panic — fails too. The default has to be a WORKING interval.
func TestWatchIdentity_ZeroIntervalFallsBackToAWorkingDefault(t *testing.T) {
	if ReloadInterval <= 0 {
		t.Fatalf("ReloadInterval is %v; the fallback cannot be non-positive or NewTicker panics", ReloadInterval)
	}
	// The whole point of the fallback is that it is short enough to be useful. A production default
	// longer than this would make an identity swap take minutes to be noticed, which is the defect
	// VR9-I1 exists to close.
	if ReloadInterval > 10*time.Second {
		t.Fatalf("ReloadInterval is %v — too long for the identity watcher the router relies on", ReloadInterval)
	}

	dir := t.TempDir()
	if _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	changed := make(chan Identity, 4)
	errs := make(chan error, 4)
	stop := make(chan struct{})
	defer close(stop)

	// ⛔ INTERVAL 0 — EXACTLY WHAT main.go PASSES. If reload.go's fallback is gone, this call panics
	// inside the goroutine and the test binary dies. That is the intended failure.
	go WatchIdentity(dir, 0, stop, func(next Identity) { changed <- next }, func(e error) { errs <- e })

	// Give the watcher a tick to take its baseline before changing anything underneath it.
	time.Sleep(ReloadInterval / 2)

	// `router register` mints a new identity.key under the running server. That is the real trigger.
	if err := os.Remove(filepath.Join(dir, identityFile)); err != nil {
		t.Fatalf("remove identity: %v", err)
	}
	second, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("mint a second identity: %v", err)
	}

	deadline := time.After(4 * ReloadInterval)
	for {
		select {
		case got := <-changed:
			if got.PublicKeyB64() != second.PublicKeyB64() {
				t.Fatalf("the watcher reported a key that is neither the old nor the new one")
			}
			return // the default interval is working
		case e := <-errs:
			// A transient read of a half-written file is legitimate; keep waiting.
			t.Logf("watcher reported (tolerated): %v", e)
		case <-deadline:
			t.Fatalf("the watcher did not notice a replaced identity.key within %v. It was started "+
				"with interval 0, so it is running on reload.go's fallback — which is either gone, or "+
				"is no longer a usable period. Every heartbeat after a `router register` would keep "+
				"signing with the old key and be answered 401 in silence.", 4*ReloadInterval)
		}
	}
}

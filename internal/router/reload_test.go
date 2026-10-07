package router

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// waitFor polls a condition rather than sleeping a fixed time — a fixed sleep either makes the test
// slow or makes it flaky on a loaded machine, and this suite already runs on one.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatchState_AFolderWiredWhileServingBecomesRoutable(t *testing.T) {
	dir := t.TempDir()
	st, first, _, err := UpsertFolder(State{Port: 9765}, spec("/w/a", role.Test, "suta"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	tbl, _ := TableFrom(st)

	stop := make(chan struct{})
	defer close(stop)
	ready := make(chan struct{}, 1)
	go WatchState(dir, tbl, 10*time.Millisecond, stop, ready, nil, nil)
	<-ready // the watcher's baseline stat is taken; a write from here on is guaranteed to be seen as a change

	// The folder that did not exist when the router started.
	st2, second, _, err := UpsertFolder(st, spec("/w/b", role.Product, "sutb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tbl.Resolve(second.Token); err == nil {
		t.Fatal("the new folder resolved before its state was even written")
	}
	if err := SaveState(dir, st2); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the newly wired folder to become routable", func() bool {
		_, err := tbl.Resolve(second.Token)
		return err == nil
	})
	// and the folder that was already there keeps working across the swap
	if _, err := tbl.Resolve(first.Token); err != nil {
		t.Fatalf("the reload dropped an existing folder: %v", err)
	}
}

func TestWatchState_ATornDownFolderStopsResolving(t *testing.T) {
	dir := t.TempDir()
	st, _, _, _ := UpsertFolder(State{Port: 9765}, spec("/w/a", role.Test, "suta"))
	st, gone, _, _ := UpsertFolder(st, spec("/w/b", role.Product, "sutb"))
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	tbl, _ := TableFrom(st)

	stop := make(chan struct{})
	defer close(stop)
	ready := make(chan struct{}, 1)
	go WatchState(dir, tbl, 10*time.Millisecond, stop, ready, nil, nil)
	<-ready // the watcher's baseline stat is taken; a write from here on is guaranteed to be seen as a change

	st2, _, err := RemoveInstance(st, "/w/b", "sutb")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, st2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the torn-down folder to stop resolving", func() bool {
		_, err := tbl.Resolve(gone.Token)
		return err != nil
	})
}

// THE RULE THAT MATTERS MORE THAN THE FEATURE. A broken state file must never empty the table:
// swapping in nothing would strand every agent on the machine in response to a typo, while looking
// exactly like a router working correctly with nothing configured.
func TestWatchState_ACorruptStateKeepsThePreviousTable(t *testing.T) {
	dir := t.TempDir()
	st, live, _, _ := UpsertFolder(State{Port: 9765}, spec("/w/a", role.Test, "suta"))
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	tbl, _ := TableFrom(st)

	stop := make(chan struct{})
	defer close(stop)
	errs := make(chan error, 4)
	changes := make(chan int, 4)
	ready := make(chan struct{}, 1)
	go WatchState(dir, tbl, 10*time.Millisecond, stop, ready, func(n int) {
		select {
		case changes <- n:
		default:
		}
	}, func(err error) {
		select {
		case errs <- err:
		default:
		}
	})

	// WAIT FOR THE BASELINE STAT, DON'T RACE IT.
	//
	// WatchState takes its BASELINE stamp on its first line, INSIDE the goroutine. A corrupt write
	// that landed before that line ran used to become the baseline itself: `cur == last` on every tick
	// afterwards, no change was ever detected, and this test failed reporting "a corrupt state file was
	// never reported" — which is the OPPOSITE of what happened. Nothing was wrong with the watcher; the
	// test simply never gave it a change to see (measured 2026-08-21, round-8 release run: failed once in
	// a full-suite run, then once more in three isolated runs). `ready` (ticket AC-15) closes that window
	// instead of merely proving after the fact that it didn't open, so this test no longer needs a
	// timeout to notice it lost the race.
	<-ready

	// A valid change, ACKNOWLEDGED, proves the corrupt write below lands on a live watcher.
	st2, _, _, err := UpsertFolder(st, spec("/w/b", role.Test, "sutb"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveState(dir, st2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(4 * time.Second):
		t.Fatal("the watcher never observed a VALID change, so the corrupt-file case below would prove nothing")
	}

	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("a corrupt state must be reported")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("a corrupt state file was never reported")
	}
	// The point of the test: the previously loaded folder still routes.
	if _, err := tbl.Resolve(live.Token); err != nil {
		t.Fatalf("a corrupt state file emptied the routing table: %v", err)
	}
}

// VR-R4 across a reload, not merely across a restart. A state file that somehow records a product
// folder holding a cloud credential is refused, and refusing means KEEPING what is already serving.
func TestWatchState_ARefusedStateIsNotApplied(t *testing.T) {
	dir := t.TempDir()
	st, live, _, _ := UpsertFolder(State{Port: 9765}, spec("/w/a", role.Test, "suta"))
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	tbl, _ := TableFrom(st)

	stop := make(chan struct{})
	defer close(stop)
	errs := make(chan error, 4)
	ready := make(chan struct{}, 1)
	go WatchState(dir, tbl, 10*time.Millisecond, stop, ready, nil, func(err error) {
		select {
		case errs <- err:
		default:
		}
	})
	<-ready // the watcher's baseline stat is taken; a write from here on is guaranteed to be seen as a change

	// Hand-written: UpsertFolder would refuse to build this, which is the point — the file is the
	// attack surface, not the API.
	bad := State{Port: 9765, Folders: []Folder{{
		Path: `/w/p`, Hat: role.Product, Token: "odtr_forged",
		Upstreams: map[string]Upstream{"sutb": {URL: "http://x/sse", Token: "t"}},
		Cloud:     &CloudRef{URL: "https://cp/mcp", User: "u"},
	}}}
	if err := SaveState(dir, bad); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("a refused state must be reported")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("a state violating VR-R4 was applied silently")
	}
	if _, err := tbl.Resolve("odtr_forged"); err == nil {
		t.Fatal("a product folder holding a cloud credential became routable via a reload")
	}
	if _, err := tbl.Resolve(live.Token); err != nil {
		t.Fatalf("the refused reload dropped the good table: %v", err)
	}
}

func TestWatchState_StopsWhenAsked(t *testing.T) {
	dir := t.TempDir()
	st, _, _, _ := UpsertFolder(State{Port: 9765}, spec("/w/a", role.Test, "suta"))
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	tbl, _ := TableFrom(st)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { WatchState(dir, tbl, 5*time.Millisecond, stop, nil, nil, nil); close(done) }()
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchState ignored its stop channel")
	}
}

func TestTable_ReplaceIsAtomicFromAReadersView(t *testing.T) {
	st, a, _, _ := UpsertFolder(State{}, spec("/w/a", role.Test, "suta"))
	tbl, _ := TableFrom(st)

	st2, b, _, _ := UpsertFolder(State{}, spec("/w/b", role.Product, "sutb"))
	next, _ := TableFrom(st2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			// Every read must resolve to exactly one of the two tables, never panic and never see a
			// half-built map.
			_, errA := tbl.Resolve(a.Token)
			_, errB := tbl.Resolve(b.Token)
			if errA != nil && errB != nil {
				t.Errorf("a read saw neither the old table nor the new one")
				return
			}
		}
	}()
	tbl.Replace(next)
	<-done
	if _, err := tbl.Resolve(b.Token); err != nil {
		t.Fatalf("after Replace the new folder must resolve: %v", err)
	}
	if _, err := tbl.Resolve(a.Token); err == nil {
		t.Fatal("after Replace the old folder must be gone")
	}
}

// A rewrite with different bytes of the same size, straight after the previous write, usually lands
// inside one filesystem timestamp tick. Both watchers must still see it as a change.
func TestWatchStamps_ASameSizeRewriteIsAChange(t *testing.T) {
	keys := make([][]byte, 2)
	for i := range keys {
		d := t.TempDir()
		if _, err := LoadOrCreateIdentity(d); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(d, identityFile))
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = b
	}
	if len(keys[0]) != len(keys[1]) || string(keys[0]) == string(keys[1]) {
		t.Fatalf("fixture: want two different keys of one size, got %d and %d bytes", len(keys[0]), len(keys[1]))
	}

	dir := t.TempDir()
	const rewrites = 200
	unseen := func(file string, a, b []byte, stamp func() (any, error)) int {
		p := filepath.Join(dir, file)
		n := 0
		for i := 0; i < rewrites; i++ {
			if err := os.WriteFile(p, a, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := stamp()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
			after, err := stamp()
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				n++
			}
		}
		return n
	}

	if n := unseen(identityFile, keys[0], keys[1], func() (any, error) { return statIdentity(dir) }); n > 0 {
		t.Errorf("%d of %d same-size rewrites of %s were not seen as a change", n, rewrites, identityFile)
	}
	if n := unseen("state.json", []byte(`{"port":9765}`), []byte(`{"port":9766}`), func() (any, error) { return statState(dir) }); n > 0 {
		t.Errorf("%d of %d same-size rewrites of state.json were not seen as a change", n, rewrites)
	}
}

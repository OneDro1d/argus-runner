package router

import (
	"fmt"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/role"
)

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// THE ROUTING TABLE IS SHARED BETWEEN A SERVER AND A RELOADER, AND HAD NO LOCK DISCIPLINE.
//
// Found by running `go test -race` for the first time this round — it had been reported UNVERIFIED
// for want of a C compiler on the dev host, which is an absence, not a pass. Run in the Linux build
// image it took seconds:
//
//	LOCKS   byToken   Replace()      LOCKS   byToken   Len()
//	NO LOCK byToken   Resolve()      NO LOCK byToken   AddFolder()
//
// 🚨 WHY IT MATTERS RATHER THAN BEING A PURITY POINT. `Resolve` is the SERVING HOT PATH — proxy.go
// calls it for every request through the router. `Replace` fires on every state reload, which is what
// happens when onboarding wires a folder into a RUNNING router (the whole reason reload exists). So
// the two race in the ordinary flow, not an exotic one.
//
// In Go a concurrent map read during a map write is not "stale data": the runtime may
// `throw("concurrent map read and map write")`, which is UNRECOVERABLE — it takes down the
// machine-wide component every agent folder on that host routes through.
//
// ⚠ AND AddFolder IS A WRITER. Its duplicate-token check and its insert were both unlocked, so the
// check cannot do its job under concurrency: two callers can both observe "not present" and both
// insert. A token IS a folder's identity here, and the error text says a collision "makes every scope
// check unanswerable" — so the guard against that was itself racy.
//
// ⛔ PRE-EXISTING, not introduced by round 7: `git log d63b2ad..HEAD -- internal/router/table.go` is
// empty. Recorded because a defect's age is not a reason to leave it, and because the reason it went
// unseen for so long is that nobody had ever run the detector.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

func raceFolder(tok string) Folder {
	return Folder{Token: tok, Hat: role.Product, Path: "/f/" + tok}
}

// 🚩 Resolve (reader) against Replace (writer) — the pair the detector caught. Under `-race` this
// fails on the unfixed table; without `-race` it is a plain smoke test that neither call panics.
func TestTable_ResolveDoesNotRaceWithReplace(t *testing.T) {
	tbl := New()
	if err := tbl.AddFolder(raceFolder("tok-serving")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // the serving hot path
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = tbl.Resolve("tok-serving")
			}
		}
	}()

	for i := 0; i < 50; i++ { // the reloader
		next := New()
		if err := next.AddFolder(raceFolder("tok-serving")); err != nil {
			t.Fatalf("build replacement: %v", err)
		}
		tbl.Replace(next)
	}
	close(stop)
	wg.Wait()
}

// 🚩 AddFolder (writer) against Resolve (reader). Two writers would corrupt the map outright; a
// writer against the hot path is the shape onboarding produces.
func TestTable_AddFolderDoesNotRaceWithResolve(t *testing.T) {
	tbl := New()
	if err := tbl.AddFolder(raceFolder("tok-0")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = tbl.Resolve("tok-0")
			}
		}
	}()

	for i := 1; i <= 50; i++ {
		if err := tbl.AddFolder(raceFolder(fmt.Sprintf("tok-%d", i))); err != nil {
			t.Fatalf("AddFolder %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

// 🚩 THE DUPLICATE GUARD MUST HOLD UNDER CONCURRENCY, which is the half a race detector alone does
// not prove. A token is a folder's identity; two folders sharing one is the state AddFolder's own
// error message calls unanswerable.
func TestTable_ConcurrentAddFolderAdmitsExactlyOneOfADuplicatePair(t *testing.T) {
	for round := 0; round < 40; round++ {
		tbl := New()
		var wg sync.WaitGroup
		errs := make([]error, 8)
		wg.Add(len(errs))
		for i := range errs {
			go func(i int) {
				defer wg.Done()
				errs[i] = tbl.AddFolder(raceFolder("same-token"))
			}(i)
		}
		wg.Wait()

		accepted := 0
		for _, e := range errs {
			if e == nil {
				accepted++
			}
		}
		if accepted != 1 {
			t.Fatalf("round %d: %d of %d concurrent AddFolder calls accepted the SAME token, want exactly 1.\n"+
				"  The duplicate check and the insert must be one atomic step. Unlocked, every caller\n"+
				"  observes 'not present' before any of them inserts.", round, accepted, len(errs))
		}
		if tbl.Len() != 1 {
			t.Fatalf("round %d: table holds %d folders after a duplicate storm, want 1", round, tbl.Len())
		}
	}
}

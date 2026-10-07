package router

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ReloadInterval is how often the router looks for a rewritten state file.
//
// Onboarding is a human-timescale operation, so seconds are ample and cheap: the poll reads ONE file
// and does nothing else unless it changed. A file watcher would be tighter and would also add a
// platform-specific dependency to the one component whose failure strands every agent on the machine.
const ReloadInterval = 2 * time.Second

// stateStamp is the "did it change?" fingerprint: a SHA-256 of state.json's bytes. A rewrite that
// changes the content is a change whatever its size and however soon after the previous write it
// lands; a modification time only advances in filesystem timestamp ticks.
type stateStamp [sha256.Size]byte

func statState(dir string) (stateStamp, error) {
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return stateStamp{}, err
	}
	return sha256.Sum256(b), nil
}

// WatchState reloads the routing table whenever the state file changes, so a folder wired by
// onboarding becomes routable WITHOUT restarting the router.
//
// Why this exists, measured rather than imagined: with the table read once at startup, a folder
// wired against a running router failed `initialize` with `-32001 unrecognized router token`. That
// message points at the token — the one thing that was correct — so the operator's next hour goes
// into the credential rather than into the process that never re-read its config. During the cutover,
// where seven instances are re-onboarded in sequence against a router that must stay up, this would
// have fired six times.
//
// TWO RULES GOVERN THE FAILURE PATH, and both are the same rule:
//
//   - a state file that cannot be read or parsed leaves the CURRENT table in place. Swapping in an
//     empty table would strand every agent on the machine in response to a typo, and it would look
//     like the router working correctly with nothing configured.
//   - a state file whose folders TableFrom refuses (a product folder holding a cloud credential, say)
//     is likewise not applied. VR-R4 holds across a reload for the same reason it holds across a
//     restart: the check belongs to the data, not to the moment it was first read.
//
// ready, ondChange and onErr are all optional. ready — if non-nil — is signaled (non-blocking) once the
// baseline stamp below has been taken, so a caller that is about to mutate the state file has a deterministic
// event to wait on instead of a fixed sleep: a write landing before this goroutine got scheduled would
// otherwise become the baseline itself, and the change it made would never be seen as a change (ticket AC-15
// — this raced TestWatchState_ATornDownFolderStopsResolving against `go WatchState(...)`'s own startup).
//
// onChange and onErr are both optional. onErr is called on every failed attempt — a reload that keeps
// failing must keep saying so, because "the last thing I loaded is still serving" is only reassuring
// while someone knows it is happening.
func WatchState(dir string, tbl *Table, interval time.Duration, stop <-chan struct{}, ready chan<- struct{}, onChange func(int), onErr func(error)) {
	if interval <= 0 {
		interval = ReloadInterval
	}
	last, _ := statState(dir) // a missing file is not an error: nothing has been wired on this machine yet

	if ready != nil {
		select {
		case ready <- struct{}{}:
		default:
		}
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			cur, err := statState(dir)
			if err != nil {
				// The file disappeared. Keep serving what is already loaded — deleting the state file
				// is not a request to disconnect every agent, and if it IS, restarting the router says
				// so unambiguously.
				continue
			}
			if cur == last {
				continue
			}
			st, err := LoadState(dir)
			if err != nil {
				if onErr != nil {
					onErr(fmt.Errorf("router: state changed but could not be loaded, KEEPING the previous routing table: %w", err))
				}
				last = cur // do not re-report the same broken file every tick
				continue
			}
			next, err := TableFrom(st)
			if err != nil {
				if onErr != nil {
					onErr(fmt.Errorf("router: state changed but was REFUSED, keeping the previous routing table: %w", err))
				}
				last = cur
				continue
			}
			tbl.Replace(next)
			last = cur
			if onChange != nil {
				onChange(tbl.Len())
			}
		}
	}
}

// identityStamp fingerprints identity.key the way stateStamp fingerprints state.json.
//
// ⛔ IT IS SEPARATE ON PURPOSE. Folding the key into stateStamp would make an identity change trigger a
// full routing-table reload, and a folder wire re-read the identity — each doing the other's work for
// no reason, on a file that changes for entirely different causes.
type identityStamp [sha256.Size]byte

func statIdentity(dir string) (identityStamp, error) {
	b, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return identityStamp{}, err
	}
	return sha256.Sum256(b), nil
}

// WatchIdentity re-reads identity.key whenever it changes and hands the new value to onChange.
//
// ── WHY THIS EXISTS (VR9-I1) ──────────────────────────────────────────────────────────────────────
//
// `router register` mints a new identity under a RUNNING server. Before this, the serving process kept
// signing with the key it read at start-up, so every beat 401'd — silently, because a beat that does
// not land is deliberately non-fatal — and no token rotation could reach the machine until it was
// restarted. Restarting also destroys the in-memory routing table, so "just restart it" is not a fix:
// on this estate many containers are `restart: no` and never come back.
//
// ⚠ THE CALLER MUST PUBLISH THE VALUE SOMEWHERE THE BEAT GOROUTINE CAN READ IT SAFELY. This watcher
// runs in its own goroutine; handing the Identity straight into a variable the heartbeat loop reads is
// a DATA RACE, and an invisible one. See cmd/argus/main.go, which stores it in an atomic.Pointer.
//
// A read that fails is reported and otherwise ignored: a half-written key during a re-register is a
// transient, and the next tick picks up the finished file.
func WatchIdentity(dir string, interval time.Duration, stop <-chan struct{}, onChange func(Identity), onErr func(error)) {
	if interval <= 0 {
		interval = ReloadInterval
	}
	last, _ := statIdentity(dir) // an absent key is a legitimate start state; the first write is a change
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			cur, err := statIdentity(dir)
			if err != nil || cur == last {
				continue
			}
			id, lerr := LoadOrCreateIdentity(dir)
			if lerr != nil {
				if onErr != nil {
					onErr(lerr)
				}
				continue // do NOT advance the stamp: retry the same change on the next tick
			}
			last = cur
			if onChange != nil {
				onChange(id)
			}
		}
	}
}

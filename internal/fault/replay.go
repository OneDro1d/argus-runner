package fault

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// ReplayConfig wires Replay. Journal is the directory the previous executor's Manager wrote to.
type ReplayConfig struct {
	Journal *Journal
	Owner   string // this executor's identity (Hostname)
	Kube    func() (Kube, error)
	Toxi    func(api string) Toxi
	Logf    func(format string, args ...any)
	Now     func() time.Time
	// StaleAfter is how long an entry must have gone untouched before another owner's is presumed dead
	// (default 60s). MaxWait bounds how long startup waits for a live foreign run to finish and clear its
	// own entries (default 2m); WaitStep is the poll between looks (default 5s).
	StaleAfter, MaxWait, WaitStep time.Duration
}

// ReplayResult is what happened to one pending entry.
type ReplayResult struct {
	ID, Run, Op, Target, Outcome string
	Detail                       string // a sanitised cause on failure; never an address
}

// Replay undoes every fault a dead executor left in the journal, newest first, BEFORE this executor takes
// new work. It is what makes "the executor died mid-run" survivable: the undo was written before the fault
// was applied (Manager), so there is always something to replay.
//
// WHOSE ENTRIES. The results volume is shared by every replica, so an entry may belong to a run that is
// alive right now on another pod. An entry is replayed when its owner is THIS executor (a restart: that
// process is dead by construction) or when nobody has touched it for StaleAfter (its owner stopped
// beating). A fresh foreign entry is waited for up to MaxWait - the live run's own Finish removes it - and
// otherwise left alone.
//
// A failed undo stays journaled (the next start tries again) and is reported loudly: the log line and
// replay.log both say so. The outcome of every entry is appended to <journal>/replay.log.
func Replay(ctx context.Context, cfg ReplayConfig) []ReplayResult {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 60 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 2 * time.Minute
	}
	if cfg.WaitStep <= 0 {
		cfg.WaitStep = 5 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var results []ReplayResult
	done := map[string]bool{}
	deadline := cfg.Now().Add(cfg.MaxWait)
	for {
		pend, err := cfg.Journal.Pending()
		if err != nil {
			logf("FAULT UNDO JOURNAL PROBLEM (an operator must look): %v", err)
		}
		waiting := 0
		for i := len(pend) - 1; i >= 0; i-- { // newest first
			u := pend[i]
			if done[u.ID] {
				continue
			}
			if u.Owner != cfg.Owner && cfg.Journal.Age(u, cfg.Now()) < cfg.StaleAfter {
				waiting++
				continue
			}
			done[u.ID] = true
			outcome, uerr := undoEntryWithRetries(ctx, u, cfg)
			r := ReplayResult{ID: u.ID, Run: u.Run, Op: u.Op, Target: u.Target, Outcome: outcome}
			if uerr != nil {
				r.Detail = uerr.Error()
				logf("FAULT NOT RESTORED after executor restart: run %s, %s of %s could not be undone (%s) - the system under test may still be broken; the undo stays journaled and is retried at the next start", u.Run, u.Op, u.Target, r.Detail)
			} else {
				_ = cfg.Journal.Remove(u)
				cfg.Journal.Unlock(lockKey(u))
				logf("fault undone after executor restart: run %s, %s of %s -> %s", u.Run, u.Op, u.Target, outcome)
			}
			appendReplayLog(cfg.Journal, r, cfg.Now())
			results = append(results, r)
		}
		if waiting == 0 || !cfg.Now().Before(deadline) || ctx.Err() != nil {
			if waiting > 0 {
				logf("fault replay: %d entr(y/ies) belong to a run that is still alive on another executor; left for its owner", waiting)
			}
			return results
		}
		select {
		case <-ctx.Done():
			return results
		case <-time.After(cfg.WaitStep):
		}
	}
}

// lockKey is the target lock an entry's fault held (the key Manager.lock took).
func lockKey(u Undo) string {
	if u.Kind == KindScale {
		return "deployment-" + u.Namespace + "-" + u.Deployment
	}
	return "proxy-" + u.Proxy
}

func undoEntryWithRetries(ctx context.Context, u Undo, cfg ReplayConfig) (string, error) {
	var last error
	for try := 0; try < undoTries; try++ {
		if try > 0 {
			time.Sleep(RestoreBackoff * time.Duration(try))
		}
		uctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		outcome, err := undoEntry(uctx, u, cfg.Kube, cfg.Toxi)
		cancel()
		if err == nil {
			return outcome, nil
		}
		last = err
	}
	return report.FaultRestoreFailed, last
}

func appendReplayLog(j *Journal, r ReplayResult, now time.Time) {
	b, err := json.Marshal(struct {
		At string `json:"at"`
		ReplayResult
	}{now.UTC().Format(time.RFC3339), r})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(j.Dir, "replay.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n", b)
}

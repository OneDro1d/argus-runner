package runner

// direct.go — the DIRECT local run scenario SOURCE: the owner-locked HYBRID (D-FED.3, UC059/060/061).
// Before a local run executes, we ask the CP for a FRESH materialization; if the CP is slow/unreachable
// we fall back to the LAST materialized set, stamped "cached (CP unreachable)"; with no cache AND no CP
// we refuse with a structured error. Either way the run reports up on its own run_id (UC062).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

// SetSource is the resolved scenario set for a direct run, plus its provenance.
type SetSource struct {
	Scope     string
	SetHash   string
	Scenarios []federation.ScenarioPayload
	Cached    bool   // true = served from the local cache (the CP was unreachable)
	Stamp     string // "" when fresh; "cached (CP unreachable)" when Cached (UC060)
}

// ErrNoSetAvailable is the UC061 structured refusal: no cache AND no reachable control plane.
var ErrNoSetAvailable = errors.New("no scenario set available — connect to the control plane once or use author__request_run")

type cacheFile struct {
	Scope     string                       `json:"scope"`
	SetHash   string                       `json:"set_hash"`
	Scenarios []federation.ScenarioPayload `json:"scenarios"`
}

// ResolveDirectSet implements the hybrid source. It tries a fresh CP fetch (bounded by fetchTimeout);
// on success it persists the set to cachePath and returns it fresh. On any fetch failure it returns the
// cached set stamped "cached (CP unreachable)". With neither, it returns ErrNoSetAvailable.
func ResolveDirectSet(ctx context.Context, client *Client, cachePath, scope string, sel federation.Selection, fetchTimeout time.Duration) (*SetSource, error) {
	if scope == "" {
		scope = "full"
	}
	if client != nil {
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		mr, err := client.FetchSet(fctx, scope, sel)
		cancel()
		if err == nil {
			src := &SetSource{Scope: mr.Scope, SetHash: mr.SetHash, Scenarios: mr.Scenarios}
			_ = writeCache(cachePath, src) // best-effort persist for the next CP-down run
			return src, nil
		}
	}
	if cached, err := readCache(cachePath); err == nil && cached != nil {
		cached.Cached = true
		cached.Stamp = "cached (CP unreachable)"
		return cached, nil
	}
	return nil, ErrNoSetAvailable
}

// DirectRun resolves the scenario source (the hybrid) and runs it locally, then reports up on its own
// run_id (UC062). It returns the push, the source provenance (for the in-env stamp), and any error —
// ErrNoSetAvailable for the no-cache/no-CP refusal (UC061), ErrCPBusy when the W1 fence says a run
// (either path) is already in flight for this instance (§D-3.1.2/UC188 — the caller surfaces *busy*).
func (e *Executor) DirectRun(ctx context.Context, cachePath, scope string, sel federation.Selection, fetchTimeout time.Duration) (federation.ResultsPush, *SetSource, error) {
	src, err := ResolveDirectSet(ctx, e.Client, cachePath, scope, sel, fetchTimeout)
	if err != nil {
		return federation.ResultsPush{}, nil, err
	}
	// The W1 CP fence, FRESH path only: the CP answered the materialize, so it must also grant the
	// instance_run_lock BEFORE any SUT effect. ErrCPBusy → refuse (surface *busy*); a transport
	// failure (CP dropped between materialize and begin) → proceed as the offline residual (the
	// documented bounded window; the push reconciles on reconnect). CACHED source = the CP was
	// already unreachable → no CP lock to take (same residual).
	fenced := false
	preRunID := ""
	if e.Client != nil && !src.Cached {
		preRunID = argus.NewRunID()
		bctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		berr := e.Client.BeginRun(bctx, preRunID, src.Scope)
		cancel()
		switch {
		case berr == nil:
			fenced = true
		case errors.Is(berr, ErrCPBusy):
			return federation.ResultsPush{}, src, ErrCPBusy
		default:
			e.log("run begin unreachable (proceeding offline; the local lock is the second line): %v", berr)
		}
	}
	// a direct run has no run_request_id — it is triggered locally, not enqueued (UC062).
	a := &federation.RunAssignment{Scope: src.Scope, Selection: sel, SetHash: src.SetHash, Scenarios: src.Scenarios, RunID: preRunID}
	var stopHB func()
	if fenced {
		stopHB = e.startHeartbeat(ctx, preRunID, "", src.Scope) // W1/3: keep the fenced run alive under the watchdog
	}
	push, err := e.Run(ctx, a)
	if stopHB != nil {
		stopHB()
	}
	if err != nil {
		// The run never produced a push — release the held fence with a minimal terminal 'failed'
		// push (records the aborted run honestly AND frees the lock; §D-3.1.2). DETACHED context:
		// the run error may BE a ctx cancellation (Ctrl-C / deadline) — the abort must still land
		// (gate follow-up, CP-M3-117).
		if fenced && e.Client != nil {
			actx, acancel := context.WithTimeout(context.Background(), 10*time.Second)
			now := time.Now().UTC()
			// 019: the DIRECT path carries its reason too. A direct run that aborts writes the same
			// "status=failed, tallies 0/0" row a federated one does, and it is read from the same
			// Runs page — so leaving this one silent would fix half the estate and quietly keep the
			// other half undiagnosable.
			abort := federation.ResultsPush{RunID: preRunID, Scope: src.Scope, Status: "failed",
				SetHash: src.SetHash, StartedAt: &now, FinishedAt: &now, FailureReason: err.Error()}
			if oerr := e.Outbox.Put(abort); oerr != nil {
				e.log("WARNING: could not queue the abort push durably: %v", oerr)
			}
			if perr := e.Client.Push(actx, abort); perr != nil {
				e.log("abort push failed — queued; the watchdog reaps the stale direct run meanwhile: %v", perr)
			} else {
				_ = e.Outbox.Ack(abort.RunID)
			}
			acancel()
		}
		return federation.ResultsPush{}, src, err
	}
	// ALL runs report up. This used to be a SINGLE attempt whose failure was logged with the claim
	// "will land on the next reachable CP" — and nothing ever landed it (F1, UC060). The results
	// really were local files, but no code path ever carried them to the ledger, so a run that
	// produced 30 passed / 3 failed stayed in the ledger as failed 0/0/0/0 until the watchdog
	// reaped it. Now the claim is true: queued first, delivered by the executor's next reachable
	// poll — which is why it holds even for a `run-direct` process that exits immediately after.
	if e.Client != nil {
		if oerr := e.Outbox.Put(push); oerr != nil {
			e.log("WARNING: could not queue results for run %s durably (%v) — this push is best-effort only", push.RunID, oerr)
		}
		if perr := e.Client.Push(ctx, push); perr != nil {
			if e.Outbox.enabled() {
				e.log("direct run push failed — results for run %s are QUEUED and land on the next poll that reaches the control plane: %v", push.RunID, perr)
			} else {
				e.log("direct run push failed and NO durable outbox is configured — these results will not reach the ledger: %v", perr)
			}
		} else {
			_ = e.Outbox.Ack(push.RunID)
		}
	}
	return push, src, nil
}

func writeCache(path string, src *SetSource) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(cacheFile{Scope: src.Scope, SetHash: src.SetHash, Scenarios: src.Scenarios})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readCache(path string) (*SetSource, error) {
	if path == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf cacheFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, err
	}
	return &SetSource{Scope: cf.Scope, SetHash: cf.SetHash, Scenarios: cf.Scenarios}, nil
}

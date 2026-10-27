package runner

// inflight.go — the in-flight run marker.
//
// THE DEFECT, measured 2026-10-19 on Kubernetes: an executor container was ended by the kernel (an out-of-memory
// kill of the `argus` process) in the middle of a run. The heartbeats stopped, the control plane's watchdog marked
// the run `abandoned` about three minutes later, and the restarted container polled and took the next run, so
// nothing anywhere said what had happened. The check concluded "no verdict" with no reason.
//
// THE FIX: a small file per run, written when the executor picks a run up and removed when that run's results are
// safe (queued durably in the outbox, or delivered). The results volume survives a container restart inside the
// same pod, so a process that starts and finds a marker of ITS OWN pod knows the previous process stopped during
// that run, and says so to the control plane with the existing results push: status `failed`, tallies 0/0, and a
// plain failure reason. The control plane accepts a `failed` push over an `abandoned` row (RecordResults), so the
// run reads "ended, with this reason" instead of "abandoned".
//
// WHOSE MARKER IS WHOSE. The results volume is shared by every replica of the executor Deployment, so a marker
// carries its owner, the pod name (os.Hostname, which a container restart keeps and a new pod does not). A process
// reports only the markers of its own pod; another replica's marker is a run in progress on that replica. The
// marker of a pod that was replaced (a rollout, a node loss) is left alone: nothing here can tell it from a
// replica that is still running, and the control plane's watchdog is still the answer for it.
//
// WHAT IT DOES NOT COVER: the few milliseconds between the control plane handing the run over and the marker
// reaching disk; a volume that is not persistent (compose without a results volume, an emptyDir), where the marker
// goes with the container.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// errExecutorStopped is the reason a run carries when its executor process stopped in the middle of it. Argus's own
// words (the rule of ownwords.go), so it reaches the control plane as written.
var errExecutorStopped = ownWordsf("the executor process stopped during this run (it was restarted; on Kubernetes an out-of-memory kill is the usual cause), so this run has no results")

// endInFlight removes a run's marker, saying so in the log if it cannot.
func (e *Executor) endInFlight(runID string) {
	if err := e.InFlight.End(runID); err != nil {
		e.log("WARNING: could not remove the in-flight marker of run %s (%v) — the next start will report that run as stopped", runID, err)
	}
}

// reportInterruptedRuns is called once by Loop before the first poll. For each marker a previous process of this
// pod left, it pushes the run to the control plane as ended with errExecutorStopped (status `failed`, the push a
// run that could not produce results already makes) through pushWithRetry, so the push is queued durably first
// and its marker removed once it is. A run whose results ARE already queued in the outbox finished; only its
// marker was left, and it is dropped without a push (the outbox delivers the real results).
func (e *Executor) reportInterruptedRuns(ctx context.Context) {
	for _, m := range e.InFlight.Interrupted() {
		if e.Outbox.has(m.RunID) {
			e.log("in-flight marker of run %s found, but its results are already queued: dropping the marker", m.RunID)
			e.endInFlight(m.RunID)
			continue
		}
		e.log("run %s was in progress when the previous executor process stopped (it started %s): reporting it to the control plane as ended",
			m.RunID, m.StartedAt.Format(time.RFC3339))
		now := time.Now().UTC()
		started := m.StartedAt
		e.pushWithRetry(ctx, federation.ResultsPush{
			RunID: m.RunID, RunRequestID: m.RunRequestID, Scope: m.Scope, SetHash: m.SetHash,
			Status: "failed", StartedAt: &started, FinishedAt: &now,
			FailureReason: failureReason(errExecutorStopped),
		})
		if ctx.Err() != nil {
			return
		}
	}
}

// inflightMarker is what lands on disk.
type inflightMarker struct {
	RunID        string    `json:"run_id"`
	RunRequestID string    `json:"run_request_id,omitempty"`
	Scope        string    `json:"scope"`
	SetHash      string    `json:"set_hash,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	Owner        string    `json:"owner"` // the pod name (os.Hostname) of the process that started the run
	PID          int       `json:"pid"`
}

// InFlight is the marker store: one file per run under Dir. The zero value (Dir == "") does nothing.
type InFlight struct {
	Dir   string
	Owner string // this process's pod name; "" = os.Hostname()
}

// InFlightDir is where an instance's markers live: beside the outbox, on the same volume.
func InFlightDir(resultsRoot, instance string) string {
	if resultsRoot == "" {
		return ""
	}
	return filepath.Join(resultsRoot, instance, "inflight")
}

func (f *InFlight) enabled() bool { return f != nil && f.Dir != "" }

func (f *InFlight) owner() string {
	if f.Owner != "" {
		return f.Owner
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "executor"
	}
	return h
}

func (f *InFlight) path(runID string) string { return filepath.Join(f.Dir, safeName(runID)+".json") }

// Begin writes the marker for a run this process has picked up. Atomic and synced, like the outbox entry.
func (f *InFlight) Begin(runID, runRequestID, scope, setHash string, startedAt time.Time) error {
	if !f.enabled() {
		return nil
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return fmt.Errorf("in-flight marker mkdir: %w", err)
	}
	b, err := json.Marshal(inflightMarker{RunID: runID, RunRequestID: runRequestID, Scope: scope, SetHash: setHash,
		StartedAt: startedAt.UTC(), Owner: f.owner(), PID: os.Getpid()})
	if err != nil {
		return err
	}
	tmp := filepath.Join(f.Dir, ".tmp-"+safeName(runID)+".json")
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err = fh.Write(b); err == nil {
		err = fh.Sync()
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, f.path(runID))
}

// End removes a run's marker. Absent is success.
func (f *InFlight) End(runID string) error {
	if !f.enabled() {
		return nil
	}
	if err := os.Remove(f.path(runID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Interrupted returns the markers left by a previous process of THIS pod, oldest first. A marker that does not
// parse, or that belongs to another pod, is not returned (and is not touched).
func (f *InFlight) Interrupted() []inflightMarker {
	if !f.enabled() {
		return nil
	}
	des, err := os.ReadDir(f.Dir)
	if err != nil {
		return nil
	}
	var out []inflightMarker
	for _, de := range des {
		n := de.Name()
		if de.IsDir() || strings.HasPrefix(n, ".tmp-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(f.Dir, n))
		if rerr != nil {
			continue
		}
		var m inflightMarker
		if json.Unmarshal(b, &m) != nil || m.RunID == "" || m.Owner != f.owner() {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

package runner

// outbox.go — the durable results outbox (F1, UC065/UC060). THE Stage III gate-blocker.
//
// THE DEFECT, measured 2026-08-06
// A run that really produced 30 passed / 3 failed sat in the cloud ledger permanently as `failed`
// with 0/0/0/0, because the control plane was unreachable while the run finished. Two paths, one
// hole:
//
//   * cloud-requested runs retried the terminal push 6 times at a flat 3s gap (~15s of wall clock)
//     and then gave up — executor.go:223-236;
//   * direct runs never retried at all — direct.go:122-126, a single Push whose error was logged.
//
// Both carried a comment promising a reconnect that neither implemented: "will re-push on a later
// run" and "the push reconciles on reconnect". Nothing re-pushed. A LATER run completed normally and
// did not carry the old results with it; what finally resolved the row was the CP watchdog reaping it
// as lost. The operator was never told the results were sitting on local disk.
//
// The results were never actually lost — they are files in the results volume — but nothing ever
// carried them to the ledger, and the ledger is what everyone reads.
//
// THE FIX, layer 1 of two
// Persist the terminal payload to <results>/outbox/<run_id>.json BEFORE the first network call, and
// delete it only when the control plane has ACKNOWLEDGED it (Client.Push returns nil only on HTTP
// 200). Every poll tick that reaches the CP drains whatever is pending. So the retry budget stops
// being a deadline: a push that fails is not "given up on", it is queued, and the next reachable poll
// delivers it — minutes or days later, across executor restarts, because the queue is on the same
// volume as the results themselves.
//
// Layer 2 (the honest terminal states — results_pending / abandoned) is separate: this layer makes
// the results ARRIVE, that one stops the ledger from lying while they are in flight.
//
// WHAT IS DELIBERATELY NOT IN THE OUTBOX
// Heartbeats (status="running"). They are worthless once late — a heartbeat's only job is to be
// recent — and replaying stale ones would fight the watchdog. Only TERMINAL pushes are durable.

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

// maxDrainPerTick bounds the work one poll tick will do. A backlog drains over several ticks rather
// than blocking the loop (and the SUT's next run) behind a hundred sequential HTTP calls.
const maxDrainPerTick = 20

// Outbox is a crash-safe queue of terminal results pushes awaiting control-plane acknowledgement.
// The zero value (Dir == "") is a no-op, so every call site is safe before the wiring exists.
type Outbox struct {
	Dir string // <results>/outbox
	Log func(format string, args ...any)
}

// entry is what lands on disk. The push is the payload; the rest is operator-facing so a stuck
// queue can be diagnosed from the file alone, without the executor's logs.
type entry struct {
	Push        federation.ResultsPush `json:"push"`
	QueuedAt    time.Time              `json:"queued_at"`
	Attempts    int                    `json:"attempts"`
	LastAttempt *time.Time             `json:"last_attempt,omitempty"`
	LastError   string                 `json:"last_error,omitempty"`
}

func (o *Outbox) enabled() bool { return o != nil && o.Dir != "" }

func (o *Outbox) log(f string, a ...any) {
	if o != nil && o.Log != nil {
		o.Log(f, a...)
	}
}

// safeName keeps a run id from escaping the outbox directory or colliding with the temp prefix.
// Run ids are minted as timestamps (20260806T130924607), so in practice this changes nothing — but
// the id reaches here from a CP-supplied assignment, and a path separator in it would otherwise
// write outside Dir.
func safeName(runID string) string {
	if runID == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (o *Outbox) path(runID string) string { return filepath.Join(o.Dir, safeName(runID)+".json") }

// Put persists a terminal push. It MUST be called before the first network attempt — that ordering is
// the whole guarantee, and it is what the unit test kills the process between.
//
// Atomic by write-temp-then-rename (the same idiom writeCache uses for the set cache): a crash mid-
// write leaves a .tmp file that Drain ignores, never a half-parsed entry that would be dropped as
// corrupt. fsync before the rename so the bytes — not just the directory entry — survive a power cut.
func (o *Outbox) Put(push federation.ResultsPush) error {
	if !o.enabled() {
		return nil
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return fmt.Errorf("outbox mkdir: %w", err)
	}
	e := entry{Push: push, QueuedAt: time.Now().UTC()}
	// Preserve the attempt history if this run is already queued (a re-push of the same run id).
	if prev, err := o.read(o.path(push.RunID)); err == nil && prev != nil {
		e.Attempts, e.LastError, e.LastAttempt = prev.Attempts, prev.LastError, prev.LastAttempt
	}
	return o.write(o.path(push.RunID), e)
}

func (o *Outbox) write(path string, e entry) error {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = f.Sync(); err != nil { // durability, not just visibility
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func (o *Outbox) read(path string) (*entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Ack removes an entry the control plane has confirmed. Absent is success — Ack is idempotent.
func (o *Outbox) Ack(runID string) error {
	if !o.enabled() {
		return nil
	}
	if err := os.Remove(o.path(runID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// list returns queued entry paths, OLDEST FIRST. Run ids are timestamps, so lexical order is
// chronological; results therefore reach the ledger in the order they were produced. .tmp- files are
// skipped: they are torn writes, and their real entry either landed or never existed.
func (o *Outbox) list() ([]string, error) {
	des, err := os.ReadDir(o.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, de := range des {
		n := de.Name()
		if de.IsDir() || strings.HasPrefix(n, ".tmp-") || !strings.HasSuffix(n, ".json") {
			continue
		}
		out = append(out, filepath.Join(o.Dir, n))
	}
	sort.Strings(out)
	return out, nil
}

// Pending is how many terminal pushes have been produced but not yet acknowledged. Surfaced so the
// executor can SAY it is holding results instead of leaving the operator to infer it (the original
// complaint: nobody was told the results were on local disk).
func (o *Outbox) Pending() int {
	if !o.enabled() {
		return 0
	}
	l, _ := o.list()
	return len(l)
}

// heldReportCap bounds what one poll announces. An executor that has been offline for days could
// hold hundreds of runs, and a poll body is not the place to enumerate them all — the CP marks what
// it is told and the rest arrive on later polls, since the queue drains oldest-first anyway.
const heldReportCap = 50

// Held is WHAT the outbox is holding — the finished-but-undelivered runs, oldest first (VR-F28).
//
// Pending() answers "how many", which was enough for a log line and not enough for the ledger: the
// control plane cannot mark runs it cannot name. This returns the index-level facts each one needs —
// id, scope, tallies, how long it has been queued, how many deliveries have already failed — and no
// per-scenario evidence (VR-C8).
//
// Unreadable entries are SKIPPED rather than guessed at. A file we cannot parse still counts in
// Pending(), so "held 3, reported 2" remains visible; inventing a run id for it would put a
// fabricated row in the ledger, which is worse than an unexplained gap.
func (o *Outbox) Held() []federation.PendingRun {
	if !o.enabled() {
		return nil
	}
	paths, err := o.list()
	if err != nil || len(paths) == 0 {
		return nil
	}
	if len(paths) > heldReportCap {
		o.log("outbox: holding %d runs; reporting the oldest %d on this poll (the rest follow as the queue drains)",
			len(paths), heldReportCap)
		paths = paths[:heldReportCap]
	}
	out := make([]federation.PendingRun, 0, len(paths))
	for _, p := range paths {
		e, rerr := o.read(p)
		if rerr != nil {
			continue // already reported every sweep by Drain; not a run we can name
		}
		out = append(out, federation.PendingRun{
			RunID:    e.Push.RunID,
			Scope:    e.Push.Scope,
			Tallies:  e.Push.Tallies,
			QueuedAt: e.QueuedAt,
			Attempts: e.Attempts,
		})
	}
	return out
}

// Drain re-pushes every queued entry, oldest first, and removes each one the CP acknowledges.
// Returns (delivered, remaining).
//
// A failing entry is NEVER discarded — that is the entire point — but it also must not block the
// ones behind it, so a failure moves on to the next entry rather than aborting the sweep. Attempts
// are recorded on the entry so a permanently-rejected payload (a malformed push the CP will refuse
// forever) is visible as a growing count in the file rather than as silence.
func (o *Outbox) Drain(ctx context.Context, push func(context.Context, federation.ResultsPush) error) (delivered, remaining int) {
	if !o.enabled() {
		return 0, 0
	}
	paths, err := o.list()
	if err != nil {
		o.log("outbox: cannot read %s: %v", o.Dir, err)
		return 0, 0
	}
	if len(paths) == 0 {
		return 0, 0
	}
	budget := len(paths)
	if budget > maxDrainPerTick {
		budget = maxDrainPerTick
	}
	for _, p := range paths[:budget] {
		if ctx.Err() != nil {
			break
		}
		e, rerr := o.read(p)
		if rerr != nil {
			// Unreadable after a complete write is not something a retry fixes. Leave it in place and
			// say so every sweep: a file we cannot parse is still evidence, and deleting it would be
			// the very data loss this file exists to prevent.
			o.log("outbox: %s is unreadable (%v) — left in place for inspection", filepath.Base(p), rerr)
			continue
		}
		if perr := push(ctx, e.Push); perr != nil {
			now := time.Now().UTC()
			e.Attempts++
			e.LastAttempt = &now
			e.LastError = perr.Error()
			_ = o.write(p, *e)
			// Loud on the first failure and then at widening intervals: a CP outage should not fill
			// the log with one line per run per tick, but a queue that never drains must stay visible.
			if e.Attempts <= 3 || e.Attempts%20 == 0 {
				o.log("outbox: run %s still undelivered after %d attempt(s): %v", e.Push.RunID, e.Attempts, perr)
			}
			continue
		}
		if aerr := o.Ack(e.Push.RunID); aerr != nil {
			o.log("outbox: run %s was accepted but its queue entry could not be removed (%v) — a duplicate push may follow, which the ledger upsert absorbs", e.Push.RunID, aerr)
		}
		delivered++
		o.log("outbox: delivered run %s to the control plane (%d/%d passed, queued %s ago, %d earlier attempt(s))",
			e.Push.RunID, e.Push.Tallies.Passed, e.Push.Tallies.Total,
			time.Since(e.QueuedAt).Round(time.Second), e.Attempts)
	}
	return delivered, o.Pending()
}

// OutboxDir is where an instance's durable queue lives: alongside its results, on the same volume,
// so the queue survives exactly what the results survive — a container restart, a host reboot, a
// `docker compose up` that replaces the executor. Instance-scoped to match toolcore's own layout
// (<results>/<instance>/report.json).
func OutboxDir(resultsRoot, instance string) string {
	if resultsRoot == "" {
		return ""
	}
	return filepath.Join(resultsRoot, instance, "outbox")
}

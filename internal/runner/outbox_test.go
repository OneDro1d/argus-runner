package runner

// The Stage III gate-blocker (F1, UC065/UC060): results produced while the control plane is
// unreachable must still reach the ledger.
//
// The measured defect: a run that really produced 30 passed / 3 failed was filed permanently as
// `failed` 0/0/0/0 because the CP was down when it finished. Cloud runs retried 6 times over ~15s
// and gave up; direct runs never retried. Both logged a promise to re-push that no code kept.
//
// The load-bearing test is TestOutbox_SurvivesProcessDeathBetweenWriteAndPush: it is the plan's own
// evidence gate, and it is what the ORDERING in pushWithRetry exists for. If Put ever moves after
// Push, that test and TestPushWithRetry_QueuesBeforeTheNetworkCall both fail.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

func samplePush(runID string, passed, total int) federation.ResultsPush {
	return federation.ResultsPush{
		RunID: runID, Scope: "full", Status: "completed",
		Tallies: federation.Tallies{Passed: passed, Total: total, Failed: total - passed},
	}
}

func newOutbox(t *testing.T) *Outbox {
	t.Helper()
	return &Outbox{Dir: filepath.Join(t.TempDir(), "outbox")}
}

// ── THE GATE ─────────────────────────────────────────────────────────────────────────────────────
// "kill the process between write and push — the entry survives". A process death is modelled by
// simply never pushing and then building a COMPLETELY NEW Outbox over the same directory: no shared
// memory, nothing carried over but the disk. That is the whole claim.
func TestOutbox_SurvivesProcessDeathBetweenWriteAndPush(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")

	// ── process 1: queues the results, then dies ──
	if err := (&Outbox{Dir: dir}).Put(samplePush("20260806T130924607", 30, 33)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// ── process 2: a fresh executor over the same volume, CP now reachable ──
	revived := &Outbox{Dir: dir}
	if got := revived.Pending(); got != 1 {
		t.Fatalf("a restarted executor sees %d queued runs, want 1 — the results did not survive", got)
	}
	var delivered []federation.ResultsPush
	sent, remaining := revived.Drain(context.Background(), func(_ context.Context, p federation.ResultsPush) error {
		delivered = append(delivered, p)
		return nil
	})
	if sent != 1 || remaining != 0 {
		t.Fatalf("Drain = (%d sent, %d remaining), want (1, 0)", sent, remaining)
	}
	if len(delivered) != 1 || delivered[0].Tallies.Passed != 30 || delivered[0].Tallies.Total != 33 {
		t.Fatalf("delivered %+v — the REAL tallies must arrive, not 0/0/0/0 (the defect)", delivered)
	}
	if delivered[0].RunID != "20260806T130924607" {
		t.Errorf("run id = %q, want the original", delivered[0].RunID)
	}
}

func TestOutbox_EntrySurvivesAFailedPushAndLandsLater(t *testing.T) {
	o := newOutbox(t)
	if err := o.Put(samplePush("r1", 5, 5)); err != nil {
		t.Fatal(err)
	}
	// The CP is down: the entry must NOT be consumed.
	if sent, remaining := o.Drain(context.Background(), func(context.Context, federation.ResultsPush) error {
		return errors.New("connection refused")
	}); sent != 0 || remaining != 1 {
		t.Fatalf("failed drain = (%d, %d), want (0, 1) — a failing push must never discard results", sent, remaining)
	}
	// …and the failure is recorded on the entry, so a stuck queue is diagnosable from the file alone.
	b, err := os.ReadFile(filepath.Join(o.Dir, "r1.json"))
	if err != nil {
		t.Fatalf("entry gone after a failed push: %v", err)
	}
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	if e.Attempts != 1 || e.LastError == "" {
		t.Errorf("attempts=%d lastError=%q, want the failure recorded", e.Attempts, e.LastError)
	}
	// The CP returns.
	if sent, remaining := o.Drain(context.Background(), func(context.Context, federation.ResultsPush) error {
		return nil
	}); sent != 1 || remaining != 0 {
		t.Fatalf("recovery drain = (%d, %d), want (1, 0)", sent, remaining)
	}
}

func TestOutbox_DeliversOldestFirst(t *testing.T) {
	o := newOutbox(t)
	for _, id := range []string{"20260806T120000000", "20260806T100000000", "20260806T110000000"} {
		if err := o.Put(samplePush(id, 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	o.Drain(context.Background(), func(_ context.Context, p federation.ResultsPush) error {
		order = append(order, p.RunID)
		return nil
	})
	want := []string{"20260806T100000000", "20260806T110000000", "20260806T120000000"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("delivery order = %v, want %v — results must reach the ledger as they were produced", order, want)
		}
	}
}

// A failing entry must not block the ones behind it, or one poison payload stalls every later run.
func TestOutbox_OneFailureDoesNotBlockTheQueue(t *testing.T) {
	o := newOutbox(t)
	for _, id := range []string{"a1", "b2", "c3"} {
		if err := o.Put(samplePush(id, 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	sent, remaining := o.Drain(context.Background(), func(_ context.Context, p federation.ResultsPush) error {
		if p.RunID == "a1" {
			return errors.New("the CP rejects this one forever")
		}
		return nil
	})
	if sent != 2 || remaining != 1 {
		t.Fatalf("Drain = (%d, %d), want (2, 1) — the healthy entries behind a stuck one must still go", sent, remaining)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, "a1.json")); err != nil {
		t.Errorf("the rejected entry was discarded (%v) — it must be kept, that is the point", err)
	}
}

func TestOutbox_IgnoresTornWritesAndIsIdempotent(t *testing.T) {
	o := newOutbox(t)
	if err := o.Put(samplePush("r1", 1, 1)); err != nil {
		t.Fatal(err)
	}
	// a crash mid-write leaves this behind; it must never be parsed or delivered
	if err := os.WriteFile(filepath.Join(o.Dir, ".tmp-r9.json"), []byte("{half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := o.Pending(); got != 1 {
		t.Errorf("Pending = %d, want 1 — a .tmp- torn write is not a queued run", got)
	}
	if sent, _ := o.Drain(context.Background(), func(context.Context, federation.ResultsPush) error { return nil }); sent != 1 {
		t.Errorf("sent = %d, want 1", sent)
	}
	if err := o.Ack("r1"); err != nil { // already gone
		t.Errorf("Ack must be idempotent, got %v", err)
	}
}

func TestOutbox_DisabledIsANoOp(t *testing.T) {
	var o *Outbox // nil receiver — every call site must survive it
	if err := o.Put(samplePush("r1", 1, 1)); err != nil {
		t.Errorf("Put on a nil Outbox: %v", err)
	}
	if err := o.Ack("r1"); err != nil {
		t.Errorf("Ack on a nil Outbox: %v", err)
	}
	if o.Pending() != 0 {
		t.Error("Pending on a nil Outbox must be 0")
	}
	if sent, rem := o.Drain(context.Background(), func(context.Context, federation.ResultsPush) error {
		t.Fatal("a disabled outbox must never push")
		return nil
	}); sent != 0 || rem != 0 {
		t.Errorf("Drain on a nil Outbox = (%d, %d)", sent, rem)
	}
}

// ── the ORDERING guarantee, end to end through the real Client ───────────────────────────────────
// Put must happen BEFORE the network call. Proven from the server's side: the handler checks the
// filesystem while the request is in flight. If the ordering is ever reversed, the file is absent at
// that moment and this fails — which no amount of "it works" testing after the fact would catch.
func TestPushWithRetry_QueuesBeforeTheNetworkCall(t *testing.T) {
	dir := t.TempDir()
	priv, err := LoadOrCreateKey(filepath.Join(dir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "outbox")

	queuedWhenCalled := false
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if _, statErr := os.Stat(filepath.Join(outDir, "r42.json")); statErr == nil {
			queuedWhenCalled = true
		}
		w.WriteHeader(http.StatusInternalServerError) // the CP is having a bad day
	}))
	defer srv.Close()

	e := &Executor{
		Client:      NewClient(srv.URL, "inst", priv),
		Outbox:      &Outbox{Dir: outDir},
		PushRetries: 1,
		Log:         func(string, ...any) {},
	}
	e.pushWithRetry(context.Background(), samplePush("r42", 30, 33))

	if calls == 0 {
		t.Fatal("the client never called the CP")
	}
	if !queuedWhenCalled {
		t.Error("the results were NOT on disk when the push was attempted — Put must precede Push, or a crash mid-push loses the run")
	}
	if got := e.Outbox.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1 — a rejected push must stay queued", got)
	}

	// The CP comes back; the queued run lands without anyone re-running anything.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ok.Close()
	e.Client = NewClient(ok.URL, "inst", priv)
	e.drainOutbox(context.Background())
	if got := e.Outbox.Pending(); got != 0 {
		t.Errorf("Pending = %d after the CP returned, want 0 — the queue must drain on a reachable poll", got)
	}
}

// ── the THIRD seam (CP-M3-III-62) ────────────────────────────────────────────────────────────────
// The Stage III live gate found that fixing Executor.pushWithRetry and DirectRun was not enough:
// Client.ReportUp is a SEPARATE terminal-push seam — used by the MCP runner__run tool and by the
// `run` CLI, neither of which builds an Executor — and it still pushed once and gave up. The
// cloud-requested path recovered its results after a simulated outage; the MCP direct path did not.
//
// Unit tests passed the whole time, because they only covered the two seams that had been fixed.
// This is the test that fails if a fourth seam is ever added without an outbox behind it.
func TestReportUp_QueuesBeforeTheNetworkCall(t *testing.T) {
	dir := t.TempDir()
	priv, err := LoadOrCreateKey(filepath.Join(dir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "outbox")

	queuedWhenCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, e := os.Stat(filepath.Join(outDir, "direct-1.json")); e == nil {
			queuedWhenCalled = true
		}
		w.WriteHeader(http.StatusServiceUnavailable) // the CP is down mid-report
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "inst", priv)
	c.Outbox = &Outbox{Dir: outDir}
	rep := &report.Report{}
	rep.Summary.Total, rep.Summary.Passed = 33, 30

	if err := c.ReportUp(context.Background(), rep, "direct-1", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}); err == nil {
		t.Fatal("ReportUp must surface the push error so the caller can log it")
	}
	if !queuedWhenCalled {
		t.Error("the results were NOT on disk when the push was attempted — a direct/MCP run still loses them on an outage")
	}
	if got := c.Outbox.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1 — the run must stay queued for the executor's poll loop to drain", got)
	}

	// the executor's loop, sharing the same results volume, delivers it
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ok.Close()
	e := &Executor{Client: NewClient(ok.URL, "inst", priv), Outbox: c.Outbox, Log: func(string, ...any) {}}
	e.drainOutbox(context.Background())
	if got := e.Outbox.Pending(); got != 0 {
		t.Errorf("Pending = %d after the CP returned, want 0", got)
	}
}

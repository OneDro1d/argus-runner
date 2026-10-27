package runner

// a run whose executor process died in the middle of it is reported as ended, with a reason,
// instead of being left for the watchdog to call `abandoned`. A "previous process" is modelled the only honest
// way: it writes its marker to the volume and is never heard from again; a NEW Executor over the same directories
// is the restarted process.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// fakeCP answers the federation endpoints an Executor.Loop uses and records every results push. The first poll
// hands out `assign` when it is non-nil; every later poll is empty.
type fakeCP struct {
	mu      sync.Mutex
	pushes  []federation.ResultsPush
	assign  *federation.RunAssignment
	polls   int
	srv     *httptest.Server
	pushErr bool
}

func newFakeCP(t *testing.T, assign *federation.RunAssignment) *fakeCP {
	t.Helper()
	cp := &fakeCP{assign: assign}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		switch r.URL.Path {
		case "/fed/poll":
			cp.polls++
			resp := federation.PollResponse{}
			if cp.assign != nil {
				resp.HasRun, resp.Run = true, cp.assign
				cp.assign = nil
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/fed/results":
			var p federation.ResultsPush
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p.Status != "running" { // a heartbeat is not a result
				cp.pushes = append(cp.pushes, p)
			}
			if cp.pushErr {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *fakeCP) results() []federation.ResultsPush {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]federation.ResultsPush(nil), cp.pushes...)
}

func (cp *fakeCP) pollCount() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.polls
}

// loopUntil runs e.Loop until the fake CP has seen at least `polls` polls AND `pushes` result pushes (or fails).
func loopUntil(t *testing.T, e *Executor, cp *fakeCP, polls, pushes int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Loop(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cp.pollCount() >= polls && len(cp.results()) >= pushes {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// let any extra push that is going to happen happen, then stop
	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done
	if cp.pollCount() < polls || len(cp.results()) < pushes {
		t.Fatalf("the loop did not reach %d polls / %d pushes: polls=%d pushes=%d", polls, pushes, cp.pollCount(), len(cp.results()))
	}
}

type inflightRig struct {
	root   string
	flight *InFlight
	outbox *Outbox
}

func newInflightRig(t *testing.T, owner string) inflightRig {
	t.Helper()
	root := t.TempDir()
	return inflightRig{
		root:   root,
		flight: &InFlight{Dir: InFlightDir(root, "inst"), Owner: owner},
		outbox: &Outbox{Dir: OutboxDir(root, "inst")},
	}
}

func (rg inflightRig) executor(t *testing.T, cp *fakeCP, run RunFunc) *Executor {
	t.Helper()
	priv, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{
		Client: NewClient(cp.srv.URL, "inst", priv), Run: run, RunnerVersion: "0.0.0-test",
		PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond, PushRetries: 1,
		HeartbeatEvery: time.Hour, Outbox: rg.outbox, InFlight: rg.flight,
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	}
}

func neverRuns(t *testing.T) RunFunc {
	return func(context.Context, *federation.RunAssignment) (federation.ResultsPush, error) {
		t.Error("no run was assigned: Run must not be called")
		return federation.ResultsPush{}, nil
	}
}

func markerFiles(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

// THE PROMISE: a marker left by a previous process of this pod produces exactly ONE error-type push with the
// reason, the marker is gone afterwards, and a further restart reports nothing.
func TestInFlight_APreviousProcessesRunIsReportedOnceWithItsReason(t *testing.T) {
	rg := newInflightRig(t, "pod-a")
	started := time.Now().Add(-5 * time.Minute)
	// the previous process: picked a run up, wrote its marker, was killed.
	if err := rg.flight.Begin("20261019T101500000", "rr-1", "full", "hash-1", started); err != nil {
		t.Fatal(err)
	}

	cp := newFakeCP(t, nil)
	loopUntil(t, rg.executor(t, cp, neverRuns(t)), cp, 2, 1)

	got := cp.results()
	if len(got) != 1 {
		t.Fatalf("want exactly one results push for the interrupted run, got %d: %+v", len(got), got)
	}
	p := got[0]
	if p.RunID != "20261019T101500000" || p.RunRequestID != "rr-1" || p.Scope != "full" || p.SetHash != "hash-1" {
		t.Errorf("the push must carry the interrupted run's identity, got %+v", p)
	}
	if p.Status != "failed" {
		t.Errorf("status = %q, want the error-type terminal status \"failed\" (never completed, never abandoned)", p.Status)
	}
	if !strings.Contains(p.FailureReason, "executor process stopped during this run") || !strings.Contains(p.FailureReason, "out-of-memory") {
		t.Errorf("the push must say why: %q", p.FailureReason)
	}
	if p.Outcome != "" || p.Tallies.Total != 0 || len(p.Scenarios) != 0 || p.StartedAt == nil || p.FinishedAt == nil {
		t.Errorf("no verdict, no tallies, and the real start time: %+v", p)
	}
	if files := markerFiles(t, rg.flight.Dir); len(files) != 0 {
		t.Errorf("the marker must be gone once the report is safe, found %v", files)
	}
	if rg.outbox.Pending() != 0 {
		t.Errorf("the delivered report must be acknowledged, %d still queued", rg.outbox.Pending())
	}

	// a second restart finds nothing to report.
	cp2 := newFakeCP(t, nil)
	loopUntil(t, rg.executor(t, cp2, neverRuns(t)), cp2, 2, 0)
	if n := len(cp2.results()); n != 0 {
		t.Fatalf("the second restart pushed %d results, want none", n)
	}
}

// Another replica's marker (it shares the volume) is a run in progress there: it is not reported and not removed.
func TestInFlight_AnotherPodsMarkerIsLeftAlone(t *testing.T) {
	rg := newInflightRig(t, "pod-a")
	other := &InFlight{Dir: rg.flight.Dir, Owner: "pod-b"}
	if err := other.Begin("20261019T101500000", "rr-9", "full", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	cp := newFakeCP(t, nil)
	loopUntil(t, rg.executor(t, cp, neverRuns(t)), cp, 2, 0)
	if n := len(cp.results()); n != 0 {
		t.Fatalf("pod-a reported pod-b's run: %+v", cp.results())
	}
	if files := markerFiles(t, rg.flight.Dir); len(files) != 1 {
		t.Fatalf("pod-b's marker must stay, found %v", files)
	}
}

// A run whose results are already queued in the outbox finished; only the marker was left. No second verdict.
func TestInFlight_AMarkerWhoseResultsAreQueuedIsDroppedWithoutAPush(t *testing.T) {
	rg := newInflightRig(t, "pod-a")
	if err := rg.flight.Begin("r-done", "rr-1", "full", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := rg.outbox.Put(samplePush("r-done", 3, 3)); err != nil {
		t.Fatal(err)
	}
	cp := newFakeCP(t, nil)
	e := rg.executor(t, cp, neverRuns(t))
	e.reportInterruptedRuns(context.Background())
	if n := len(cp.results()); n != 0 {
		t.Fatalf("a finished run must not be reported as stopped, got %+v", cp.results())
	}
	if files := markerFiles(t, rg.flight.Dir); len(files) != 0 {
		t.Fatalf("the stale marker should be dropped, found %v", files)
	}
	if rg.outbox.Pending() != 1 {
		t.Fatal("the real results must stay queued for delivery")
	}
}

// A clean run: the marker exists WHILE the run runs, one completed push is sent, and no marker is left.
func TestInFlight_ACleanRunLeavesNoMarker(t *testing.T) {
	rg := newInflightRig(t, "pod-a")
	cp := newFakeCP(t, &federation.RunAssignment{RunRequestID: "rr-2", RunID: "20261019T111500000", Scope: "full", SetHash: "h"})
	sawMarker := false
	run := func(_ context.Context, a *federation.RunAssignment) (federation.ResultsPush, error) {
		sawMarker = len(rg.flight.Interrupted()) == 1
		return federation.ResultsPush{RunID: a.RunID, RunRequestID: a.RunRequestID, Scope: a.Scope, Status: "completed",
			Tallies: federation.Tallies{Passed: 1, Total: 1}}, nil
	}
	loopUntil(t, rg.executor(t, cp, run), cp, 2, 1)
	if !sawMarker {
		t.Error("the run must be marked while it runs")
	}
	got := cp.results()
	if len(got) != 1 || got[0].Status != "completed" || got[0].FailureReason != "" {
		t.Fatalf("want exactly one completed push, got %+v", got)
	}
	if files := markerFiles(t, rg.flight.Dir); len(files) != 0 {
		t.Fatalf("a clean run leaves no marker, found %v", files)
	}
}

// A run that errors (no push produced) is closed with a minimal failed push, and its marker goes too.
func TestInFlight_ARunThatErrorsLeavesNoMarker(t *testing.T) {
	rg := newInflightRig(t, "pod-a")
	cp := newFakeCP(t, &federation.RunAssignment{RunRequestID: "rr-3", RunID: "20261019T121500000", Scope: "full"})
	run := func(context.Context, *federation.RunAssignment) (federation.ResultsPush, error) {
		return federation.ResultsPush{}, ownWordsf("the config would not parse")
	}
	loopUntil(t, rg.executor(t, cp, run), cp, 2, 1)
	if files := markerFiles(t, rg.flight.Dir); len(files) != 0 {
		t.Fatalf("an errored run leaves no marker, found %v", files)
	}
	if got := cp.results(); len(got) != 1 || got[0].Status != "failed" {
		t.Fatalf("want one failed push, got %+v", got)
	}
}

// With no marker directory configured (compose without a results volume) nothing is written and nothing breaks.
func TestInFlight_ZeroValueIsANoOp(t *testing.T) {
	var f *InFlight
	if err := f.Begin("r", "", "full", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := (&InFlight{}).End("r"); err != nil {
		t.Fatal(err)
	}
	if len(f.Interrupted()) != 0 {
		t.Fatal("a nil store knows nothing")
	}
}

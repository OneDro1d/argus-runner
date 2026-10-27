package fault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// ── fakes ────────────────────────────────────────────────────────────────────────────────────────

type fakeKube struct {
	mu       sync.Mutex
	scale    map[string]int
	ready    map[string]int
	calls    []string
	failSet  map[string]error
	onSet    func(ns, name string, n int)
	notReady int // GetReady answers "not settled" this many times first
}

func newFakeKube() *fakeKube {
	return &fakeKube{scale: map[string]int{}, ready: map[string]int{}, failSet: map[string]error{}}
}
func (k *fakeKube) GetScale(_ context.Context, ns, name string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n, ok := k.scale[ns+"/"+name]
	if !ok {
		return 0, errors.New("not found")
	}
	return n, nil
}
func (k *fakeKube) SetScale(_ context.Context, ns, name string, n int) error {
	k.mu.Lock()
	hook := k.onSet
	err := k.failSet[ns+"/"+name]
	k.mu.Unlock()
	if hook != nil {
		hook(ns, name, n)
	}
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, fmt.Sprintf("scale %s/%s=%d", ns, name, n))
	k.scale[ns+"/"+name] = n
	k.ready[ns+"/"+name] = n
	return nil
}
func (k *fakeKube) Ready(_ context.Context, ns, name string) (int, int, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.notReady > 0 {
		k.notReady--
		return k.scale[ns+"/"+name], 0, true, nil
	}
	return k.scale[ns+"/"+name], k.ready[ns+"/"+name], true, nil
}

type fakeToxi struct {
	mu      sync.Mutex
	toxics  map[string][]string // proxy -> toxic names
	enabled map[string]bool
	calls   []string
	onAdd   func(proxy string, t Toxic)
}

func newFakeToxi() *fakeToxi {
	return &fakeToxi{toxics: map[string][]string{}, enabled: map[string]bool{"trades": true}}
}
func (f *fakeToxi) ProxyEnabled(_ context.Context, p string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled[p], nil
}
func (f *fakeToxi) SetEnabled(_ context.Context, p string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("enable %s=%v", p, on))
	f.enabled[p] = on
	return nil
}
func (f *fakeToxi) AddToxic(_ context.Context, p string, t Toxic) error {
	if f.onAdd != nil {
		f.onAdd(p, t)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "add "+p+"/"+t.Name+" "+t.Type)
	f.toxics[p] = append(f.toxics[p], t.Name)
	return nil
}
func (f *fakeToxi) DeleteToxic(_ context.Context, p, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "del "+p+"/"+name)
	for i, n := range f.toxics[p] {
		if n == name {
			f.toxics[p] = append(f.toxics[p][:i], f.toxics[p][i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

type fakeTargets struct{}

func (fakeTargets) Deployment(n string) (string, bool) {
	if n == "trades" || n == "trades-mcp-server" {
		return "calm-demo", true
	}
	return "", false
}
func (fakeTargets) Link(n string) (string, string, bool) {
	if n == "mcp-to-trades" {
		return "http://toxiproxy.secret-host.invalid:8474", "trades", true
	}
	return "", "", false
}

type rig struct {
	m  *Manager
	k  *fakeKube
	tx *fakeToxi
	j  *Journal
}

func newRig(t *testing.T) *rig {
	t.Helper()
	k, tx := newFakeKube(), newFakeToxi()
	k.scale["calm-demo/trades"], k.ready["calm-demo/trades"] = 2, 2
	k.scale["calm-demo/trades-mcp-server"], k.ready["calm-demo/trades-mcp-server"] = 1, 1
	j := &Journal{Dir: filepath.Join(t.TempDir(), "fault-journal")}
	m := New(Config{
		Run: "tr-abc", Journal: j, Targets: fakeTargets{}, Owner: "pod-a",
		Kube: func() (Kube, error) { return k, nil }, Toxi: func(string) Toxi { return tx },
		HeartbeatEvery: time.Hour, UndoTimeout: 5 * time.Second,
	})
	RestoreBackoff = time.Millisecond
	t.Cleanup(func() { m.Finish(context.Background()) })
	return &rig{m: m, k: k, tx: tx, j: j}
}

func ctx() context.Context { return context.Background() }

func pending(t *testing.T, j *Journal) []Undo {
	t.Helper()
	p, err := j.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ── the journal write happens BEFORE the fault is applied ───────────────────────────────────────

func TestApply_UndoIsJournaledBeforeTheScaleIsApplied(t *testing.T) {
	r := newRig(t)
	var seen []Undo
	r.k.onSet = func(ns, name string, n int) { seen = pending(t, r.j) }
	if err := r.m.Apply(ctx(), Spec{Step: "stop-api", Op: OpStop, Deployment: "trades"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Kind != KindScale || seen[0].Replicas != 2 || seen[0].Deployment != "trades" {
		t.Fatalf("at the moment the deployment was scaled the journal held %+v, want one scale undo back to 2", seen)
	}
	if got := r.k.scale["calm-demo/trades"]; got != 0 {
		t.Fatalf("stop left %d replicas", got)
	}
}

func TestApply_ToxicUndoIsJournaledBeforeTheToxicIsAdded(t *testing.T) {
	r := newRig(t)
	var seen []Undo
	r.tx.onAdd = func(string, Toxic) { seen = pending(t, r.j) }
	if err := r.m.Apply(ctx(), Spec{Step: "slow", Op: OpDelay, Link: "mcp-to-trades", LatencyMs: 5000}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Kind != KindToxic || !strings.HasPrefix(seen[0].Toxic, "argus-tr-abc-") {
		t.Fatalf("journal at add time: %+v", seen)
	}
}

func TestApply_JournalWriteFailureMeansNothingIsApplied(t *testing.T) {
	r := newRig(t)
	if err := os.WriteFile(r.j.Dir, []byte("x"), 0o600); err != nil { // a FILE where the directory must be
		t.Fatal(err)
	}
	err := r.m.Apply(ctx(), Spec{Step: "s", Op: OpStop, Deployment: "trades"})
	if err == nil {
		t.Fatal("a fault whose undo could not be made durable was applied")
	}
	if len(r.k.calls) != 0 {
		t.Fatalf("the cluster was touched anyway: %v", r.k.calls)
	}
}

// ── the allowlist is the door ───────────────────────────────────────────────────────────────────

func TestApply_UnlistedTargetIsRefusedBeforeAnythingHappens(t *testing.T) {
	r := newRig(t)
	for _, sp := range []Spec{
		{Step: "s", Op: OpStop, Deployment: "payments"},
		{Step: "s", Op: OpDelay, Link: "other", LatencyMs: 10},
	} {
		err := r.m.Apply(ctx(), sp)
		if err == nil || !strings.Contains(err.Error(), "fault_allowed_targets") {
			t.Fatalf("%+v: want a refusal naming fault_allowed_targets, got %v", sp, err)
		}
	}
	if len(pending(t, r.j)) != 0 || len(r.k.calls) != 0 || len(r.tx.calls) != 0 {
		t.Fatal("a refused fault left a journal entry or touched a system")
	}
}

func TestApply_RefusalNeverEchoesAURL(t *testing.T) {
	r := newRig(t)
	r.tx.onAdd = nil
	r.m.cfg.Toxi = func(string) Toxi { return failingToxi{} }
	err := r.m.Apply(ctx(), Spec{Step: "s", Op: OpDelay, Link: "mcp-to-trades", LatencyMs: 10})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "secret-host") || strings.Contains(err.Error(), "8474") {
		t.Fatalf("error leaks the toxiproxy address: %v", err)
	}
}

type failingToxi struct{}

func (failingToxi) ProxyEnabled(context.Context, string) (bool, error) {
	return false, errors.New("dial tcp secret-host.invalid:8474: refused")
}
func (failingToxi) SetEnabled(context.Context, string, bool) error { return errors.New("x") }
func (failingToxi) AddToxic(context.Context, string, Toxic) error  { return errors.New("x") }
func (failingToxi) DeleteToxic(context.Context, string, string) (bool, error) {
	return false, errors.New("x")
}

// ── restore on every path ───────────────────────────────────────────────────────────────────────

func TestFinish_UndoesEveryActiveFaultInReverseOrder(t *testing.T) {
	r := newRig(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"}))
	must(r.m.Apply(ctx(), Spec{Step: "b", Op: OpScale, Deployment: "trades-mcp-server", Replicas: ptr(4)}))
	must(r.m.Apply(ctx(), Spec{Step: "c", Op: OpDelay, Link: "mcp-to-trades", LatencyMs: 5000, JitterMs: 100}))
	r.k.calls, r.tx.calls = nil, nil
	if errs := r.m.Finish(ctx()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(r.tx.calls) != 1 || !strings.HasPrefix(r.tx.calls[0], "del trades/argus-tr-abc-") {
		t.Fatalf("toxic not removed first: %v", r.tx.calls)
	}
	want := []string{"scale calm-demo/trades-mcp-server=1", "scale calm-demo/trades=2"}
	if strings.Join(r.k.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("restore order %v, want %v", r.k.calls, want)
	}
	if len(pending(t, r.j)) != 0 {
		t.Fatal("journal not emptied after a successful restore")
	}
	for _, f := range r.m.Records() {
		if f.RestoreOutcome != report.FaultRestored || f.RestoredAt == "" {
			t.Fatalf("record %+v not marked restored", f)
		}
	}
}

func TestRestoreStep_EmptiesTheSetNow_AndFinishHasNothingLeft(t *testing.T) {
	r := newRig(t)
	if err := r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"}); err != nil {
		t.Fatal(err)
	}
	if errs := r.m.Restore(ctx()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if r.k.scale["calm-demo/trades"] != 2 {
		t.Fatal("restore step did not put the replicas back")
	}
	r.k.calls = nil
	r.m.Finish(ctx())
	if len(r.k.calls) != 0 {
		t.Fatalf("Finish undid something the restore step already undid: %v", r.k.calls)
	}
}

func TestStart_ScalesBackToTheRecordedCount_AndClosesTheStop(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "stop", Op: OpStop, Deployment: "trades"})
	if err := r.m.Apply(ctx(), Spec{Step: "start", Op: OpStart, Deployment: "trades"}); err != nil {
		t.Fatal(err)
	}
	if r.k.scale["calm-demo/trades"] != 2 {
		t.Fatal("start did not return to the count stop recorded")
	}
	if len(pending(t, r.j)) != 0 {
		t.Fatal("the stop's journal entry survived its own start")
	}
	if rs := r.m.Records(); len(rs) != 1 || rs[0].RestoreOutcome != report.FaultRestored {
		t.Fatalf("records: %+v", rs)
	}
}

func TestStart_WithoutAnEarlierStopIsRefused_UnlessReplicasGiven(t *testing.T) {
	r := newRig(t)
	if err := r.m.Apply(ctx(), Spec{Step: "start", Op: OpStart, Deployment: "trades"}); err == nil {
		t.Fatal("start with nothing recorded and no replicas must be refused")
	}
}

func TestFinish_FailedRestoreIsLoud_KeepsTheJournal_NeverPanics(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"})
	r.k.failSet["calm-demo/trades"] = errors.New("forbidden")
	errs := r.m.Finish(ctx())
	if len(errs) != 1 {
		t.Fatalf("a failed restore must be returned, got %v", errs)
	}
	rs := r.m.Records()
	if rs[0].RestoreOutcome != report.FaultRestoreFailed {
		t.Fatalf("record %+v", rs[0])
	}
	if len(pending(t, r.j)) != 1 {
		t.Fatal("the undo of a failed restore must stay journaled so the next start retries it")
	}
}

func TestRestore_ToleratesAlreadyRestored(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "slow", Op: OpDelay, Link: "mcp-to-trades", LatencyMs: 50})
	r.tx.toxics["trades"] = nil // the operator (or a replay) already removed it
	if errs := r.m.Finish(ctx()); len(errs) != 0 {
		t.Fatalf("already gone is a success: %v", errs)
	}
	if got := r.m.Records()[0].RestoreOutcome; got != report.FaultAlreadyRestored {
		t.Fatalf("outcome %q", got)
	}
}

func TestRestore_RemovesOnlyArgusOwnedToxics(t *testing.T) {
	r := newRig(t)
	r.tx.toxics["trades"] = []string{"ops-existing-latency"}
	_ = r.m.Apply(ctx(), Spec{Step: "slow", Op: OpDelay, Link: "mcp-to-trades", LatencyMs: 50})
	r.m.Finish(ctx())
	if got := r.tx.toxics["trades"]; len(got) != 1 || got[0] != "ops-existing-latency" {
		t.Fatalf("toxics after restore: %v", got)
	}
}

func TestDrop_DisablesTheProxy_AndRestoresOnlyWhatItDisabled(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "cut", Op: OpDrop, Link: "mcp-to-trades"})
	if r.tx.enabled["trades"] {
		t.Fatal("drop did not disable the proxy")
	}
	r.m.Finish(ctx())
	if !r.tx.enabled["trades"] {
		t.Fatal("restore did not re-enable it")
	}
	// the operator had it disabled already: we must not turn it on
	r2 := newRig(t)
	r2.tx.enabled["trades"] = false
	_ = r2.m.Apply(ctx(), Spec{Step: "cut", Op: OpDrop, Link: "mcp-to-trades"})
	r2.m.Finish(ctx())
	if r2.tx.enabled["trades"] {
		t.Fatal("restore enabled a proxy the operator had disabled")
	}
}

func TestLinkFault_IsRecordedWithToxiproxyInPath_AndNoURL(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "t", Op: OpTruncate, Link: "mcp-to-trades", Bytes: 100})
	f := r.m.Records()[0]
	if !f.Toxiproxy || f.Target != "mcp-to-trades" || f.Params["bytes"] != 100 || f.Op != OpTruncate {
		t.Fatalf("%+v", f)
	}
	if strings.Contains(fmt.Sprintf("%+v", f), "secret-host") {
		t.Fatal("record carries a URL")
	}
}

func TestApply_AfterFinishIsRefused(t *testing.T) { // an abandoned step's goroutine must not fire late
	r := newRig(t)
	r.m.Finish(ctx())
	if err := r.m.Apply(ctx(), Spec{Step: "late", Op: OpStop, Deployment: "trades"}); err == nil {
		t.Fatal("a fault applied after the chain ended")
	}
	if len(r.k.calls) != 0 {
		t.Fatal("cluster touched after Finish")
	}
}

func TestApply_APartialApplyIsUndoneAtOnce(t *testing.T) {
	r := newRig(t)
	n := 0
	r.k.onSet = func(string, string, int) { n++ }
	r.k.failSet["calm-demo/trades"] = errors.New("boom")
	if err := r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"}); err == nil {
		t.Fatal("want the apply error")
	}
	// the apply changed nothing, so the immediate undo finds the recorded count already in place
	if len(r.m.Records()) != 1 || r.m.Records()[0].RestoreOutcome != report.FaultAlreadyRestored {
		t.Fatalf("records %+v", r.m.Records())
	}
	if len(pending(t, r.j)) != 0 {
		t.Fatal("journal entry survived a closed fault")
	}
}

func TestWaitReady_WaitsForReadyEqualsSpec_AndTimesOut(t *testing.T) {
	r := newRig(t)
	PollEvery = time.Millisecond
	r.k.notReady = 3
	if err := r.m.Apply(ctx(), Spec{Step: "w", Op: OpWaitReady, Deployment: "trades", Timeout: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	r.k.notReady = 1 << 30
	err := r.m.Apply(ctx(), Spec{Step: "w", Op: OpWaitReady, Deployment: "trades", Timeout: 30 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("want a not-ready timeout, got %v", err)
	}
	if len(pending(t, r.j)) != 0 {
		t.Fatal("wait_ready journals nothing: it changes nothing")
	}
}

// ── a second run may not fault the same target ──────────────────────────────────────────────────

func TestTargetLock_ASecondLiveRunIsRefused(t *testing.T) {
	r := newRig(t)
	if err := r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"}); err != nil {
		t.Fatal(err)
	}
	other := New(Config{Run: "tr-other", Journal: r.j, Targets: fakeTargets{}, Owner: "pod-b",
		Kube: func() (Kube, error) { return r.k, nil }, Toxi: func(string) Toxi { return r.tx }, HeartbeatEvery: time.Hour})
	err := other.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"})
	if err == nil || !strings.Contains(err.Error(), "another run") {
		t.Fatalf("want a refusal naming another run, got %v", err)
	}
}

// ── startup replay ──────────────────────────────────────────────────────────────────────────────

func TestReplay_UndoesWhatADeadExecutorLeft_BeforeNewWork(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"})
	_ = r.m.Apply(ctx(), Spec{Step: "b", Op: OpDelay, Link: "mcp-to-trades", LatencyMs: 10})
	// the executor "dies": no Finish. A new process of the same pod starts.
	var logs []string
	res := Replay(ctx(), ReplayConfig{Journal: r.j, Owner: "pod-a", Kube: func() (Kube, error) { return r.k, nil },
		Toxi: func(string) Toxi { return r.tx }, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	if len(res) != 2 {
		t.Fatalf("replayed %d, want 2: %+v", len(res), res)
	}
	if r.k.scale["calm-demo/trades"] != 2 || len(r.tx.toxics["trades"]) != 0 {
		t.Fatal("the dead run's faults are still applied")
	}
	if len(pending(t, r.j)) != 0 {
		t.Fatal("journal not emptied")
	}
	if len(logs) == 0 {
		t.Fatal("a replay must say what it did")
	}
	if b, err := os.ReadFile(filepath.Join(r.j.Dir, "replay.log")); err != nil || !strings.Contains(string(b), "restored") {
		t.Fatalf("replay.log: %v %q", err, b)
	}
}

func TestReplay_LeavesALiveForeignRunAlone_ButTakesAStaleOne(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"}) // owned by pod-a, fresh mtime
	cfg := ReplayConfig{Journal: r.j, Owner: "pod-b", Kube: func() (Kube, error) { return r.k, nil },
		Toxi: func(string) Toxi { return r.tx }, StaleAfter: time.Hour, MaxWait: 20 * time.Millisecond, WaitStep: 5 * time.Millisecond}
	res := Replay(ctx(), cfg)
	if len(res) != 0 || r.k.scale["calm-demo/trades"] != 0 {
		t.Fatalf("replay undid a live run's fault: %+v", res)
	}
	cfg.StaleAfter = time.Nanosecond // now the owner has been silent for longer than the limit
	time.Sleep(2 * time.Millisecond)
	res = Replay(ctx(), cfg)
	if len(res) != 1 || r.k.scale["calm-demo/trades"] != 2 {
		t.Fatalf("stale entry not replayed: %+v", res)
	}
}

func TestReplay_AFailedUndoStaysJournaledAndIsReported(t *testing.T) {
	r := newRig(t)
	_ = r.m.Apply(ctx(), Spec{Step: "a", Op: OpStop, Deployment: "trades"})
	r.k.failSet["calm-demo/trades"] = errors.New("forbidden")
	res := Replay(ctx(), ReplayConfig{Journal: r.j, Owner: "pod-a", Kube: func() (Kube, error) { return r.k, nil }, Toxi: func(string) Toxi { return r.tx }})
	if len(res) != 1 || res[0].Outcome != report.FaultRestoreFailed {
		t.Fatalf("%+v", res)
	}
	if len(pending(t, r.j)) != 1 {
		t.Fatal("entry dropped although its undo failed")
	}
}

func TestJournal_CorruptEntryIsReportedNeverSilentlyDropped(t *testing.T) {
	j := &Journal{Dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(j.Dir, "00000000000000000001-x.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Pending(); err == nil {
		t.Fatal("a corrupt journal entry must be an error, not an absence")
	}
}

func ptr(n int) *int { return &n }

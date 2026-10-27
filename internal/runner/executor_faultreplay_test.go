package runner_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/fault"
	"github.com/OneDro1d/argus-runner/internal/runner"
)

// The executor start replays every pending fault undo BEFORE it takes new work.

type orderedLog struct {
	mu sync.Mutex
	ev []string
}

func (o *orderedLog) add(s string) { o.mu.Lock(); o.ev = append(o.ev, s); o.mu.Unlock() }
func (o *orderedLog) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ev...)
}

type replayKube struct {
	log      *orderedLog
	replicas int
}

func (k *replayKube) GetScale(context.Context, string, string) (int, error) { return k.replicas, nil }
func (k *replayKube) SetScale(_ context.Context, _, _ string, n int) error {
	k.log.add("undo")
	k.replicas = n
	return nil
}
func (k *replayKube) Ready(context.Context, string, string) (int, int, bool, error) {
	return k.replicas, k.replicas, true, nil
}

type oneTarget struct{}

func (oneTarget) Deployment(n string) (string, bool) { return "calm-demo", n == "trades" }
func (oneTarget) Link(string) (string, string, bool) { return "", "", false }

func TestExecutorLoop_ReplaysPendingFaultUndosBeforeItPollsForWork(t *testing.T) {
	log := &orderedLog{}
	kube := &replayKube{log: log, replicas: 2}
	jdir := filepath.Join(t.TempDir(), "fault-journal")
	owner := fault.Hostname()
	// a previous executor process applied a stop and died: its undo is in the journal, the deployment is at 0
	m := fault.New(fault.Config{Run: "tr-dead", Journal: &fault.Journal{Dir: jdir}, Targets: oneTarget{}, Owner: owner,
		Kube: func() (fault.Kube, error) { return kube, nil }, HeartbeatEvery: time.Hour})
	if err := m.Apply(context.Background(), fault.Spec{Step: "cut", Op: fault.OpStop, Deployment: "trades"}); err != nil {
		t.Fatal(err)
	}
	log.ev = nil // forget the apply; only the replay and the poll are under test

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add("poll")
		w.WriteHeader(500)
	}))
	defer srv.Close()
	priv, err := runner.LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	if err != nil {
		t.Fatal(err)
	}
	ex := &runner.Executor{
		Client: runner.NewClient(srv.URL, "inst", priv), Run: nil, RunnerVersion: "1.0.0",
		PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond,
		FaultJournalDir: jdir, FaultKube: func() (fault.Kube, error) { return kube, nil },
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ev := log.get(); len(ev) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	ev := log.get()
	if len(ev) < 2 || ev[0] != "undo" || ev[1] != "poll" {
		t.Fatalf("events %v: the undo must run before the first poll", ev)
	}
	if kube.replicas != 2 {
		t.Fatalf("the dead run's stop was not undone: %d", kube.replicas)
	}
}

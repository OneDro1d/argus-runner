package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// THE LOOP CONNECTS THE TWO HALVES. startHeartbeatTally, NewRunFunc and the fenced direct path each have their
// own test, and all of them stayed green with Loop handing the run a context WITHOUT the tally
// (`e.Run(ctx, …)` in place of `e.Run(withTally(ctx, tally), …)`): the heartbeat then read a tally nothing
// wrote to and sent zeros for the whole run, on the one path every federated run takes. This drives Loop
// itself: a polled run must receive the same Tally the heartbeat is reading.
func TestLoop_TheRunWritesTheTallyTheHeartbeatReads(t *testing.T) {
	var (
		mu     sync.Mutex
		polled bool
		pushes []federation.ResultsPush
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/fed/poll", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		first := !polled
		polled = true
		mu.Unlock()
		if first {
			_ = json.NewEncoder(w).Encode(federation.PollResponse{HasRun: true,
				Run: &federation.RunAssignment{RunID: "run-loop-tally", RunRequestID: "rr-loop-tally", Scope: "full"}})
			return
		}
		_ = json.NewEncoder(w).Encode(federation.PollResponse{HasRun: false})
	})
	mux.HandleFunc("/fed/results", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p federation.ResultsPush
		_ = json.Unmarshal(b, &p)
		mu.Lock()
		pushes = append(pushes, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	priv, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	gotTally := make(chan bool, 1)
	ex := &Executor{
		Client: NewClient(srv.URL, "inst-loop-tally", priv), RunnerVersion: "1.0.0",
		PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond, HeartbeatEvery: 10 * time.Millisecond, PushRetries: 2,
		Run: func(ctx context.Context, a *federation.RunAssignment) (federation.ResultsPush, error) {
			tl := tallyFrom(ctx)
			gotTally <- tl != nil
			tl.SetPlanned(3) // nil-safe: under the mutation this writes nowhere, which is the defect
			tl.Finished(report.ScenarioResult{ID: "S1", Status: "passed"})
			time.Sleep(150 * time.Millisecond) // several heartbeats' worth
			return federation.ResultsPush{RunID: a.RunID, RunRequestID: a.RunRequestID, Scope: a.Scope, Status: "completed",
				Tallies: federation.Tallies{Passed: 3, Total: 3}}, nil
		},
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()

	select {
	case ok := <-gotTally:
		if !ok {
			t.Error("Loop ran the assignment with a context that carries no Tally: the heartbeat reads counts nothing writes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never ran the assignment — the test proves nothing")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		finished := false
		for _, p := range pushes {
			finished = finished || p.Status == "completed"
		}
		mu.Unlock()
		if finished {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	var sawLive bool
	for _, p := range pushes {
		if p.Status != "running" {
			continue
		}
		if len(p.Scenarios) != 0 {
			t.Errorf("a heartbeat carried a scenario list: %+v", p.Scenarios)
		}
		if p.Tallies.Planned == 3 && p.Tallies.Passed == 1 && p.Tallies.Total == 1 {
			sawLive = true
		}
	}
	if !sawLive {
		t.Errorf("no heartbeat carried the run's counts so far (want planned 3, passed 1, total 1) among %d pushes; the last heartbeat's tallies: %+v", len(pushes), pushes[0].Tallies)
	}
}

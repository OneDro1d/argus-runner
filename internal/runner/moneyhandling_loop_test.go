package runner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/runner"
)

// T5.4 follow-up — the LOOP actually sends the SUT's money_handling on its polls.
// moneyHandlingFor and pollRequestFor are each tested on their own; this proves Loop connects them.
// If it did not, every poll would carry nil, the control plane would keep NULL for every k3d/aks
// instance (whose re-register is refused), and its guard would never turn on: silently, because NULL
// is by design "not money-handling".
func TestExecutor_PollsCarryTheSUTsMoneyHandling(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte("project:\n  name: x\nmoney_handling: true\ntargets:\n  http:\n    base_url: http://sut:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		seen []*bool
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/fed/poll", func(w http.ResponseWriter, r *http.Request) {
		var body federation.PollRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.MoneyHandling)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(federation.PollResponse{HasRun: false})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	priv, _ := runner.LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	ex := &runner.Executor{
		Client: runner.NewClient(srv.URL, "inst-money-loop", priv), RunnerVersion: "1.0.0",
		ConfigPath: cfg,
		PollGap:    10 * time.Millisecond, Backoff: 10 * time.Millisecond,
		Run: func(context.Context, *federation.RunAssignment) (federation.ResultsPush, error) {
			return federation.ResultsPush{}, nil
		},
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the loop never polled — the test proves nothing")
	}
	if seen[0] == nil || !*seen[0] {
		t.Fatalf("the loop's poll carried money_handling=%v for a config declaring true", seen[0])
	}
}

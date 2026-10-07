package runner_test

import (
	"context"
	"encoding/json"
	"fmt"
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

// UC073: the executor observes a SUT redeploy (a change in the SUT's declared deployment fingerprint)
// and emits a deployment marker — but NOT on the first observation (baseline) and NOT on an unchanged
// fingerprint (a wrong marker would falsely reset Current SUT State). TEST-FIRST: this is the change-
// detection safety core.

// stubFP is a controllable fingerprint source.
type stubFP struct {
	mu  sync.Mutex
	val string
	err error
	n   int
}

func (s *stubFP) fn(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.val, s.err
}
func (s *stubFP) set(v string) { s.mu.Lock(); s.val = v; s.mu.Unlock() }

func TestDeploymentWatcher_BaselineThenChange(t *testing.T) {
	fp := &stubFP{val: "v1"}
	w := runner.NewDeploymentWatcher(fp.fn, "")
	w.MinInterval = 0 // probe every call in the test

	// 1) baseline — first observation establishes state, emits NO marker.
	if m, changed, err := w.Check(context.Background()); err != nil || changed || m != nil {
		t.Fatalf("baseline: got (marker=%s changed=%v err=%v), want no marker", m, changed, err)
	}
	// 2) unchanged — no marker.
	if _, changed, _ := w.Check(context.Background()); changed {
		t.Fatal("unchanged fingerprint must NOT emit a marker")
	}
	// 3) CHANGE — exactly one marker, carrying the new fingerprint.
	fp.set("v2")
	m, changed, err := w.Check(context.Background())
	if err != nil || !changed || m == nil {
		t.Fatalf("change: got (marker=%s changed=%v err=%v), want a marker", m, changed, err)
	}
	var obj map[string]any
	if json.Unmarshal(m, &obj) != nil || obj["fingerprint"] != "v2" || obj["source"] != "auto" || obj["at"] == nil {
		t.Fatalf("marker = %s, want {at, source:auto, fingerprint:v2}", m)
	}
	// 4) same new value again — no re-emit.
	if _, changed, _ := w.Check(context.Background()); changed {
		t.Fatal("re-observing the SAME new fingerprint must NOT re-emit")
	}
}

func TestDeploymentWatcher_ProbeErrorNoMarker(t *testing.T) {
	fp := &stubFP{val: "v1"}
	w := runner.NewDeploymentWatcher(fp.fn, "")
	w.MinInterval = 0
	if _, _, _ = w.Check(context.Background()); false {
	} // baseline v1
	fp.mu.Lock()
	fp.err = fmt.Errorf("connection refused")
	fp.mu.Unlock()
	m, changed, err := w.Check(context.Background())
	if err == nil || changed || m != nil {
		t.Fatalf("probe error: got (marker=%s changed=%v err=%v), want error + no marker", m, changed, err)
	}
	// recovery: the error must not have corrupted the baseline — same value ⇒ still no marker.
	fp.mu.Lock()
	fp.err = nil
	fp.mu.Unlock()
	if _, changed, _ := w.Check(context.Background()); changed {
		t.Fatal("after a transient probe error, an UNCHANGED fingerprint must not emit")
	}
}

func TestDeploymentWatcher_PersistedBaselineAcrossRestart(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "deploy-fingerprint")
	// first watcher establishes + persists baseline "v1".
	fp1 := &stubFP{val: "v1"}
	w1 := runner.NewDeploymentWatcher(fp1.fn, cache)
	w1.MinInterval = 0
	if _, changed, _ := w1.Check(context.Background()); changed {
		t.Fatal("first baseline must not emit")
	}
	if b, _ := os.ReadFile(cache); string(b) == "" {
		t.Fatal("baseline was not persisted")
	}
	// the SUT redeploys while the executor is DOWN → a fresh watcher (same cache) sees "v2" first and
	// MUST emit (the redeploy happened, we just weren't watching).
	fp2 := &stubFP{val: "v2"}
	w2 := runner.NewDeploymentWatcher(fp2.fn, cache)
	w2.MinInterval = 0
	if _, changed, _ := w2.Check(context.Background()); !changed {
		t.Fatal("a fresh watcher whose persisted baseline differs from the current SUT MUST emit a marker")
	}
}

func TestDeploymentWatcher_MinIntervalGate(t *testing.T) {
	fp := &stubFP{val: "v1"}
	now := time.Unix(1_000_000, 0)
	w := runner.NewDeploymentWatcher(fp.fn, "")
	w.MinInterval = 15 * time.Second
	w.Now = func() time.Time { return now }

	_, _, _ = w.Check(context.Background()) // baseline (probes)
	fp.set("v2")
	// within the interval: MUST NOT probe (n unchanged) and MUST NOT emit.
	if _, changed, _ := w.Check(context.Background()); changed {
		t.Fatal("within MinInterval the watcher must not emit")
	}
	fp.mu.Lock()
	nAfter := fp.n
	fp.mu.Unlock()
	if nAfter != 1 {
		t.Fatalf("within MinInterval the watcher probed %d times, want 1 (gated)", nAfter)
	}
	// past the interval: probes again and emits the change.
	now = now.Add(20 * time.Second)
	if _, changed, _ := w.Check(context.Background()); !changed {
		t.Fatal("past MinInterval the watcher must probe and emit the change")
	}
}

// TestExecutor_AutoDeploymentMarker proves the LOOP wiring end-to-end without a SUT or PG: a stub CP
// answers empty polls and records /fed/deployment hits; a stub fingerprint flips → the loop pushes a
// marker carrying the new value.
func TestExecutor_AutoDeploymentMarker(t *testing.T) {
	var (
		mu      sync.Mutex
		markers []json.RawMessage
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/fed/poll", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(federation.PollResponse{HasRun: false})
	})
	mux.HandleFunc("/fed/deployment", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		markers = append(markers, body["marker"])
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	priv, _ := runner.LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	client := runner.NewClient(srv.URL, "inst-dw", priv)

	fp := &stubFP{val: "build-1"}
	watch := runner.NewDeploymentWatcher(fp.fn, "")
	watch.MinInterval = 0
	ex := &runner.Executor{
		Client: client, RunnerVersion: "1.0.0", DeployWatch: watch,
		PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond,
		Run: func(context.Context, *federation.RunAssignment) (federation.ResultsPush, error) {
			return federation.ResultsPush{}, nil
		},
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()

	time.Sleep(60 * time.Millisecond) // let it establish the baseline over a few empty polls
	fp.set("build-2")                 // SUT redeploys

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(markers)
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
	if len(markers) == 0 {
		t.Fatal("the loop never auto-pushed a deployment marker after the fingerprint changed")
	}
	var obj map[string]any
	if json.Unmarshal(markers[0], &obj) != nil || obj["fingerprint"] != "build-2" {
		t.Fatalf("auto marker = %s, want fingerprint build-2", markers[0])
	}
}

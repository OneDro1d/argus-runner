package runner

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// ⛔ ENV-GATED, and meant to run in an OLD tree. scripts/compare-old-executor-check.sh copies this file
// into a git worktree of the last release before `compare` runs (v0.3.56) and runs it THERE
// (ARGUS-CMP-3, design 11.2 item 2): an old executor handed a compare assignment must fire NOTHING.
//
// It uses nothing this change added (NewRunFunc, Executor, NewClient, ExecConfig, RunAssignment decoded
// from JSON, SealBody), so it compiles in the old tree. In THIS tree it would behave differently on
// purpose, which is why it SKIPS unless ARGUS_OLD_EXECUTOR=1. With ARGUS_REQUIRE_OLDEXEC=1 a missing
// ARGUS_OLD_EXECUTOR is a FAILURE, never a skip ("ok" must not be the word for a test that did not run).
//
// Two things are driven, both against the OLD code:
//  1. NewRunFunc, the run core's front door, handed the decoded assignment: it must return an error.
//  2. The OUTER Executor loop (poll -> pickup -> run -> push) against a stub control plane that hands out
//     the same assignment: the loop must push ONE terminal `failed` with a failure_reason and no outcome.
//
// The assignment's body is SEALED (a real SealBody for a real X25519 key, as a control plane holding the
// executor's key sends it), not a clear one: the old tree must refuse it as an unknown field, not because
// the body was plain text.

func compareWireAssignment(t *testing.T, runID string) string {
	t.Helper()
	pub, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := federation.SealBody(pub.PublicKey(), runID, "# Scenario: old")
	if err != nil {
		t.Fatalf("SealBody: %v", err)
	}
	sbJSON, _ := json.Marshal(sb)
	return `{"run_request_id":"rq-old","run_id":"` + runID + `","scope":"full","mode":"compare","set_hash":"sh",` +
		`"compare_scenarios":[{"path":"permissions/CHN-OLD.md","sealed":` + string(sbJSON) + `}]}`
}

func TestOldExecutorRefusesACompareAssignment(t *testing.T) {
	if os.Getenv("ARGUS_OLD_EXECUTOR") != "1" {
		if os.Getenv("ARGUS_REQUIRE_OLDEXEC") == "1" {
			t.Fatal("ARGUS_REQUIRE_OLDEXEC=1 but ARGUS_OLD_EXECUTOR is not 1: this test must run in the OLD tree, not be skipped")
		}
		t.Skip("env-gated: runs only in the old-executor worktree (scripts/compare-old-executor-check.sh)")
	}
	var hits int32
	sut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer sut.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: p\ntargets:\n  http:\n    base_url: "+sut.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wire := compareWireAssignment(t, "run-old")
	if !strings.Contains(wire, `"sealed"`) || strings.Contains(wire, `"body"`) {
		t.Fatalf("the test assignment must carry a sealed body and no clear one: %s", wire)
	}

	// 1) the run core's front door
	var a federation.RunAssignment
	if err := json.Unmarshal([]byte(wire), &a); err != nil {
		t.Fatalf("the OLD tree could not even decode the assignment: %v", err)
	}
	if len(a.Scenarios) != 0 {
		t.Fatalf("the old tree decoded %d ordinary scenarios from a compare assignment: the set must ride compare_scenarios only", len(a.Scenarios))
	}
	cfg := ExecConfig{ConfigPath: cfgPath, ResultsRoot: filepath.Join(dir, "results")}
	push, err := NewRunFunc(cfg)(context.Background(), &a)
	t.Logf("OLD NewRunFunc: err=%v push.status=%q push.total=%d sut_hits=%d", err, push.Status, push.Tallies.Total, atomic.LoadInt32(&hits))
	if err == nil {
		t.Errorf("the old executor ran a compare assignment to completion: %+v", push.Tallies)
	}
	if push.Tallies.Total != 0 || push.Outcome != "" {
		t.Errorf("the old executor produced a result for a compare assignment: %+v", push)
	}

	// 2) the OUTER Executor loop against a stub control plane that hands out the same assignment once
	var (
		mu      sync.Mutex
		pushes  []json.RawMessage
		polled  int
		gotFail = make(chan struct{})
		once    sync.Once
	)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fed/poll":
			mu.Lock()
			polled++
			first := polled == 1
			mu.Unlock()
			if first {
				_, _ = w.Write([]byte(`{"has_run":true,"run":` + wire + `,"versions":{}}`))
				return
			}
			_, _ = w.Write([]byte(`{"has_run":false,"versions":{}}`))
		case "/fed/results":
			mu.Lock()
			pushes = append(pushes, json.RawMessage(body))
			mu.Unlock()
			var p struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(body, &p) == nil && p.Status == "failed" {
				once.Do(func() { close(gotFail) })
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer cp.Close()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ex := &Executor{
		Client: NewClient(cp.URL, "inst-old", priv), Run: NewRunFunc(cfg), RunnerVersion: "0.3.56",
		PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond, PushRetries: 1, HeartbeatEvery: time.Hour,
		Log: func(f string, a ...any) { t.Logf("executor: "+f, a...) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()
	select {
	case <-gotFail:
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatalf("the OLD executor loop never pushed a terminal failed for the compare assignment (pushes: %d)", len(pushes))
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	var terminal []federation.ResultsPush
	for _, raw := range pushes {
		var p federation.ResultsPush
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("a push the old executor sent does not decode: %v", err)
		}
		if p.Status != "running" {
			terminal = append(terminal, p)
		}
	}
	if len(terminal) != 1 {
		t.Fatalf("want exactly one terminal push, got %d: %s", len(terminal), pushes)
	}
	p := terminal[0]
	t.Logf("OLD EXECUTOR LOOP: terminal push status=%q failure_reason=%q outcome=%q scenarios=%d total=%d run_id=%s sut_hits=%d",
		p.Status, p.FailureReason, p.Outcome, len(p.Scenarios), p.Tallies.Total, p.RunID, atomic.LoadInt32(&hits))
	if p.Status != "failed" || strings.TrimSpace(p.FailureReason) == "" {
		t.Errorf("want a terminal failed push WITH a failure_reason, got status %q reason %q", p.Status, p.FailureReason)
	}
	if p.Outcome != "" || p.Tallies.Total != 0 || len(p.Scenarios) != 0 {
		t.Errorf("the old executor produced an outcome or results for a compare assignment: %+v", p)
	}
	if p.RunID != "run-old" {
		t.Errorf("the failed push closes run %q, want run-old", p.RunID)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("the old executor fired %d request(s) at the SUT", n)
	}
}

package runner

// ownwords_diagnosis_test.go — AC-F1's other half: Argus's OWN words stay. #679: "keep the author able to diagnose …
// which step of the run failed and why, in Argus's terms. Only text received from the app is removed." The rule is
// fail-closed, so a site that stops vouching for its text silently turns into the fixed sentence; these pin every
// run-path refusal and failure the executor can name, the not-wired answer, and the local log that keeps what is
// withheld from the wire.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/mcpserver"
	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

func TestNewRunFunc_ArgussOwnWordsStayOnTheWire(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingCfg := filepath.Join(t.TempDir(), "no-such-config.yaml")
	clear := []federation.ScenarioPayload{{Path: "a/S1.md", Body: "# S1"}}
	for _, c := range []struct {
		name  string
		cfg   ExecConfig
		a     *federation.RunAssignment
		stub  bool     // the run core writes no report (so the read-back fails on the file system)
		parts []string // Argus's words the failure reason must carry
	}{
		{"a final run with no digest", ExecConfig{}, &federation.RunAssignment{Mode: "final", RunID: "r1"},
			false, []string{"refused: a final run requires an artifact digest"}},
		{"an unsealed scheduled run", ExecConfig{}, &federation.RunAssignment{Mode: "scheduled", ArtifactDigest: mdA, RunID: "r2", Scenarios: clear},
			false, []string{"refused: a scheduled run's scenarios must arrive sealed"}},
		{"an unsealed compare run", ExecConfig{}, &federation.RunAssignment{Mode: "compare", RunID: "r3", CompareScenarios: clear},
			false, []string{"refused: a compare run's scenarios must arrive sealed"}},
		{"a declared digest that is not running", ExecConfig{MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}, nil)},
			&federation.RunAssignment{Mode: "final", ArtifactDigest: mdC, RunID: "r4"},
			false, []string{"refused", mdC, mdA}},
		{"a results directory that cannot be made", ExecConfig{ResultsRoot: notDir}, &federation.RunAssignment{RunID: "r5", Scenarios: clear},
			false, []string{"materialize: ", notDir}},
		{"a config that does not load", ExecConfig{ConfigPath: missingCfg}, &federation.RunAssignment{RunID: "r6", Scenarios: clear},
			false, []string{"run: load config: ", missingCfg}},
		{"a run core that left no report", ExecConfig{}, &federation.RunAssignment{RunID: "r7", Scenarios: clear},
			true, []string{"read report: ", "report.json"}},
		{"a report that does not decode (it quotes the app's number)", ExecConfig{}, &federation.RunAssignment{RunID: "r8", Scenarios: clear},
			true, []string{"read report: " + reportNotDecodable}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.stub {
				damaged := c.a.RunID == "r8"
				prev := toolcoreRun
				toolcoreRun = func(e toolcore.Env, _, _, _, _ string) (any, bool, error) {
					if damaged {
						return nil, false, writeTestReport(e, []byte(`{"summary":{"passed":`+appNumber+`}}`))
					}
					return nil, false, nil
				}
				t.Cleanup(func() { toolcoreRun = prev })
			}
			if c.cfg.ResultsRoot == "" {
				c.cfg.ResultsRoot = t.TempDir()
			}
			t.Setenv("ARGUS_SUT_NAMESPACE", "")
			_, err := NewRunFunc(c.cfg)(context.Background(), c.a)
			if err == nil {
				t.Fatal("the run did not fail")
			}
			got := failureReason(err)
			if got == reasonNotOwnWords {
				t.Fatalf("Argus's own words were replaced by the fixed sentence — this site no longer vouches for its text: %v", localDetail(err))
			}
			for _, p := range c.parts {
				if !strings.Contains(got, p) {
					t.Errorf("the failure reason %q lost %q", got, p)
				}
			}
			if got != err.Error() {
				t.Errorf("Error() of these errors must be their words: wire %q, Error() %q", got, err.Error())
			}
			if strings.Contains(got, appNumber) {
				t.Errorf("the app's number reached the failure reason: %q", got)
			}
			if c.a.RunID == "r8" && !strings.Contains(localDetail(err), appNumber) {
				t.Errorf("positive control: the local detail must still quote the decoder: %q", localDetail(err))
			}
		})
	}
}

// capturedLog collects an executor's log lines.
type capturedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *capturedLog) log(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmtLine(f, a...))
}

func (l *capturedLog) has(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

// The two run-error shapes the field sites must handle: one that withholds the app's text (its words go, the log
// keeps it all) and a plain one nobody vouched for (the fixed sentence goes — never its text).
var runErrorShapes = []struct {
	name     string
	err      func() error
	wantSent string
}{
	{"withheld", func() error {
		return ownWordsf("read report: %w", withhold(reportNotDecodable, errors.New(appMarker)))
	}, "read report: " + reportNotDecodable},
	{"unvouched", func() error { return errors.New(appMarker + " the app failed") }, reasonNotOwnWords},
}

// loopFailure runs one assignment through Executor.Loop with a run function that fails with runErr, and returns the
// failure reason the control plane received and the executor's own log.
func loopFailure(t *testing.T, runErr error) (string, *capturedLog) {
	t.Helper()
	var (
		mu     sync.Mutex
		polled bool
		failed *federation.ResultsPush
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/fed/poll", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		first := !polled
		polled = true
		mu.Unlock()
		if first {
			_ = json.NewEncoder(w).Encode(federation.PollResponse{HasRun: true, Run: &federation.RunAssignment{RunID: "run-w", RunRequestID: "rr-w", Scope: "full"}})
			return
		}
		_ = json.NewEncoder(w).Encode(federation.PollResponse{})
	})
	mux.HandleFunc("/fed/results", func(w http.ResponseWriter, r *http.Request) {
		var p federation.ResultsPush
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p.Status == "failed" {
			mu.Lock()
			failed = &p
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	lg := &capturedLog{}
	ex := &Executor{Client: tallyTestClient(t, srv.URL), PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond,
		HeartbeatEvery: time.Hour, PushRetries: 1, Log: lg.log,
		Run: func(context.Context, *federation.RunAssignment) (federation.ResultsPush, error) {
			return federation.ResultsPush{}, runErr
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := failed != nil
		mu.Unlock()
		if got {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if failed == nil {
		t.Fatal("no failed push arrived")
	}
	return failed.FailureReason, lg
}

func TestLoop_ARunErrorIsSentAsItsWordsAndLoggedWhole(t *testing.T) {
	for _, c := range runErrorShapes {
		t.Run(c.name, func(t *testing.T) {
			sent, lg := loopFailure(t, c.err())
			if sent != c.wantSent {
				t.Errorf("failure reason = %q, want %q", sent, c.wantSent)
			}
			if !lg.has("run failed: ") || !lg.has(appMarker) {
				t.Errorf("the executor's own log must keep the whole error: %q", lg.lines)
			}
		})
	}
}

func TestDirectRun_ARunErrorIsSentAsItsWordsAndLoggedWhole(t *testing.T) {
	for _, c := range runErrorShapes {
		t.Run(c.name, func(t *testing.T) {
			rec := &pushRecorder{set: federation.MaterializeResponse{Scope: "full", SetHash: "h", Scenarios: []federation.ScenarioPayload{{Path: "a/S1.md", Body: "# S1"}}}}
			srv := httptest.NewServer(rec.handler())
			defer srv.Close()
			lg := &capturedLog{}
			runErr := c.err()
			e := &Executor{Client: tallyTestClient(t, srv.URL), HeartbeatEvery: time.Hour, Log: lg.log,
				Run: func(context.Context, *federation.RunAssignment) (federation.ResultsPush, error) {
					return federation.ResultsPush{}, runErr
				}}
			if _, _, err := e.DirectRun(context.Background(), filepath.Join(t.TempDir(), "cache.json"), "full", federation.Selection{}, 5*time.Second); err == nil {
				t.Fatal("the direct run did not fail")
			}
			pushes, _ := rec.pushes(t)
			var reason string
			for _, p := range pushes {
				if p.Status == "failed" {
					reason = p.FailureReason
				}
			}
			if reason != c.wantSent {
				t.Errorf("abort reason = %q, want %q", reason, c.wantSent)
			}
			if !lg.has(appMarker) {
				t.Errorf("the direct run must say the whole error in its own output: %q", lg.lines)
			}
		})
	}
}

// A relayed command whose error withholds something, or that nobody vouched for: the control plane gets the words or
// the fixed sentence, the log gets it all; an executor with no command function answers with Argus's own sentence.
func TestRunCommands_WordsOnTheWireTheWholeErrorInTheLog(t *testing.T) {
	var (
		mu  sync.Mutex
		got = map[string]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var res federation.CommandResult
		_ = json.NewDecoder(r.Body).Decode(&res)
		mu.Lock()
		got[res.CommandID] = res.Error
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	lg := &capturedLog{}
	e := &Executor{Client: tallyTestClient(t, srv.URL), Log: lg.log,
		Commands: func(_ context.Context, verb string, _ json.RawMessage) (json.RawMessage, error) {
			if verb == "get_report" {
				return nil, withhold(reportNotDecodable, errors.New(appMarker))
			}
			return nil, errors.New(appMarker + " the app failed")
		}}
	e.runCommands(context.Background(), []federation.CommandEnvelope{{CommandID: "c1", Verb: "get_report"}, {CommandID: "c3", Verb: "validate_config"}})
	(&Executor{Client: e.Client}).runCommands(context.Background(), []federation.CommandEnvelope{{CommandID: "c2", Verb: "get_report"}})
	mu.Lock()
	defer mu.Unlock()
	if got["c1"] != reportNotDecodable {
		t.Errorf("c1 sent %q", got["c1"])
	}
	if got["c3"] != commandNotOwnWords {
		t.Errorf("an error nobody vouched for must be sent as Argus's sentence, c3 sent %q", got["c3"])
	}
	if !lg.has(appMarker) {
		t.Errorf("the executor's own log must keep what was withheld: %q", lg.lines)
	}
	if got["c2"] != "this executor is not wired to answer relayed commands" {
		t.Errorf("an executor with no command function must answer with Argus's own sentence, sent %q", got["c2"])
	}
}

// hubAuth resolves one static bearer to the executor hat, standing in for the hub (the AC-16 TransportMCP path).
type hubAuth struct{ token string }

func (h hubAuth) Authenticate(tok string) (mcpserver.Principal, error) {
	if tok != h.token {
		return mcpserver.Principal{}, errors.New("hub: unrecognized bearer")
	}
	return mcpserver.Principal{Hat: role.Executor}, nil
}

// The hub transport (tools/call executor__<verb>) carries the same words as the direct one.
func TestHubTransport_CarriesOnlyTheWords(t *testing.T) {
	var (
		mu      sync.Mutex
		reasons []string
		errs    = map[string]string{}
	)
	_, priv, _ := ed25519.GenerateKey(nil)
	pushArgs := toArgs(federation.ResultsPush{RunID: "r", Status: "failed", FailureReason: "x"})
	pushProps := map[string]any{"envelope": map[string]any{"type": "string"}}
	for k := range pushArgs {
		pushProps[k] = map[string]any{}
	}
	tools := []mcpserver.Tool{
		{Name: "executor__command_result", Namespace: mcpserver.NSExecutor, Description: "test",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"envelope": map[string]any{"type": "string"}, "command_id": map[string]any{"type": "string"},
				"result": map[string]any{"type": "object"}, "error": map[string]any{"type": "string"}}},
			Handler: func(args json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
				var a federation.CommandResult
				_ = json.Unmarshal(args, &a)
				mu.Lock()
				errs[a.CommandID] = a.Error
				mu.Unlock()
				return mcpserver.Ok(map[string]any{"ok": true})
			}},
		{Name: "executor__results", Namespace: mcpserver.NSExecutor, Description: "test",
			InputSchema: map[string]any{"type": "object", "properties": pushProps},
			Handler: func(args json.RawMessage, _ mcpserver.Principal) mcpserver.Outcome {
				var p federation.ResultsPush
				_ = json.Unmarshal(args, &p)
				mu.Lock()
				reasons = append(reasons, p.FailureReason)
				mu.Unlock()
				return mcpserver.Ok(map[string]any{"ok": true})
			}},
	}
	srv, err := mcpserver.NewServerWithAuth(hubAuth{token: "hub-tok"}, tools...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpserver.NewHTTPHandler(srv))
	defer ts.Close()
	c := NewMCPClient(ts.URL+"/mcp", "hub-tok", "inst-hub", priv)
	ctx := context.Background()
	if err := c.CommandResult(ctx, "c1", nil, withhold(reportNotDecodable, errors.New(appMarker))); err != nil {
		t.Fatalf("the hub refused the command answer: %v", err)
	}
	_ = c.CommandResult(ctx, "c2", nil, errors.New(appMarker))
	withheld := ownWordsf("read report: %w", withhold(reportNotDecodable, errors.New(appMarker)))
	if err := c.Push(ctx, federation.ResultsPush{RunID: "r1", Scope: "full", Status: "failed", FailureReason: failureReason(withheld)}); err != nil {
		t.Fatalf("the hub refused the push: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if errs["c1"] != reportNotDecodable || errs["c2"] != commandNotOwnWords {
		t.Errorf("over the hub the command errors went out as %q / %q", errs["c1"], errs["c2"])
	}
	if len(reasons) != 1 || reasons[0] != "read report: "+reportNotDecodable {
		t.Errorf("over the hub the failure reason went out as %q", reasons)
	}
}

func fmtLine(f string, a ...any) string { return strings.TrimSpace(fmt.Sprintf(f, a...)) }

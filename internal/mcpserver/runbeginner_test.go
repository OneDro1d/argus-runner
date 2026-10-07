package mcpserver

// runbeginner_test.go — the runner__run CP-fence hook (W1 direct path, M3.1 §D-3.1.2/UC188;
// CP-M3-115). TEST-FIRST. Contract:
//   - Server.RunBeginner (nil by default → standalone behavior unchanged) is called SYNCHRONOUSLY
//     before the async run dispatch, with the minted run_id + the computed scope.
//   - A beginner error REFUSES the run: the caller gets the distinguished run-in-progress RPC error,
//     the LOCAL lock is released (a later permitted run proceeds), and the handler NEVER runs.
//   - Server.RunDone is called EXACTLY ONCE when the background handler returns — ok=false on an
//     ERROR outcome (infra/tool error — no report push happened → the wrapper must release the CP
//     lock), ok=true on a clean outcome (the wrapper only stops its heartbeat ticker).

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// runTool returns a runner__run fake whose handler records invocation and returns outcome.
func runTool(mu *sync.Mutex, ran *[]string, outcome func() Outcome) Tool {
	return Tool{Name: "runner__run", Namespace: NSRunner, Async: true, Description: "run", InputSchema: sch(),
		Handler: func(args json.RawMessage, _ Principal) Outcome {
			var a struct {
				RunID string `json:"run_id"`
			}
			_ = json.Unmarshal(args, &a)
			mu.Lock()
			*ran = append(*ran, a.RunID)
			mu.Unlock()
			return outcome()
		}}
}

func callRun(t *testing.T, s *Server, sid string, args map[string]any) Response {
	t.Helper()
	if args == nil {
		args = map[string]any{"instance_id": "local"}
	}
	raw, ok := s.Dispatch(sid, rtok, reqBytes(9, "tools/call", map[string]any{"name": "runner__run", "arguments": args}))
	if !ok {
		t.Fatalf("no response")
	}
	return decode(t, raw)
}

func TestRunBeginner_BusyRefusesAndReleasesLocalLock(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	s := newSrv(t, runTool(&mu, &ran, func() Outcome { return Ok(map[string]any{"ok": true}) }))
	handshake(t, s, "sid-b", rtok)

	var beginCalls []string
	busy := errors.New("instance busy — a run is already in flight for this instance")
	s.RunBeginner = func(runID, scope string) error {
		beginCalls = append(beginCalls, runID+"/"+scope)
		return busy
	}

	resp := callRun(t, s, "sid-b", map[string]any{"instance_id": "local", "layer": "http-ingestion"})
	if resp.Error == nil {
		t.Fatalf("busy beginner: got success %+v; want the distinguished run-in-progress error", resp.Result)
	}
	if resp.Error.Code != CodeRunInProgress {
		t.Fatalf("busy error code = %d; want CodeRunInProgress", resp.Error.Code)
	}
	if !strings.Contains(strings.ToLower(resp.Error.Message), "busy") {
		t.Fatalf("busy error message %q; want it to say busy", resp.Error.Message)
	}
	if len(beginCalls) != 1 || !strings.HasSuffix(beginCalls[0], "/layer") {
		t.Fatalf("beginner calls = %v; want one call with scope layer", beginCalls)
	}
	mu.Lock()
	n := len(ran)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("handler ran despite the busy refusal")
	}

	// The LOCAL lock must have been released: with the beginner now allowing, the run proceeds.
	s.RunBeginner = func(runID, scope string) error { return nil }
	resp = callRun(t, s, "sid-b", nil)
	if resp.Error != nil {
		t.Fatalf("run after busy-release: %+v — the local lock leaked", resp.Error)
	}
}

func TestRunBeginner_NilKeepsStandaloneBehavior(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	s := newSrv(t, runTool(&mu, &ran, func() Outcome { return Ok(map[string]any{"ok": true}) }))
	handshake(t, s, "sid-n", rtok)
	resp := callRun(t, s, "sid-n", nil)
	if resp.Error != nil {
		t.Fatalf("nil beginner run: %+v; want async success", resp.Error)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(ran) == 1 })
}

func TestRunDone_OKFalseOnErrorOutcome_OKTrueOnClean(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	type doneCall struct {
		runID string
		ok    bool
	}
	var done []doneCall
	fail := true
	s := newSrv(t, runTool(&mu, &ran, func() Outcome {
		if fail {
			return ToolErr(map[string]any{"observed": "infra boom"})
		}
		return Ok(map[string]any{"ok": true})
	}))
	handshake(t, s, "sid-a", rtok)
	s.RunBeginner = func(runID, scope string) error { return nil }
	s.RunDone = func(runID string, ok bool) { mu.Lock(); done = append(done, doneCall{runID, ok}); mu.Unlock() }

	// error outcome → RunDone(runID, false) with the SAME run_id the handler got
	resp := callRun(t, s, "sid-a", nil)
	if resp.Error != nil {
		t.Fatalf("async dispatch errored synchronously: %+v", resp.Error)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(done) == 1 })
	mu.Lock()
	if len(ran) != 1 || done[0].runID != ran[0] || ran[0] == "" || done[0].ok {
		t.Fatalf("done=%v ran=%v; want RunDone(handler's run_id, ok=false) exactly once", done, ran)
	}
	mu.Unlock()

	// clean outcome → RunDone(runID, true)
	fail = false
	_ = callRun(t, s, "sid-a", nil)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(done) == 2 })
	time.Sleep(50 * time.Millisecond) // let any duplicate fire
	mu.Lock()
	if len(done) != 2 || !done[1].ok || done[1].runID != ran[1] {
		t.Fatalf("done=%v; want a second RunDone(run_id, ok=true) and no duplicates", done)
	}
	mu.Unlock()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within 3s")
}

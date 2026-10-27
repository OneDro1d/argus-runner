package runner

// heartbeat_tally_test.go -- the mid-run heartbeat carries the counts so far, and ONLY counts. Hermetic: an httptest
// control plane, no database, no JMeter.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// pushRecorder is a control plane that records every body POSTed to /fed/results, answers the fence and the
// materialize, and nothing else.
type pushRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
	set    federation.MaterializeResponse
}

func (p *pushRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/fed/results":
			p.mu.Lock()
			p.bodies = append(p.bodies, b)
			p.mu.Unlock()
		case "/fed/materialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p.set)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// pushes decodes what has been recorded so far, with the raw bytes beside it.
func (p *pushRecorder) pushes(t *testing.T) ([]federation.ResultsPush, [][]byte) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []federation.ResultsPush
	for _, b := range p.bodies {
		var rp federation.ResultsPush
		if err := json.Unmarshal(b, &rp); err != nil {
			t.Fatalf("a recorded push does not decode: %v: %s", err, b)
		}
		out = append(out, rp)
	}
	return out, append([][]byte(nil), p.bodies...)
}

// waitFor polls until a recorded push satisfies ok, or fails the test after 10 s.
func (p *pushRecorder) waitFor(t *testing.T, what string, ok func(federation.ResultsPush) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ps, _ := p.pushes(t)
		for _, rp := range ps {
			if ok(rp) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	ps, raws := p.pushes(t)
	t.Fatalf("no push satisfied %q within 10 s; %d recorded, last: %s", what, len(ps), lastOf(raws))
}

// writeTestReport writes b where NewRunFunc reads the in-env report.
func writeTestReport(e toolcore.Env, b []byte) error {
	dir := filepath.Join(e.ResultsRoot, e.Instance)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
}

func lastOf(raws [][]byte) string {
	if len(raws) == 0 {
		return "(none)"
	}
	return string(raws[len(raws)-1])
}

func tallyTestClient(t *testing.T, url string) *Client {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	return NewClient(url, "inst-tally", priv)
}

// A slow run, a short HeartbeatEvery: a captured heartbeat carries the counts so far and `planned`, an EMPTY scenario
// list, and not one check id anywhere in its bytes. Before the first check finishes it carries zeros and `planned`.
func TestHeartbeat_CarriesCountsOnly(t *testing.T) {
	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	e := &Executor{Client: tallyTestClient(t, srv.URL), HeartbeatEvery: 10 * time.Millisecond}

	tl := argus.NewTally()
	tl.SetPlanned(5)
	stop := e.startHeartbeatTally(context.Background(), "run-hb", "rr-hb", "full", tl)

	// nothing finished yet: zeros and planned
	rec.waitFor(t, "a heartbeat with zero counts and planned 5", func(p federation.ResultsPush) bool {
		return p.Status == "running" && p.Tallies.Total == 0 && p.Tallies.Planned == 5
	})
	// the run (slowly) finishes two checks, ids a canary
	tl.Finished(report.ScenarioResult{ID: "CHK-CANARY-001", Status: "passed", Failure: &report.Failure{Observed: "OBSERVED-CANARY"}})
	tl.Finished(report.ScenarioResult{ID: "CHK-CANARY-002", Status: "failed"})
	rec.waitFor(t, "a heartbeat with passed 1, failed 1, total 2, planned 5", func(p federation.ResultsPush) bool {
		return p.Tallies.Passed == 1 && p.Tallies.Failed == 1 && p.Tallies.Errored == 0 && p.Tallies.Total == 2 && p.Tallies.Planned == 5
	})
	stop()

	pushes, raws := rec.pushes(t)
	for i, p := range pushes {
		if p.Status != "running" || p.RunID != "run-hb" || p.RunRequestID != "rr-hb" {
			t.Errorf("push %d is not the run's heartbeat: %+v", i, p)
		}
		if len(p.Scenarios) != 0 {
			t.Errorf("heartbeat %d carries a scenario list (%d entries): the control plane would stamp last_run_outcome from it", i, len(p.Scenarios))
		}
		if p.Tallies.Passed+p.Tallies.Failed+p.Tallies.Errored+p.Tallies.Degraded != p.Tallies.Total {
			t.Errorf("heartbeat %d counts do not add up to its total: %+v", i, p.Tallies)
		}
		raw := string(raws[i])
		for _, banned := range []string{"CHK-CANARY", "OBSERVED-CANARY", `"outcome"`, `"summary"`, `"evidence_hash"`} {
			if strings.Contains(raw, banned) {
				t.Errorf("heartbeat %d carries %s: %s", i, banned, raw)
			}
		}
	}
}

// No tally is the old heartbeat: four zeros and no `planned` key at all.
func TestHeartbeat_NilTallyIsTheOldHeartbeat(t *testing.T) {
	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	c := tallyTestClient(t, srv.URL)
	if err := c.PushHeartbeat(context.Background(), "run-old", "", "full", time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	_, raws := rec.pushes(t)
	if len(raws) != 1 {
		t.Fatalf("recorded %d pushes", len(raws))
	}
	if !strings.Contains(string(raws[0]), `"tallies":{"passed":0,"failed":0,"errored":0,"total":0}`) || strings.Contains(string(raws[0]), "planned") {
		t.Fatalf("the nil-tally heartbeat changed: %s", raws[0])
	}
}

// The direct FENCED path carries the counts too: DirectRun starts the heartbeat after run/begin, hands the same
// Tally to the run through the context, and the run (here a stub standing in for the core) reports into it.
func TestDirectRun_FencedHeartbeatCarriesTheCounts(t *testing.T) {
	rec := &pushRecorder{set: federation.MaterializeResponse{Scope: "full", SetHash: "h1",
		Scenarios: []federation.ScenarioPayload{{Path: "a/S1.md", Body: "# S1"}}}}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()
	e := &Executor{Client: tallyTestClient(t, srv.URL), HeartbeatEvery: 10 * time.Millisecond}
	e.Run = func(ctx context.Context, a *federation.RunAssignment) (federation.ResultsPush, error) {
		tl := tallyFrom(ctx)
		if tl == nil {
			t.Error("the direct fenced run was given no Tally")
			return federation.ResultsPush{}, nil
		}
		tl.SetPlanned(4)
		tl.Finished(report.ScenarioResult{ID: "D-1", Status: "passed"})
		tl.Finished(report.ScenarioResult{ID: "D-2", Status: report.StatusError})
		rec.waitFor(t, "a direct heartbeat with passed 1, errored 1, total 2, planned 4", func(p federation.ResultsPush) bool {
			return p.Status == "running" && p.RunID == a.RunID && p.Tallies.Passed == 1 && p.Tallies.Errored == 1 && p.Tallies.Total == 2 && p.Tallies.Planned == 4
		})
		return federation.ResultsPush{RunID: a.RunID, Scope: a.Scope, Status: "completed", Tallies: federation.Tallies{Passed: 1, Errored: 1, Total: 2}}, nil
	}
	push, _, err := e.DirectRun(context.Background(), filepath.Join(t.TempDir(), "cache.json"), "full", federation.Selection{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if push.Tallies.Planned != 0 {
		t.Errorf("the final push carries planned=%d", push.Tallies.Planned)
	}
	pushes, raws := rec.pushes(t)
	last := pushes[len(pushes)-1]
	if last.Status != "completed" || strings.Contains(string(raws[len(raws)-1]), "planned") {
		t.Errorf("the last push is not the plain final push: %s", raws[len(raws)-1])
	}
	for i, p := range pushes {
		if p.Status == "running" && len(p.Scenarios) != 0 {
			t.Errorf("direct heartbeat %d carries a scenario list", i)
		}
	}
}

// The federated run function hands the context's Tally to the core as Env.Tally, and nothing when there is none.
func TestNewRunFunc_HandsTheContextTallyToTheCore(t *testing.T) {
	var seen *argus.Tally
	var calls int
	prev := toolcoreRun
	toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
		calls++
		seen = e.Tally
		rep := report.Report{RunID: runID, Summary: report.Summary{Passed: 1, Total: 1}}
		b, _ := json.Marshal(rep)
		return nil, true, writeTestReport(e, b)
	}
	t.Cleanup(func() { toolcoreRun = prev })

	a := &federation.RunAssignment{RunID: "run-ctx", RunRequestID: "rr-1", Scope: "full", SetHash: "h",
		Scenarios: []federation.ScenarioPayload{{Path: "a/S1.md", Body: "# S1"}}}
	run := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir()})

	tl := argus.NewTally()
	if _, err := run(withTally(context.Background(), tl), a); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || seen != tl {
		t.Fatalf("the core was given Env.Tally=%p, want the context's %p", seen, tl)
	}
	if _, err := run(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || seen != nil {
		t.Fatalf("with no tally on the context the core was given %p; want nil", seen)
	}
}

// The FINAL push for a fixed report is byte-identical to what it was before `planned` existed: the golden below was
// produced by this same test at the base commit (3f286ac5), and it names no `planned` key.
func TestFinalPush_TalliesAreByteIdenticalToTheBase(t *testing.T) {
	rep := &report.Report{Project: "p", Summary: report.Summary{
		Total: 6, Passed: 2, Failed: 1, Errored: 2, Degraded: 1, RateLimitedScenarios: 1, RateLimitPauses: 3}}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	push := mapReport(rep, "run-final", "rr-f", "full", "sethash", "", federation.Annotations{}, t0, t0.Add(time.Minute), "")
	got, err := json.Marshal(push.Tallies)
	if err != nil {
		t.Fatal(err)
	}
	const golden = `{"passed":2,"failed":1,"errored":2,"total":6,"rate_limited":1,"rate_limit_pauses":3,"degraded":1}`
	if string(got) != golden {
		t.Fatalf("final tallies bytes changed:\n got %s\nwant %s", got, golden)
	}
	whole, _ := json.Marshal(push)
	if strings.Contains(string(whole), "planned") {
		t.Fatalf("the final push carries planned: %s", whole)
	}
	// the WHOLE push, as the base commit marshals it (captured by running the same mapReport call there)
	const wholeGolden = `{"run_id":"run-final","run_request_id":"rr-f","scope":"full","status":"failed","tallies":{"passed":2,"failed":1,"errored":2,"total":6,"rate_limited":1,"rate_limit_pauses":3,"degraded":1},"scenarios":null,"annotations":{},"set_hash":"sethash","started_at":"2026-10-10T12:00:00Z","finished_at":"2026-10-10T12:01:00Z","duration_ms":60000,"evidence_bundle_hash":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","outcome":"failed"}`
	if string(whole) != wholeGolden {
		t.Fatalf("the final push bytes changed:\n got %s\nwant %s", whole, wholeGolden)
	}
}

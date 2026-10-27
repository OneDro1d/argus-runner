package argus

// tally_test.go -- the run's live Tally (the executor's mid-run heartbeat reads it). Hermetic: no database, no network
// beyond an httptest server. The REAL run loop (RunAllWithTally) over real scenario files.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// tallyRows is a fixed mix of finished rows: every status summarize knows, one rate-limited errored row.
func tallyRows() []report.ScenarioResult {
	return []report.ScenarioResult{
		{ID: "A", Status: "passed"},
		{ID: "B", Status: "failed"},
		{ID: "C", Status: report.StatusError},
		{ID: "D", Status: report.StatusError, RateLimited: true},
		{ID: "E", Status: report.StatusDegraded},
		{ID: "F", Status: "skipped"},
		{ID: "G", Status: "passed"},
	}
}

// After each finished check the live counts match what summarize would count for exactly those rows: the status-to-count
// mapping is ONE function, so the mid-run numbers and the final ones cannot disagree about what a status means.
func TestTally_CountsAfterEachCheckMatchSummarize(t *testing.T) {
	rows := tallyRows()
	tl := NewTally()
	tl.SetPlanned(len(rows))
	if got := tl.Snapshot(); got != (TallySnapshot{Planned: len(rows)}) {
		t.Fatalf("before the first check = %+v; want zeros and planned=%d", got, len(rows))
	}
	for i, r := range rows {
		tl.Finished(r)
		want := summarize("p", map[string][]report.ScenarioResult{"x": rows[:i+1]}).Summary
		got := tl.Snapshot()
		if got.Total != want.Total || got.Passed != want.Passed || got.Failed != want.Failed ||
			got.Errored != want.Errored || got.Degraded != want.Degraded {
			t.Fatalf("after %d checks: tally %+v, summarize %+v", i+1, got, want)
		}
		if got.Planned != len(rows) {
			t.Fatalf("planned moved to %d", got.Planned)
		}
	}
	got := tl.Snapshot()
	if got.Passed != 2 || got.Failed != 1 || got.Errored != 2 || got.Degraded != 1 || got.Total != 7 {
		t.Fatalf("final = %+v; want passed 2 failed 1 errored 2 degraded 1 total 7 (one skipped)", got)
	}
}

// A nil Tally is the old behaviour: every method is a no-op and a read is all zeros.
func TestTally_NilIsANoOp(t *testing.T) {
	var tl *Tally
	tl.SetPlanned(5)
	tl.Finished(report.ScenarioResult{Status: "passed"})
	if got := tl.Snapshot(); got != (TallySnapshot{}) {
		t.Fatalf("nil tally read %+v; want zeros", got)
	}
}

// One writer and one reader at once, the heartbeat's shape. Meaningful under -race.
func TestTally_ConcurrentReaderWhileTheLoopWrites(t *testing.T) {
	tl := NewTally()
	tl.SetPlanned(2000)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				s := tl.Snapshot()
				if s.Passed+s.Failed > s.Total {
					t.Errorf("incoherent snapshot %+v", s)
					return
				}
			}
		}
	}()
	for i := 0; i < 2000; i++ {
		st := "passed"
		if i%3 == 0 {
			st = "failed"
		}
		tl.Finished(report.ScenarioResult{Status: st})
	}
	close(stop)
	wg.Wait()
	if s := tl.Snapshot(); s.Total != 2000 {
		t.Fatalf("total = %d; want 2000", s.Total)
	}
}

// observingMCPServer wraps the throttling server and records, at every tool call, how many checks the tally says
// are FINISHED at that moment: the counts as the loop grows them.
func observingMCPServer(srv *throttlingMCPServer, tl *Tally, seen *[]TallySnapshot, mu *sync.Mutex) http.HandlerFunc {
	inner := srv.handler()
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(b, &req)
		if req.Method == "tools/call" {
			mu.Lock()
			*seen = append(*seen, tl.Snapshot())
			mu.Unlock()
		}
		inner(w, r)
	}
}

// A check re-fired after a rate-limit pause is counted ONCE, with the row that is reported: MCP-001 is throttled
// on its first call and passes on the re-fire, so when MCP-002 fires one check is finished (not two), and the run
// ends with two.
func TestRunAllWithTally_ARefiredCheckIsCountedOnce(t *testing.T) {
	os.Unsetenv("MCP_URL")
	fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: 1, retryAfter: 48}
	tl := NewTally()
	var seen []TallySnapshot
	var mu sync.Mutex
	ts := httptest.NewServer(observingMCPServer(srv, tl, &seen, &mu))
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, scDir, "MCP-001")
	writeMCPScenario(t, scDir, "MCP-002")

	rr, err := RunAllWithTally(rlCfg(t, ts.URL, ""), scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{}, nil, "", "", tl)
	if err != nil {
		t.Fatal(err)
	}
	if srv.callCount() != 3 {
		t.Fatalf("tool calls = %d; want 3 (MCP-001, its re-fire, MCP-002)", srv.callCount())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("observed %d tool calls; want 3", len(seen))
	}
	for i, wantTotal := range []int{0, 0, 1} {
		if seen[i].Total != wantTotal || seen[i].Planned != 2 {
			t.Errorf("at tool call %d the tally read %+v; want total %d and planned 2 (the re-fire must not add a second finished check)", i+1, seen[i], wantTotal)
		}
	}
	got := tl.Snapshot()
	if got.Total != 2 || got.Passed != 2 || got.Planned != 2 {
		t.Fatalf("final tally = %+v; want 2 passed of 2 planned", got)
	}
	// and the report's own final counts are the same numbers
	if s := rr.Report.Summary; s.Total != got.Total || s.Passed != got.Passed || s.Errored != got.Errored || s.Failed != got.Failed {
		t.Fatalf("report summary %+v disagrees with the tally %+v", s, got)
	}
}

// Throttled twice: the row is errored (rate-limited twice), and it is counted once, as errored; MCP-002 then passes.
func TestRunAllWithTally_AMixOfOutcomesMatchesTheReport(t *testing.T) {
	os.Unsetenv("MCP_URL")
	fakeSleep(t)
	srv := &throttlingMCPServer{throttleFirst: 2, retryAfter: 10}
	tl := NewTally()
	var seen []TallySnapshot
	var mu sync.Mutex
	ts := httptest.NewServer(observingMCPServer(srv, tl, &seen, &mu))
	defer ts.Close()

	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, scDir, "MCP-001")
	writeMCPScenario(t, scDir, "MCP-002")

	rr, err := RunAllWithTally(rlCfg(t, ts.URL, ""), scDir, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{}, nil, "", "", tl)
	if err != nil {
		t.Fatal(err)
	}
	got := tl.Snapshot()
	want := TallySnapshot{Planned: 2, Total: 2, Passed: 1, Errored: 1}
	if got != want {
		t.Fatalf("tally = %+v; want %+v", got, want)
	}
	if s := rr.Report.Summary; s.Total != got.Total || s.Passed != got.Passed || s.Errored != got.Errored {
		t.Fatalf("report summary %+v disagrees with the tally %+v", s, got)
	}
}

// tally nil is RunAllWithTarget exactly: the same loop, the same report.
func TestRunAllWithTally_NilTallyIsTheOldRun(t *testing.T) {
	os.Unsetenv("MCP_URL")
	fakeSleep(t)
	run := func(withHook bool) *report.Report {
		srv := &throttlingMCPServer{throttleFirst: 1, retryAfter: 48}
		ts := httptest.NewServer(srv.handler())
		defer ts.Close()
		dir := t.TempDir()
		scDir := filepath.Join(dir, "scenarios")
		writeMCPScenario(t, scDir, "MCP-001")
		writeMCPScenario(t, scDir, "MCP-002")
		var rr *RunResult
		var err error
		if withHook {
			rr, err = RunAllWithTally(rlCfg(t, ts.URL, ""), scDir, filepath.Join(dir, "results"), "t", "", "", "", "r1", mcpNopRunner{}, nil, "", "", NewTally())
		} else {
			rr, err = RunAllWithTarget(rlCfg(t, ts.URL, ""), scDir, filepath.Join(dir, "results"), "t", "", "", "", "r1", mcpNopRunner{}, nil, "", "")
		}
		if err != nil {
			t.Fatal(err)
		}
		return rr.Report
	}
	a, b := run(false), run(true)
	if !reflect.DeepEqual(a.Summary, b.Summary) {
		t.Fatalf("a hook changed the summary: %+v vs %+v", a.Summary, b.Summary)
	}
}

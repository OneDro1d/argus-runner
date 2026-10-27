package argus

// preconditions_test.go --: the run loop over real scenario files, with the probe's verdict fixed.
// Hermetic: the MCP server is an httptest server; the cluster is never touched (newPreconditionKube is replaced).

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/precondition"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// writeDependentScenario drops an mcp scenario that declares np-in and depends on it.
func writeDependentScenario(t *testing.T, dir, id string, meta string) {
	t.Helper()
	d := filepath.Join(dir, "http-ingestion")
	_ = os.MkdirAll(d, 0o755)
	md := strings.Replace(mcpScenarioMD(), "- **ID**: MCP-001", "- **ID**: "+id+"\n"+meta, 1)
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

const dependsNPIn = "- **Preconditions**: np-in=networkpolicy.ingress_enforced\n- **Depends on**: np-in"

// fixProbe replaces the cluster and the probe: np-in answers `verdict`. probes counts probe invocations.
func fixProbe(t *testing.T, verdict string) *int32 {
	t.Helper()
	var probes int32
	pk, pr := newPreconditionKube, runPrecondition
	newPreconditionKube = func() (precondition.Kube, string, error) { return nil, "argus-inst-x", nil }
	runPrecondition = func(_ context.Context, _ *precondition.Prober, p scenario.Precondition) precondition.Result {
		atomic.AddInt32(&probes, 1)
		r := precondition.Result{ID: p.ID, Kind: p.Kind, Verdict: verdict, Positive: "connected", Negative: "NOT refused: connected although a deny policy selects it"}
		if verdict == precondition.Broken {
			r.Positive, r.Negative = "positive control FAILED: not connected", "not run"
		}
		if verdict == precondition.Holds {
			r.Negative = "not connected in 4 attempts (refused or dropped)"
		}
		return r
	}
	t.Cleanup(func() { newPreconditionKube, runPrecondition = pk, pr })
	return &probes
}

func runDependents(t *testing.T, srvCalls *int) *RunResult {
	t.Helper()
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPServer())
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	writeDependentScenario(t, sc, "NP-DEP-1", dependsNPIn)
	writeDependentScenario(t, sc, "NP-DEP-2", dependsNPIn)
	writeMCPScenario(t, sc, "FREE-1") // depends on nothing
	rr, err := RunAllWithTally(mcpCfg(t, ts.URL), sc, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{}, nil, "", "", NewTally())
	if err != nil {
		t.Fatal(err)
	}
	return rr
}

func statusOf(rr *RunResult) map[string]string {
	m := map[string]string{}
	for _, l := range rr.Report.Layers {
		for _, s := range l.Scenarios {
			m[s.ID] = s.Status
		}
	}
	return m
}

func TestRun_AbsentPreconditionMakesDependentsNotApplicable(t *testing.T) {
	probes := fixProbe(t, precondition.Absent)
	rr := runDependents(t, nil)
	st := statusOf(rr)
	if st["NP-DEP-1"] != report.StatusNotApplicable || st["NP-DEP-2"] != report.StatusNotApplicable {
		t.Fatalf("dependents must be not_applicable: %v", st)
	}
	if st["FREE-1"] != "passed" {
		t.Fatalf("a check with no dependency must run normally: %v", st)
	}
	if *probes != 1 {
		t.Fatalf("one precondition shared by two checks must be probed once, got %d", *probes)
	}
	s := rr.Report.Summary
	if s.NotApplicable != 2 || s.Passed != 1 || s.Failed != 0 || s.Errored != 0 || s.Total != 3 {
		t.Fatalf("summary = %+v", s)
	}
	if s.Passed+s.Failed+s.Errored+s.Degraded+s.NotApplicable != s.Total {
		t.Fatalf("tallies do not sum to total: %+v", s)
	}
	for _, l := range rr.Report.Layers {
		for _, sc := range l.Scenarios {
			if sc.ID != "NP-DEP-1" {
				continue
			}
			if sc.NotApplicable == nil || sc.NotApplicable.Precondition != "np-in" || !strings.Contains(sc.NotApplicable.Negative, "NOT refused") || sc.NotApplicable.Positive == "" {
				t.Fatalf("the row must carry the precondition id and both observations: %+v", sc.NotApplicable)
			}
			if sc.ReqSuccess+sc.ReqFailed+sc.ReqError != 0 {
				t.Fatalf("a not_applicable check must fire nothing at the SUT: %+v", sc)
			}
		}
	}
	if len(rr.Report.Preconditions) != 1 || rr.Report.Preconditions[0].Verdict != precondition.Absent {
		t.Fatalf("the in-env report must record the precondition result: %+v", rr.Report.Preconditions)
	}
	if rr.Report.Failed() {
		t.Fatal("not_applicable must not fail the run")
	}
}

func TestRun_BrokenPreconditionMakesDependentsErroredNeverNotApplicable(t *testing.T) {
	fixProbe(t, precondition.Broken)
	rr := runDependents(t, nil)
	st := statusOf(rr)
	if st["NP-DEP-1"] != report.StatusError || st["NP-DEP-2"] != report.StatusError {
		t.Fatalf("a broken instrument must end its dependents errored: %v", st)
	}
	if rr.Report.Summary.NotApplicable != 0 || rr.Report.Summary.Errored != 2 {
		t.Fatalf("summary = %+v", rr.Report.Summary)
	}
	if !rr.Report.Failed() {
		t.Fatal("errored dependents must still make the run non-green")
	}
}

func TestRun_HoldingPreconditionRunsDependentsNormally(t *testing.T) {
	fixProbe(t, precondition.Holds)
	rr := runDependents(t, nil)
	st := statusOf(rr)
	for _, id := range []string{"NP-DEP-1", "NP-DEP-2", "FREE-1"} {
		if st[id] != "passed" {
			t.Fatalf("%s = %s, want passed: %v", id, st[id], st)
		}
	}
	if rr.Report.Summary.NotApplicable != 0 {
		t.Fatalf("%+v", rr.Report.Summary)
	}
}

// A run with no `Depends on` constructs no prober at all.
func TestRun_NoDependenciesTouchesNoCluster(t *testing.T) {
	pk := newPreconditionKube
	called := false
	newPreconditionKube = func() (precondition.Kube, string, error) { called = true; return nil, "", nil }
	t.Cleanup(func() { newPreconditionKube = pk })
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPServer())
	defer ts.Close()
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	writeMCPScenario(t, sc, "FREE-1")
	rr, err := RunAllWithTally(mcpCfg(t, ts.URL), sc, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{}, nil, "", "", NewTally())
	if err != nil {
		t.Fatal(err)
	}
	if called || len(rr.Report.Preconditions) != 0 || rr.Report.Summary.NotApplicable != 0 {
		t.Fatalf("called=%v report=%+v", called, rr.Report)
	}
}

// Not running in a pod (no ServiceAccount) is a broken instrument, not an absent mechanism.
func TestRun_NoClusterIsBrokenNotAbsent(t *testing.T) {
	pk := newPreconditionKube
	newPreconditionKube = func() (precondition.Kube, string, error) { return nil, "", context.DeadlineExceeded }
	t.Cleanup(func() { newPreconditionKube = pk })
	rr := runDependents(t, nil)
	st := statusOf(rr)
	if st["NP-DEP-1"] != report.StatusError {
		t.Fatalf("%v", st)
	}
}

// Two files declaring one id with different kinds: the set cannot say what the precondition is.
func TestRun_ConflictingDeclarationsEndErrored(t *testing.T) {
	fixProbe(t, precondition.Holds)
	os.Unsetenv("MCP_URL")
	ts := httptest.NewServer(fakeMCPServer())
	defer ts.Close()
	dir := t.TempDir()
	sc := filepath.Join(dir, "scenarios")
	writeDependentScenario(t, sc, "A-1", dependsNPIn)
	writeDependentScenario(t, sc, "A-2", "- **Preconditions**: np-in=networkpolicy.egress_enforced\n- **Depends on**: np-in")
	rr, err := RunAllWithTally(mcpCfg(t, ts.URL), sc, filepath.Join(dir, "results"), "t", "", "", "", "", mcpNopRunner{}, nil, "", "", NewTally())
	if err != nil {
		t.Fatal(err)
	}
	st := statusOf(rr)
	if st["A-1"] != report.StatusError || st["A-2"] != report.StatusError {
		t.Fatalf("%v", st)
	}
}

// The live tally counts not_applicable on its own, exactly as summarize does.
func TestTally_CountsNotApplicable(t *testing.T) {
	rows := []report.ScenarioResult{{ID: "a", Status: "passed"}, {ID: "b", Status: report.StatusNotApplicable}, {ID: "c", Status: report.StatusError}}
	tl := NewTally()
	for _, r := range rows {
		tl.Finished(r)
	}
	got := tl.Snapshot()
	want := summarize("p", map[string][]report.ScenarioResult{"x": rows}).Summary
	if got.NotApplicable != 1 || got.NotApplicable != want.NotApplicable || got.Errored != 1 || got.Total != 3 {
		t.Fatalf("tally %+v summary %+v", got, want)
	}
}

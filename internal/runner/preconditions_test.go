package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

// The precondition tier at the push: not_applicable is its own outcome and its own tally
// (never errored), the run verdict follows the rule, and the precondition results are bound into the
// evidence bundle hash.

func repWith(sum report.Summary, rows ...report.ScenarioResult) *report.Report {
	return &report.Report{RunID: "r", Summary: sum, Layers: []report.Layer{{Layer: "L", Scenarios: rows}}}
}

func mapIt(rep *report.Report) federation.ResultsPush {
	return mapReport(rep, "r", "rr", "full", "sh", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
}

func TestOutcome_NotApplicableIsItsOwnWordNeverErrored(t *testing.T) {
	if got := outcome(report.StatusNotApplicable); got != "not_applicable" {
		t.Fatalf("outcome(not_applicable) = %q: it must not fall into the errored default", got)
	}
	if got := outcome("skipped"); got != "errored" {
		t.Fatalf("outcome(skipped) = %q - the in-env skipped still reaches the CP as errored", got)
	}
	if genericSummary(report.StatusNotApplicable) == genericSummary(report.StatusError) {
		t.Fatal("a not_applicable summary must not read as an execution error")
	}
}

func TestMapReport_NotApplicableTalliesAndScenarioOutcome(t *testing.T) {
	rep := repWith(report.Summary{Total: 3, Passed: 1, NotApplicable: 2},
		report.ScenarioResult{ID: "A", Status: "passed"},
		report.ScenarioResult{ID: "B", Status: report.StatusNotApplicable},
		report.ScenarioResult{ID: "C", Status: report.StatusNotApplicable})
	push := mapIt(rep)
	tl := push.Tallies
	if tl.NotApplicable != 2 || tl.Errored != 0 || tl.Passed != 1 || tl.Failed != 0 {
		t.Fatalf("tallies = %+v", tl)
	}
	if tl.Passed+tl.Failed+tl.Errored+tl.Degraded+tl.NotApplicable != tl.Total {
		t.Fatalf("the buckets do not sum to total: %+v", tl)
	}
	got := map[string]string{}
	for _, s := range push.Scenarios {
		got[s.ID] = s.Outcome
	}
	if got["B"] != "not_applicable" || got["C"] != "not_applicable" || got["A"] != "passed" {
		t.Fatalf("outcomes = %v", got)
	}
}

// The run verdict: N/A never counts as a pass or a fail.
func TestMapReport_RunVerdictRules(t *testing.T) {
	row := func(id, st string) report.ScenarioResult { return report.ScenarioResult{ID: id, Status: st} }
	for _, tc := range []struct {
		name         string
		sum          report.Summary
		rows         []report.ScenarioResult
		status, want string
	}{
		{"every scenario N/A is never passed", report.Summary{Total: 2, NotApplicable: 2},
			[]report.ScenarioResult{row("A", report.StatusNotApplicable), row("B", report.StatusNotApplicable)}, "completed", "not_applicable"},
		{"passed + N/A is passed", report.Summary{Total: 2, Passed: 1, NotApplicable: 1},
			[]report.ScenarioResult{row("A", "passed"), row("B", report.StatusNotApplicable)}, "completed", "passed"},
		{"a failure outranks N/A", report.Summary{Total: 2, Failed: 1, NotApplicable: 1},
			[]report.ScenarioResult{row("A", "failed"), row("B", report.StatusNotApplicable)}, "failed", "failed"},
		{"an errored scenario (broken instrument) outranks N/A", report.Summary{Total: 2, Errored: 1, NotApplicable: 1},
			[]report.ScenarioResult{row("A", report.StatusError), row("B", report.StatusNotApplicable)}, "failed", "failed"},
		{"N/A + degraded stays degraded", report.Summary{Total: 2, Degraded: 1, NotApplicable: 1},
			[]report.ScenarioResult{row("A", report.StatusDegraded), row("B", report.StatusNotApplicable)}, "degraded", "degraded"},
		{"no precondition: unchanged passed", report.Summary{Total: 1, Passed: 1},
			[]report.ScenarioResult{row("A", "passed")}, "completed", "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			push := mapIt(repWith(tc.sum, tc.rows...))
			if push.Status != tc.status || push.Outcome != tc.want {
				t.Fatalf("status/outcome = %s/%s, want %s/%s", push.Status, push.Outcome, tc.status, tc.want)
			}
		})
	}
}

// R7: the run view reads the ledger, so the push's summary line for a not_applicable scenario carries the
// precondition id and BOTH controls' observations (words about the instrument only).
func TestMapReport_NotApplicableSummaryCarriesTheIdAndBothObservations(t *testing.T) {
	na := &report.NotApplicable{Precondition: "np-in", Kind: "networkpolicy.ingress_enforced", Positive: "connected", Negative: "NOT refused: connected although a deny policy selects it"}
	push := mapIt(repWith(report.Summary{Total: 2, Passed: 1, NotApplicable: 1},
		report.ScenarioResult{ID: "A", Status: "passed"},
		report.ScenarioResult{ID: "B", Status: report.StatusNotApplicable, NotApplicable: na}))
	var got string
	for _, s := range push.Scenarios {
		if s.ID == "B" {
			got = s.Summary
		}
	}
	for _, want := range []string{"not applicable: np-in absent", "positive control: connected", "negative control: NOT refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	if push.Scenarios[0].Summary != "" {
		t.Errorf("a passed row's summary moved: %q", push.Scenarios[0].Summary)
	}
}

// A report without a precondition maps to the push it always did: the new tally is absent from the wire.
func TestMapReport_NoPreconditionLeavesTheWireUnchanged(t *testing.T) {
	push := mapIt(repWith(report.Summary{Total: 1, Passed: 1}, report.ScenarioResult{ID: "A", Status: "passed"}))
	b, _ := json.Marshal(push.Tallies)
	if strings.Contains(string(b), "not_applicable") {
		t.Fatalf("a run with no N/A grew a wire key: %s", b)
	}
}

func TestReduceReport_NotApplicable(t *testing.T) {
	rep := repWith(report.Summary{Total: 2, NotApplicable: 2}, report.ScenarioResult{ID: "A", Status: report.StatusNotApplicable}, report.ScenarioResult{ID: "B", Status: report.StatusNotApplicable})
	rep.Mode = "build"
	r := reduceReport(rep, "")
	if r.Status != "not_applicable" || r.NotApplicable != 2 || len(r.FailingScenarios) != 0 {
		t.Fatalf("%+v", r)
	}
	rep = repWith(report.Summary{Total: 2, Passed: 1, NotApplicable: 1}, report.ScenarioResult{ID: "A", Status: "passed"}, report.ScenarioResult{ID: "B", Status: report.StatusNotApplicable})
	if r := reduceReport(rep, ""); r.Status != "passed" {
		t.Fatalf("passed + N/A relays as passed, got %+v", r)
	}
}

func TestMeasuredPreconditions_ProjectInOrder_AndNilStaysNil(t *testing.T) {
	got := measuredPreconditions([]report.Precondition{
		{ID: "a", Kind: "networkpolicy.ingress_enforced", Verdict: "absent", Positive: "p", Negative: "n"},
		{ID: "b", Kind: "networkpolicy.egress_enforced", Verdict: "holds", Positive: "p2", Negative: "n2", Residue: "r"},
	})
	if len(got) != 2 || got[0].ID != "a" || got[1].Residue != "r" || got[1].Negative != "n2" {
		t.Fatalf("%+v", got)
	}
	if measuredPreconditions(nil) != nil {
		t.Fatal("no preconditions must stay nil so the record hashes as it always did")
	}
}

// The wiring: a final run whose report carries precondition results pushes a measurement that carries them
// and a bundle hash that covers them; a final run without any pushes neither.
func TestFinalRun_PreconditionsInTheReportAreBoundIntoThePush_AndAbsentWhenThereAreNone(t *testing.T) {
	run := func(pre []report.Precondition, rows []report.ScenarioResult, sum report.Summary) federation.ResultsPush {
		prev := toolcoreRun
		toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
			rep := report.Report{RunID: runID, Preconditions: pre, Summary: sum, Layers: []report.Layer{{Layer: "http", Scenarios: rows}}}
			b, _ := json.Marshal(rep)
			dir := filepath.Join(e.ResultsRoot, e.Instance)
			_ = os.MkdirAll(dir, 0o755)
			return nil, true, os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
		}
		t.Cleanup(func() { toolcoreRun = prev })
		cfg := ExecConfig{MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}, nil)}
		push, err := runFinal(t, cfg, &federation.RunAssignment{Mode: "final", ArtifactDigest: mdA, RunID: "run-p1"})
		if err != nil {
			t.Fatal(err)
		}
		return push
	}
	plain := run(nil, []report.ScenarioResult{{ID: "S-1", Status: "passed"}}, report.Summary{Passed: 1, Total: 1})
	if strings.Contains(string(plain.ArtifactMeasurement), "preconditions") {
		t.Fatalf("a run with no preconditions grew a key: %s", plain.ArtifactMeasurement)
	}
	pre := []report.Precondition{{ID: "np-in", Kind: "networkpolicy.ingress_enforced", Verdict: "absent", Positive: "connected", Negative: "NOT refused: connected"}}
	na := run(pre, []report.ScenarioResult{{ID: "S-1", Status: "passed"}, {ID: "S-2", Status: report.StatusNotApplicable}}, report.Summary{Passed: 1, NotApplicable: 1, Total: 2})
	m := decodeMeasurement(t, na)
	if len(m.Preconditions) != 1 || m.Preconditions[0].ID != "np-in" || m.Preconditions[0].Verdict != "absent" {
		t.Fatalf("the push's measurement carries %+v", m.Preconditions)
	}
	if na.Tallies.NotApplicable != 1 || na.Outcome != "passed" {
		t.Fatalf("tallies/outcome = %+v / %s", na.Tallies, na.Outcome)
	}
	if na.EvidenceBundleHash != artifactmeasure.BundleHash(na.ScenarioEvidenceRoot, &m) {
		t.Fatal("the anchored bundle hash is not the hash of the measurement the push carries")
	}
	// the same scenarios WITHOUT the precondition record hash differently: the results are inside the hash
	again := run(nil, []report.ScenarioResult{{ID: "S-1", Status: "passed"}, {ID: "S-2", Status: report.StatusNotApplicable}}, report.Summary{Passed: 1, NotApplicable: 1, Total: 2})
	if again.EvidenceBundleHash == na.EvidenceBundleHash {
		t.Fatal("the precondition results are not covered by the anchored evidence_bundle_hash")
	}
}

func TestBindMeasurement_PreconditionsChangeTheAnchoredBundleHash_NoneDoNot(t *testing.T) {
	m := artifactmeasure.Evaluate(mdA, nil, artifactmeasure.Reading{Reason: "x"})
	push := func(pre []report.Precondition) federation.ResultsPush {
		mm := m
		mm.Preconditions = measuredPreconditions(pre)
		p := federation.ResultsPush{Scenarios: []federation.ScenarioResult{{ID: "S", Outcome: "passed", EvidenceHash: "h"}}}
		bindMeasurement(&p, &mm)
		return p
	}
	base := report.Precondition{ID: "np", Kind: "networkpolicy.ingress_enforced", Verdict: "absent", Positive: "p", Negative: "n"}
	plain := push(nil)
	with := push([]report.Precondition{base})
	if plain.EvidenceBundleHash == with.EvidenceBundleHash {
		t.Fatal("the precondition list is not in the anchored hash")
	}
	if push(nil).EvidenceBundleHash != plain.EvidenceBundleHash {
		t.Fatal("the hash of a run with no preconditions is not stable")
	}
	for name, edit := range map[string]func(p *report.Precondition){
		"id": func(p *report.Precondition) { p.ID = "x" }, "kind": func(p *report.Precondition) { p.Kind = "networkpolicy.egress_enforced" },
		"verdict": func(p *report.Precondition) { p.Verdict = "holds" }, "positive": func(p *report.Precondition) { p.Positive = "q" },
		"negative": func(p *report.Precondition) { p.Negative = "m" }, "residue": func(p *report.Precondition) { p.Residue = "r" },
	} {
		e := base
		edit(&e)
		if push([]report.Precondition{e}).EvidenceBundleHash == with.EvidenceBundleHash {
			t.Errorf("editing the precondition's %s does not move the anchored hash", name)
		}
	}
}

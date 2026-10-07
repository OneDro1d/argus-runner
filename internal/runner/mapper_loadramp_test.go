package runner

// mapper_loadramp_test.go -- PR-E: an AMQP Load result rides the results push as
// ResultsPush.load_ramp, is committed to by the evidence hashes, and never reaches a builder projection.

import (
	"context"
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

func ramp2Steps() []report.LoadStep {
	restarts := 0
	return []report.LoadStep{
		{Step: 1, Sessions: 10, Status: report.LoadStepMeasured, WindowSeconds: 30, OfferedPerS: 100, SentPerS: 100, ConfirmedPerS: 100,
			DeliveredPerS: 100, DeliveredRatio: 1,
			PublishConfirmUs: &report.Quantiles{Min: 300, P50: 800, P75: 1000, P95: 2000, P99: 3000, Max: 9000},
			PublishDeliverUs: &report.Quantiles{Min: 400, P50: 900, P75: 1200, P95: 2500, P99: 4000, Max: 12000},
			RestartsDelta:    &restarts, Comfortable: true},
		{Step: 2, Sessions: 20, Status: report.LoadStepBlocked, WindowSeconds: 30, OfferedPerS: 200, SentPerS: 150, DeliveredPerS: 140, DeliveredRatio: 0.93,
			Errors:  map[string]int{"blocked": 12},
			Blocked: []report.BlockedPeriod{{SinceMs: 1000, UntilMs: 4000, Reason: "memory"}}, BlockedSeconds: 3, Comfortable: false},
	}
}

func loadReport(steps []report.LoadStep) *report.Report {
	return &report.Report{
		RunID:   "run-lr",
		Summary: report.Summary{Total: 2, Passed: 2},
		Layers: []report.Layer{
			{Layer: "AMQP Load", Scenarios: []report.ScenarioResult{
				{ID: "AMQL-001", Status: "passed", DurationMs: 70000, LoadDriver: "amqp", LoadTarget: "lab-broker", LoadSteps: steps},
			}},
			{Layer: "HTTP Ingestion", Scenarios: []report.ScenarioResult{{ID: "HTTP-001", Status: "passed", DurationMs: 10}}},
		},
	}
}

func mapLoad(rep *report.Report) federation.ResultsPush {
	return mapReport(rep, "run-lr", "", "full", "h", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
}

func TestMapReport_CarriesTheLoadRampOnTheWire(t *testing.T) {
	push := mapLoad(loadReport(ramp2Steps()))
	if len(push.LoadRamp) == 0 {
		t.Fatal("a report with an AMQP Load scenario produced a push with no load_ramp")
	}
	var got []report.LoadRampEntry
	if err := json.Unmarshal(push.LoadRamp, &got); err != nil {
		t.Fatalf("load_ramp is not a JSON array of entries: %v: %s", err, push.LoadRamp)
	}
	if len(got) != 1 || got[0].ScenarioID != "AMQL-001" || got[0].Target != "lab-broker" || got[0].Driver != "amqp" {
		t.Fatalf("entry = %+v, want one {AMQL-001, lab-broker, amqp}", got)
	}
	want, _ := json.Marshal(ramp2Steps())
	have, _ := json.Marshal(got[0].Steps)
	if string(want) != string(have) {
		t.Fatalf("steps changed on the way to the wire:\nwant %s\nhave %s", want, have)
	}
}

func TestMapReport_OneRampEntryPerLoadScenarioInIDOrder(t *testing.T) {
	rep := loadReport(ramp2Steps())
	rep.Layers[0].Scenarios = append([]report.ScenarioResult{
		{ID: "AMQL-009", Status: "passed", LoadDriver: "amqp", LoadTarget: "other", LoadSteps: ramp2Steps()[:1]},
	}, rep.Layers[0].Scenarios...)
	var got []report.LoadRampEntry
	if err := json.Unmarshal(mapLoad(rep).LoadRamp, &got); err != nil || len(got) != 2 {
		t.Fatalf("entries = %v, err %v", got, err)
	}
	if got[0].ScenarioID != "AMQL-001" || got[1].ScenarioID != "AMQL-009" {
		t.Fatalf("entries are not in scenario-id order: %s, %s", got[0].ScenarioID, got[1].ScenarioID)
	}
}

func TestMapReport_NoLoadScenarioMeansNoLoadRamp(t *testing.T) {
	rep := &report.Report{Summary: report.Summary{Total: 1, Passed: 1},
		Layers: []report.Layer{{Layer: "HTTP Ingestion", Scenarios: []report.ScenarioResult{{ID: "HTTP-001", Status: "passed"}}}}}
	push := mapLoad(rep)
	if push.LoadRamp != nil {
		t.Fatalf("load_ramp = %s on a run with no load scenario", push.LoadRamp)
	}
	b, _ := json.Marshal(push)
	if strings.Contains(string(b), "load_ramp") {
		t.Fatalf("the key load_ramp is on the wire of a non-load run: %s", b)
	}
}

func TestMapReport_LoadRampCarriesNoAddressThresholdOrCredential(t *testing.T) {
	b, _ := json.Marshal(mapLoad(loadReport(ramp2Steps())))
	low := strings.ToLower(string(b))
	for _, bad := range []string{"amqp://", "amqps://", "password", "passwd", "secret", "@", "url", "threshold", "max_error", "target_p95"} {
		if strings.Contains(low, bad) {
			t.Errorf("the pushed run contains %q: %s", bad, b)
		}
	}
}

// the ramp numbers are in the evidence the run anchor commits to
func TestEvidenceHash_CommitsToTheRamp(t *testing.T) {
	base := mapLoad(loadReport(ramp2Steps()))
	steps := ramp2Steps()
	steps[0].PublishDeliverUs.P99++
	moved := mapLoad(loadReport(steps))
	if base.Scenarios[0].EvidenceHash == moved.Scenarios[0].EvidenceHash {
		t.Fatal("changing one step's p99 left the scenario's evidence_hash unchanged")
	}
	if base.EvidenceBundleHash == moved.EvidenceBundleHash {
		t.Fatal("changing one step's p99 left the run's evidence_bundle_hash unchanged")
	}
	// the anchored value with an artifact measurement (bindMeasurement) commits to it as well
	m := artifactmeasure.Evaluate("", nil, artifactmeasure.Reading{Reason: "x"})
	a, b := base, moved
	bindMeasurement(&a, &m)
	bindMeasurement(&b, &m)
	if a.EvidenceBundleHash == b.EvidenceBundleHash {
		t.Fatal("with an artifact measurement the bundle hash no longer commits to the ramp numbers")
	}
	// untouched neighbours keep their hash
	if base.Scenarios[1].EvidenceHash != moved.Scenarios[1].EvidenceHash {
		t.Fatal("a non-load scenario's evidence_hash moved with the ramp")
	}
}

// A non-load scenario's evidence hash is byte-identical to what it was before the load fields existed.
func TestEvidenceHash_NonLoadScenarioUnchanged(t *testing.T) {
	// computed on origin/feat/amqp-d-load-layer (c41cf8c) before PR-E touched anything
	const golden = "68f31b0e81a0d533fa0c4b8c104c2ffa7e532df0aceb4aa49b44c882b06f70a7"
	got := evidenceHash(report.ScenarioResult{ID: "HTTP-001", Status: "passed", DurationMs: 10})
	if got != golden {
		t.Fatalf("evidence hash of a plain scenario = %s, pinned %s", got, golden)
	}
}

// The real executor path (NewRunFunc -> mapReport -> bindMeasurement), with only the run core stubbed:
// the pushed run carries load_ramp, and the anchored bundle hash moves when one ramp number moves.
func TestNewRunFunc_PushCarriesLoadRampAndTheBundleCommitsToIt(t *testing.T) {
	prev := toolcoreRun
	t.Cleanup(func() { toolcoreRun = prev })
	run := func(p99 int64) federation.ResultsPush {
		toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
			steps := ramp2Steps()
			steps[0].PublishDeliverUs.P99 = p99
			rep := loadReport(steps)
			rep.RunID = runID
			b, _ := json.Marshal(rep)
			dir := filepath.Join(e.ResultsRoot, e.Instance)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, false, err
			}
			return nil, true, os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
		}
		cfg := ExecConfig{MeasureArtifact: func(context.Context) artifactmeasure.Reading {
			return artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}
		}}
		push, err := runFinal(t, cfg, &federation.RunAssignment{Mode: "final", ArtifactDigest: mdA, RunID: "run-lr"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return push
	}
	a, b := run(4000), run(4001)
	if len(a.LoadRamp) == 0 {
		t.Fatal("the executor's push carries no load_ramp")
	}
	if a.EvidenceBundleHash == b.EvidenceBundleHash {
		t.Fatal("the executor's anchored bundle hash did not move with a ramp number")
	}
}

// Builder custody: neither a build-mode nor a certification-mode report relays a step.
func TestRelayedReport_NeverCarriesLoadSteps(t *testing.T) {
	for _, mode := range []string{"build", "final", "scheduled", "rehearsal", ""} {
		rep := loadReport(ramp2Steps())
		rep.Mode = mode
		b, _ := json.Marshal(reduceReport(rep, "https://grafana/d/x"))
		for _, bad := range []string{"load_steps", "load_ramp", "p99", "sessions", "publish_deliver", "lab-broker", "amqp"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("mode %q: the relayed answer contains %q: %s", mode, bad, b)
			}
		}
	}
}

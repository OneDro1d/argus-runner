package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/toolcore"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// The faults of a run are carried into the measurement the bundle hash commits to.

func TestMeasuredFaults_DropTheScenarioInternalStepName_AndKeepOrder(t *testing.T) {
	rep := []report.Fault{
		{Step: "secret-step-a", Op: "stop", Target: "trades", Params: map[string]int{"replicas": 0}, AppliedAt: "t1", RestoredAt: "t2", RestoreOutcome: report.FaultRestored},
		{Step: "secret-step-b", Op: "delay", Target: "mcp-to-trades", AppliedAt: "t3", RestoredAt: "t4", RestoreOutcome: report.FaultAlreadyRestored, Toxiproxy: true},
	}
	got := measuredFaults(rep)
	if len(got) != 2 || got[0].Target != "trades" || got[1].Target != "mcp-to-trades" || !got[1].Toxiproxy || got[1].RestoreOutcome != "already-restored" {
		t.Fatalf("%+v", got)
	}
	if measuredFaults(nil) != nil {
		t.Fatal("no faults must stay nil so the record hashes as it always did")
	}
}

// The wiring, not just the projection: a final run whose report carries faults pushes a measurement that
// carries them and an evidence_bundle_hash that covers them; a final run without any pushes neither.
func TestFinalRun_FaultsInTheReportAreBoundIntoThePush_AndAbsentWhenThereAreNone(t *testing.T) {
	run := func(faults []report.Fault) federation.ResultsPush {
		prev := toolcoreRun
		toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
			rep := report.Report{RunID: runID, Faults: faults, Summary: report.Summary{Passed: 1, Total: 1},
				Layers: []report.Layer{{Layer: "http", Scenarios: []report.ScenarioResult{{ID: "S-1", Status: "passed"}}}}}
			b, _ := json.Marshal(rep)
			dir := filepath.Join(e.ResultsRoot, e.Instance)
			_ = os.MkdirAll(dir, 0o755)
			return nil, true, os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
		}
		t.Cleanup(func() { toolcoreRun = prev })
		cfg := ExecConfig{MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}, nil)}
		push, err := runFinal(t, cfg, &federation.RunAssignment{Mode: "final", ArtifactDigest: mdA, RunID: "run-f1"})
		if err != nil {
			t.Fatal(err)
		}
		return push
	}
	plain := run(nil)
	if strings.Contains(string(plain.ArtifactMeasurement), "faults") {
		t.Fatalf("a no-fault run grew a faults key: %s", plain.ArtifactMeasurement)
	}
	faulted := run([]report.Fault{{Step: "hidden-step", Op: "stop", Target: "trades", AppliedAt: "t1", RestoredAt: "t2", RestoreOutcome: report.FaultRestored}})
	m := decodeMeasurement(t, faulted)
	if len(m.Faults) != 1 || m.Faults[0].Target != "trades" {
		t.Fatalf("the push's measurement carries %+v", m.Faults)
	}
	if strings.Contains(string(faulted.ArtifactMeasurement), "hidden-step") {
		t.Fatal("a scenario-internal step name reached the push")
	}
	if faulted.EvidenceBundleHash != artifactmeasure.BundleHash(faulted.ScenarioEvidenceRoot, &m) || faulted.EvidenceBundleHash == plain.EvidenceBundleHash {
		t.Fatal("the anchored bundle hash does not cover the faults")
	}
}

func TestBindMeasurement_FaultsChangeTheAnchoredBundleHash_NoFaultsDoNot(t *testing.T) {
	m := artifactmeasure.Evaluate("sha256:"+string(make([]byte, 0))+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil,
		artifactmeasure.Reading{Reason: "x"})
	push := func(faults []report.Fault) federation.ResultsPush {
		mm := m
		mm.Faults = measuredFaults(faults)
		p := federation.ResultsPush{Scenarios: []federation.ScenarioResult{{ID: "S", Outcome: "passed", EvidenceHash: "h"}}}
		bindMeasurement(&p, &mm)
		return p
	}
	plain := push(nil)
	faulted := push([]report.Fault{{Op: "stop", Target: "trades", AppliedAt: "t1", RestoredAt: "t2", RestoreOutcome: report.FaultRestored}})
	if plain.EvidenceBundleHash == faulted.EvidenceBundleHash {
		t.Fatal("the fault list is not in the anchored hash")
	}
	if push(nil).EvidenceBundleHash != plain.EvidenceBundleHash {
		t.Fatal("the hash of a no-fault run is not stable")
	}
}

package runner

// artifact_measure_test.go — a certifying run MEASURES the image digests the SUT is running, before any
// scenario runs, refuses on a proven mismatch, and binds what it measured into evidence_bundle_hash.
// The run core (toolcore.Run — JMeter and a SUT) is replaced by a stub that records whether it was
// called and writes the report a passing run would; everything around it is the real NewRunFunc.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

const (
	mdA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mdB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mdC = "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// stubRunCore swaps toolcore.Run for a stub that writes a one-scenario passing report where NewRunFunc
// reads it. The returned counter says how many times a scenario run was attempted.
func stubRunCore(t *testing.T) *int {
	t.Helper()
	calls := 0
	prev := toolcoreRun
	toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
		calls++
		rep := report.Report{
			RunID:   runID,
			Summary: report.Summary{Passed: 1, Total: 1},
			Layers: []report.Layer{{Layer: "http", Scenarios: []report.ScenarioResult{
				{ID: "S-1", Status: "passed", DurationMs: 5},
			}}},
		}
		b, _ := json.Marshal(rep)
		dir := filepath.Join(e.ResultsRoot, e.Instance)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, err
		}
		return nil, true, os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
	}
	t.Cleanup(func() { toolcoreRun = prev })
	return &calls
}

func measurer(r artifactmeasure.Reading, called *int) artifactmeasure.Func {
	return func(context.Context) artifactmeasure.Reading {
		if called != nil {
			*called++
		}
		return r
	}
}

func runFinal(t *testing.T, cfg ExecConfig, a *federation.RunAssignment) (federation.ResultsPush, error) {
	t.Helper()
	cfg.ResultsRoot = t.TempDir()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "argus-config.yaml")
	return NewRunFunc(cfg)(context.Background(), a)
}

func decodeMeasurement(t *testing.T, p federation.ResultsPush) artifactmeasure.Measurement {
	t.Helper()
	if len(p.ArtifactMeasurement) == 0 {
		t.Fatalf("the push carries no artifact_measurement: %+v", p)
	}
	var m artifactmeasure.Measurement
	if err := json.Unmarshal(p.ArtifactMeasurement, &m); err != nil {
		t.Fatalf("artifact_measurement does not decode: %v", err)
	}
	return m
}

func TestFinalRun_MeasuresBeforeRunning_MatchedIsBoundIntoTheEvidenceBundle(t *testing.T) {
	core := stubRunCore(t)
	var order []string
	cfg := ExecConfig{MeasureArtifact: func(context.Context) artifactmeasure.Reading {
		order = append(order, "measure")
		if *core != 0 {
			t.Error("the SUT was measured AFTER a scenario had already run")
		}
		return artifactmeasure.Reading{Source: "k8s", Digests: []string{mdB, mdA}}
	}}
	push, err := runFinal(t, cfg, &federation.RunAssignment{
		Mode: "final", ArtifactDigest: mdA, RunID: "run-m1", CommitmentImageDigests: []string{mdB, mdC},
	})
	if err != nil {
		t.Fatalf("a matched final run must run: %v", err)
	}
	if *core != 1 || len(order) != 1 {
		t.Fatalf("scenario runs %d, measurements %d, want 1 and 1", *core, len(order))
	}
	m := decodeMeasurement(t, push)
	raw, _ := json.Marshal(push)
	t.Logf("MATCHED FINAL RUN PUSH: %s", raw)
	if m.State != artifactmeasure.StateMatched || m.Declared != mdA || strings.Join(m.Running, ",") != mdA+","+mdB {
		t.Fatalf("measurement %+v", m)
	}
	if strings.Join(m.CommitmentFound, ",") != mdB || strings.Join(m.CommitmentNotFound, ",") != mdC {
		t.Errorf("commitment images found %v not found %v, want [B] / [C]", m.CommitmentFound, m.CommitmentNotFound)
	}
	if push.ScenarioEvidenceRoot == "" || push.ScenarioEvidenceRoot != evidenceBundleHash(push.Scenarios) {
		t.Errorf("scenario_evidence_root %q is not the scenario-only bundle %q", push.ScenarioEvidenceRoot, evidenceBundleHash(push.Scenarios))
	}
	if push.EvidenceBundleHash != artifactmeasure.BundleHash(push.ScenarioEvidenceRoot, &m) {
		t.Errorf("evidence_bundle_hash %q does not cover the measurement", push.EvidenceBundleHash)
	}
	if push.EvidenceBundleHash == push.ScenarioEvidenceRoot {
		t.Error("the anchored bundle hash equals the scenario-only hash: the measurement is not bound")
	}
	if push.ArtifactDigest != mdA {
		t.Errorf("artifact_digest %q", push.ArtifactDigest)
	}
}

func TestFinalAndScheduled_ProvenMismatch_IsRefusedBeforeAnyScenarioRuns(t *testing.T) {
	for _, mode := range []string{"final", "scheduled"} {
		core := stubRunCore(t)
		resultsRoot := t.TempDir()
		run := NewRunFunc(ExecConfig{ResultsRoot: resultsRoot, MeasureArtifact: measurer(
			artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA, mdB}}, nil)})
		push, err := run(context.Background(), &federation.RunAssignment{Mode: mode, ArtifactDigest: mdC, RunID: "run-mm"})
		if err == nil {
			t.Fatalf("%s: a declared digest that is not running was not refused", mode)
		}
		t.Logf("%s REFUSAL: %v", strings.ToUpper(mode), err)
		var me *artifactmeasure.MismatchError
		if !errors.As(err, &me) {
			t.Fatalf("%s: error %T is not a *MismatchError", mode, err)
		}
		for _, want := range []string{"refused", mdC, mdA, mdB} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal must name %q: %v", mode, want, err)
			}
		}
		if *core != 0 {
			t.Errorf("%s: %d scenario run(s) started before the refusal", mode, *core)
		}
		if _, serr := os.Stat(filepath.Join(resultsRoot, "materialized")); !os.IsNotExist(serr) {
			t.Errorf("%s: the run was materialised before the refusal", mode)
		}
		if push.EvidenceBundleHash != "" || push.Outcome != "" || len(push.ArtifactMeasurement) != 0 {
			t.Errorf("%s: a refused run returned a verdict payload: %+v", mode, push)
		}
	}
}

func TestFinalRun_Unmeasurable_RunsAndSaysNotMeasuredWhy(t *testing.T) {
	stubRunCore(t)
	for name, r := range map[string]artifactmeasure.Reading{
		"forbidden":   {Source: "k8s", Reason: "forbidden: pods in namespace sut"},
		"tag only":    {Source: "k8s", Unresolved: 3},
		"sut missing": {Source: "k8s", Reason: "no running pod found in namespace sut"},
	} {
		push, err := runFinal(t, ExecConfig{MeasureArtifact: measurer(r, nil)},
			&federation.RunAssignment{Mode: "final", ArtifactDigest: mdA, RunID: "run-nm"})
		if err != nil {
			t.Fatalf("%s: an unmeasurable SUT must not stop the run: %v", name, err)
		}
		m := decodeMeasurement(t, push)
		raw, _ := json.Marshal(m)
		t.Logf("%s NOT MEASURED RECORD: %s", strings.ToUpper(name), raw)
		if m.State != artifactmeasure.StateNotMeasured || m.Reason == "" {
			t.Errorf("%s: measurement %+v, want not_measured with a reason", name, m)
		}
		if push.Outcome != "passed" {
			t.Errorf("%s: outcome %q — not being able to measure is not a failure", name, push.Outcome)
		}
		if push.EvidenceBundleHash != artifactmeasure.BundleHash(push.ScenarioEvidenceRoot, &m) {
			t.Errorf("%s: the not-measured record is not bound into the bundle hash", name)
		}
	}
}

func TestFinalRun_WithNoMeasurerConfigured_IsNotMeasured_NeverMatched(t *testing.T) {
	stubRunCore(t)
	push, err := runFinal(t, ExecConfig{}, &federation.RunAssignment{Mode: "final", ArtifactDigest: mdA, RunID: "run-nomeasurer"})
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMeasurement(t, push)
	if m.State != artifactmeasure.StateNotMeasured || !strings.Contains(m.Reason, "measurer") {
		t.Fatalf("measurement %+v, want not_measured naming the missing measurer", m)
	}
}

// build and rehearsal certify nothing: they must never be refused on a mismatch, and stay as they were.
func TestBuildAndRehearsal_NeverRefusedOnAMismatch_AndCarryNoMeasurement(t *testing.T) {
	stubRunCore(t)
	for _, mode := range []string{"build", "", "rehearsal"} {
		calls := 0
		push, err := runFinal(t, ExecConfig{MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}, &calls)},
			&federation.RunAssignment{Mode: mode, RunID: "run-build"})
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if calls != 0 || len(push.ArtifactMeasurement) != 0 || push.ScenarioEvidenceRoot != "" {
			t.Errorf("mode %q measured (%d calls) or carries a measurement: %+v", mode, calls, push)
		}
		if push.EvidenceBundleHash != evidenceBundleHash(push.Scenarios) {
			t.Errorf("mode %q: the bundle hash changed shape for a run that certifies nothing", mode)
		}
	}
}

// A scenario that failed to open is appended AFTER the run and the bundle is recomputed: the recomputation
// must keep the measurement, or a failing certifying run would quietly anchor a bundle with none.
func TestFinalRun_OpenFailureRecomputation_KeepsTheMeasurementBound(t *testing.T) {
	stubRunCore(t)
	push, err := runFinal(t, ExecConfig{MeasureArtifact: measurer(artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}, nil)},
		&federation.RunAssignment{Mode: "final", ArtifactDigest: mdA, RunID: "run-of",
			Scenarios: []federation.ScenarioPayload{{Path: "http/BAD-1.md", Sealed: &federation.SealedBody{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if push.Outcome != "failed" {
		t.Fatalf("outcome %q, want failed (a scenario did not open)", push.Outcome)
	}
	m := decodeMeasurement(t, push)
	if push.ScenarioEvidenceRoot != evidenceBundleHash(push.Scenarios) {
		t.Errorf("the scenario root was not recomputed over the errored scenario")
	}
	if push.EvidenceBundleHash != artifactmeasure.BundleHash(push.ScenarioEvidenceRoot, &m) {
		t.Errorf("the recomputed bundle hash dropped the measurement")
	}
}

// 🚩 Wiring: a correct measurer that Bootstrap never installs ships dead — every final run would say
// "not measured: no measurer" forever, which is honest, silent and useless.
func TestBootstrapInstallsTheSystemMeasurer(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "systemMeasurer(")
	j := strings.Index(s, "Run:    NewRunFunc(cfg.Fed.Exec)") // the executor literal, not a comment
	if i < 0 || j < 0 || i > j {
		t.Fatalf("Bootstrap must assign cfg.Fed.Exec.MeasureArtifact = systemMeasurer(...) BEFORE NewRunFunc(cfg.Fed.Exec) copies the struct (systemMeasurer at %d, NewRunFunc at %d)", i, j)
	}
}

func TestSystemMeasurer_TierAndNamespaceComeFromTheExecutorsOwnConfig(t *testing.T) {
	t.Setenv("ARGUS_SUT_NAMESPACE", "")
	r := systemMeasurer(ExecConfig{Tier: "k3d"})(context.Background())
	if !strings.Contains(r.Reason, "ARGUS_SUT_NAMESPACE") {
		t.Fatalf("reading %+v, want a reason that names the missing namespace setting", r)
	}
	r = systemMeasurer(ExecConfig{Tier: ""})(context.Background())
	if !strings.Contains(r.Reason, "tier") {
		t.Fatalf("reading %+v, want a reason naming the unknown tier", r)
	}
}

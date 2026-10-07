package runner

// compare_run_test.go — ARGUS-CMP-3: mode `compare` on the executor. The run core is
// stubbed where the test is about the plumbing around it (NewRunFunc, the push, the relay) and is the
// REAL toolcore.Run, over a real chain scenario against an httptest SUT, where the test is about what
// the whole run keeps and what it must not.

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/argus"
	"github.com/OneDro1d/argus-runner/internal/artifactmeasure"
	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

func newKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sealed(t *testing.T, k *ecdh.PrivateKey, runID, path, body string) federation.ScenarioPayload {
	t.Helper()
	sb, err := federation.SealBody(k.PublicKey(), runID, body)
	if err != nil {
		t.Fatal(err)
	}
	return federation.ScenarioPayload{Path: path, Sealed: &sb}
}

// stubCompareCore writes a report with recorded outputs where NewRunFunc reads it, and records what the
// run core was handed.
type coreSeen struct {
	calls    int
	mode     string
	scenFile []string
}

func stubCompareCore(t *testing.T, seen *coreSeen, rows []compare.OutputRecord, envFP string) {
	t.Helper()
	prev := toolcoreRun
	toolcoreRun = func(e toolcore.Env, runID, layer, tag, scenarioID string) (any, bool, error) {
		seen.calls++
		seen.mode = e.RunMode
		_ = filepath.Walk(e.ScenariosDir, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				seen.scenFile = append(seen.scenFile, filepath.Base(p))
			}
			return nil
		})
		rep := report.Report{RunID: runID, Mode: e.RunMode, Summary: report.Summary{Passed: 2, Total: 2},
			Layers: []report.Layer{{Layer: "Permissions", Scenarios: []report.ScenarioResult{
				{ID: "CHN-B", Status: "passed", Outputs: rows},
				{ID: "CHN-A", Status: "passed", Outputs: rows},
			}}}}
		if envFP != "" {
			rep.Environment = &envcapture.Capture{Captured: true, Fingerprint: envFP}
		}
		b, _ := json.Marshal(rep)
		dir := filepath.Join(e.ResultsRoot, e.Instance)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, false, err
		}
		return nil, true, os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
	}
	t.Cleanup(func() { toolcoreRun = prev })
}

func sampleRows() []compare.OutputRecord {
	h := strings.Repeat("a", 64)
	return []compare.OutputRecord{{V: 1, Step: "create", Sample: 1, State: compare.StateRecorded, Status: 200,
		Parts: compare.Parts{Status: h, Body: h}, Hash: h, BodyKind: compare.KindJSON, BodyBytes: 10}}
}

func compareAssignment(t *testing.T, k *ecdh.PrivateKey) *federation.RunAssignment {
	t.Helper()
	return &federation.RunAssignment{Mode: "compare", RunID: "run-c1", RunRequestID: "rq-1", Scope: "full", SetHash: "sethash",
		CompareScenarios: []federation.ScenarioPayload{
			sealed(t, k, "run-c1", "permissions/CHN-A.md", "# Scenario: a"),
			sealed(t, k, "run-c1", "permissions/CHN-B.md", "# Scenario: b"),
		}}
}

func TestCompareRun_RequiresEveryBodySealed(t *testing.T) {
	core := &coreSeen{}
	stubCompareCore(t, core, sampleRows(), "")
	k := newKey(t)
	a := compareAssignment(t, k)
	a.CompareScenarios = append(a.CompareScenarios, federation.ScenarioPayload{Path: "permissions/CHN-C.md", Body: "# clear"})
	_, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k})(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "sealed") || !strings.Contains(err.Error(), "compare") {
		t.Fatalf("a compare run with a clear body must be refused as unsealed: %v", err)
	}
	if core.calls != 0 {
		t.Errorf("the run core ran %d time(s) for a refused assignment", core.calls)
	}
}

func TestCompareRun_MaterialisesCompareScenariosAndFillsThePush(t *testing.T) {
	core := &coreSeen{}
	stubCompareCore(t, core, sampleRows(), "fp-abc")
	k := newKey(t)
	push, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k})(context.Background(), compareAssignment(t, k))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(core.scenFile)
	if core.calls != 1 || core.mode != "compare" || strings.Join(core.scenFile, ",") != "CHN-A.md,CHN-B.md" {
		t.Fatalf("core calls %d mode %q scenario files %v: compare_scenarios were not materialised", core.calls, core.mode, core.scenFile)
	}
	rows, norm, derr := compare.DecodeOutputs(push.Outputs)
	if derr != nil {
		t.Fatalf("push.outputs does not decode on the control plane's reader: %v", derr)
	}
	if len(rows) != 2 || rows[0].ScenarioID != "CHN-A" || rows[1].ScenarioID != "CHN-B" || string(norm) != string(push.Outputs) {
		t.Errorf("rows %+v (scenario-id order, canonical bytes expected)", rows)
	}
	if push.OutputsRoot == "" || push.OutputsRoot != compare.OutputsRoot(rows) {
		t.Errorf("outputs_root %q is not compare.OutputsRoot of the rows", push.OutputsRoot)
	}
	if push.EnvFingerprint != "fp-abc" {
		t.Errorf("env_fingerprint %q", push.EnvFingerprint)
	}
	if push.SetHash != "sethash" || push.RunID != "run-c1" || push.Tallies.Total != 2 {
		t.Errorf("push %+v", push)
	}
	// the wire carries digests only
	wire, _ := json.Marshal(push)
	for _, banned := range []string{"\"observed\"", "\"expected\"", "\"failure\"", "claim", "header_value"} {
		if strings.Contains(string(wire), banned) {
			t.Errorf("the push carries %s: %s", banned, wire)
		}
	}
}

func TestCompareRun_NoOutputsOnThePushOutsideCompareMode(t *testing.T) {
	for _, mode := range []string{"build", "final", "scheduled", "rehearsal", ""} {
		core := &coreSeen{}
		stubCompareCore(t, core, sampleRows(), "fp-abc") // even a report that carries rows must not put them on the wire
		k := newKey(t)
		a := &federation.RunAssignment{Mode: mode, RunID: "run-x", ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
			Scenarios: []federation.ScenarioPayload{sealed(t, k, "run-x", "permissions/CHN-A.md", "# a")}}
		push, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k,
			MeasureArtifact: func(context.Context) artifactmeasure.Reading {
				return artifactmeasure.Reading{Source: "k8s", Digests: []string{"sha256:" + strings.Repeat("a", 64)}}
			}})(context.Background(), a)
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if len(push.Outputs) != 0 || push.OutputsRoot != "" || push.EnvFingerprint != "" {
			t.Errorf("mode %q: push carries outputs=%d root=%q fp=%q", mode, len(push.Outputs), push.OutputsRoot, push.EnvFingerprint)
		}
	}
}

func TestCompareRun_TakesTheMeasurementAndRefusesOnlyAProvenMismatch(t *testing.T) {
	k := newKey(t)
	mdA := "sha256:" + strings.Repeat("a", 64)
	mdB := "sha256:" + strings.Repeat("b", 64)

	// a proven mismatch against a declared digest: refused BEFORE the core runs
	core := &coreSeen{}
	stubCompareCore(t, core, sampleRows(), "")
	a := compareAssignment(t, k)
	a.ArtifactDigest = mdB
	_, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k, MeasureArtifact: func(context.Context) artifactmeasure.Reading {
		return artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}
	}})(context.Background(), a)
	if err == nil || core.calls != 0 {
		t.Fatalf("a declared digest that is provably not running must refuse the run before it starts (err %v, core calls %d)", err, core.calls)
	}

	// no declared digest: measured, recorded as not compared, and the run goes ahead; the measurement is bound
	core = &coreSeen{}
	stubCompareCore(t, core, sampleRows(), "")
	measured := 0
	push, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k, MeasureArtifact: func(context.Context) artifactmeasure.Reading {
		measured++
		return artifactmeasure.Reading{Source: "k8s", Digests: []string{mdA}}
	}})(context.Background(), compareAssignment(t, k))
	if err != nil || measured != 1 || core.calls != 1 {
		t.Fatalf("err %v measured %d core calls %d", err, measured, core.calls)
	}
	if len(push.ArtifactMeasurement) == 0 || push.ScenarioEvidenceRoot == "" {
		t.Errorf("the measurement is not bound into the push: %+v", push)
	}

	// an unmeasurable SUT proceeds, recorded as such
	core = &coreSeen{}
	stubCompareCore(t, core, sampleRows(), "")
	a = compareAssignment(t, k)
	a.ArtifactDigest = mdB
	push, err = NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k})(context.Background(), a)
	if err != nil || core.calls != 1 || len(push.ArtifactMeasurement) == 0 {
		t.Fatalf("an unmeasurable SUT must proceed and say so: err %v core %d", err, core.calls)
	}
}

func TestCompareRun_OversizeOutputsAreOmittedNotRefused(t *testing.T) {
	core := &coreSeen{}
	big := make([]compare.OutputRecord, 0, 1200)
	h := strings.Repeat("a", 64)
	for i := 1; i <= 8; i++ {
		for j := 0; j < 300; j++ {
			big = append(big, compare.OutputRecord{V: 1, Sample: i, State: compare.StateRecorded, Status: 200, Hash: h,
				Parts: compare.Parts{Status: h, Body: h}, BodyKind: compare.KindJSON})
		}
	}
	stubCompareCore(t, core, big, "")
	k := newKey(t)
	push, err := NewRunFunc(ExecConfig{ResultsRoot: t.TempDir(), X25519Priv: k})(context.Background(), compareAssignment(t, k))
	if err != nil {
		t.Fatalf("an oversize output set must never refuse the push: %v", err)
	}
	if len(push.Outputs) != 0 || push.OutputsRoot != "" {
		t.Errorf("an oversize output set must be omitted whole (not measured), got %d bytes", len(push.Outputs))
	}
	if push.Tallies.Total != 2 {
		t.Errorf("the verdict must still be pushed: %+v", push.Tallies)
	}
}

// ── the whole run, real core ─────────────────────────────────────────────────────────────────

func TestCompareRun_EndToEnd_RealCore_DigestsOnTheWireBodyOnlyOnDisk(t *testing.T) {
	const canary = "zebra-quartz-meadow-lantern"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Leak", canary)
		_, _ = w.Write([]byte(`{"id":"ws-1","note":"` + canary + `"}`))
	}))
	defer srv.Close()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: c", "", "## Metadata", "- **ID**: CHN-E2E", "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json",
		`{"steps":[{"type":"http","name":"create","method":"POST","url":"` + srv.URL + `/things"}]}`, "```", "",
		"## EXPECT", "### Runnable", "- step create: status=200", "", "## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		"## COMPARE", "- **Reference**: measured", "- **Output**: status, body", "- **Mask**: $.id", "",
	}, "\n")
	k := newKey(t)
	a := &federation.RunAssignment{Mode: "compare", RunID: "run-e2e", RunRequestID: "rq-e2e", Scope: "full", SetHash: "sh",
		CompareScenarios: []federation.ScenarioPayload{sealed(t, k, "run-e2e", "permissions/CHN-E2E.md", md)}}
	// the real core, but a clean env: no telemetry endpoints
	t.Setenv("ARGUS_SUT_NAMESPACE", "")
	push, err := NewRunFunc(ExecConfig{ConfigPath: cfgPath, ResultsRoot: root, X25519Priv: k})(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if push.Tallies.Passed != 1 || push.Tallies.Total != 1 {
		t.Fatalf("tallies %+v", push.Tallies)
	}
	rows, _, derr := compare.DecodeOutputs(push.Outputs)
	if derr != nil || len(rows) != 1 || rows[0].ScenarioID != "CHN-E2E" || rows[0].State != compare.StateRecorded || rows[0].Step != "create" {
		t.Fatalf("outputs %s (%v)", push.Outputs, derr)
	}
	wire, _ := json.Marshal(push)
	reportJSON, _ := os.ReadFile(filepath.Join(root, "local", "report.json"))
	runJSON, _ := os.ReadFile(filepath.Join(root, "local", "runs", "run-e2e.json"))
	for name, blob := range map[string][]byte{"the encoded ResultsPush": wire, "report.json": reportJSON, "runs/run-e2e.json": runJSON} {
		if strings.Contains(string(blob), canary) || strings.Contains(strings.ToLower(string(blob)), "x-leak") {
			t.Errorf("the canary reached %s", name)
		}
	}
	if !strings.Contains(string(reportJSON), `"outputs"`) || !strings.Contains(string(reportJSON), rows[0].Hash) {
		t.Errorf("report.json carries no output digest: %s", reportJSON)
	}
	file, err := os.ReadFile(filepath.Join(root, "local", "outputs", "run-e2e", "CHN-E2E__create.1.json"))
	if err != nil || !strings.Contains(string(file), canary) || strings.Contains(string(file), "ws-1") {
		t.Fatalf("the stored file must hold the canonical, masked body (err %v): %s", err, file)
	}
	t.Logf("E2E CANARY %q: only in the stored output file; ResultsPush, report.json and runs/run-e2e.json clean", canary)

	// the relay answers get_output from that file
	cmd := NewCommandFunc(ExecConfig{ResultsRoot: root})
	out, err := cmd(context.Background(), "get_output", json.RawMessage(`{"run_id":"run-e2e","scenario_id":"CHN-E2E","step":"create","sample":1}`))
	if err != nil || string(out) != string(file) {
		t.Fatalf("get_output = %q, %v", out, err)
	}
}

// ── get_output (the relay verb, executor side) ───────────────────────────────────────────────────

func TestRelay_GetOutput_ReturnsTheStoredFileAndRefusesAnythingElse(t *testing.T) {
	root := t.TempDir()
	base := argus.OutputsBase(filepath.Join(root, "local"))
	if err := argus.WriteOutputFile(base, "run-1", "S-1", "create", 2, []byte(`{"status":200}`)); err != nil {
		t.Fatal(err)
	}
	cmd := NewCommandFunc(ExecConfig{ResultsRoot: root})
	call := func(args string) (json.RawMessage, error) {
		return cmd(context.Background(), "get_output", json.RawMessage(args))
	}
	out, err := call(`{"run_id":"run-1","scenario_id":"S-1","step":"create","sample":2}`)
	if err != nil || string(out) != `{"status":200}` {
		t.Fatalf("got %q, %v", out, err)
	}
	// missing: closed, and no filesystem path
	_, err = call(`{"run_id":"run-1","scenario_id":"S-1","step":"create","sample":3}`)
	if err == nil || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "outputs") {
		t.Errorf("a missing output must be a closed error without a path: %v", err)
	}
	for _, bad := range []string{
		`{"run_id":"../x","scenario_id":"S-1","step":"","sample":1}`,
		`{"run_id":"","scenario_id":"S-1","step":"","sample":1}`,
		`{"run_id":"run-1","scenario_id":"../S-1","step":"","sample":1}`,
		`{"run_id":"run-1","scenario_id":"S-1","step":"a/b","sample":1}`,
		`{"run_id":"run-1","scenario_id":"S-1","step":"create","sample":0}`,
		`{"run_id":"run-1","scenario_id":"S-1","step":"create","sample":9}`,
		`{"run_id":"run-1","scenario_id":"S-1","step":"create"}`,
		`not json`,
	} {
		if b, err := call(bad); err == nil {
			t.Errorf("%s was answered with %q", bad, b)
		}
	}
}

func TestRelay_GetOutput_ReadsTheToolInstanceDirectory(t *testing.T) {
	root := t.TempDir()
	base := argus.OutputsBase(filepath.Join(root, "inst-x"))
	if err := argus.WriteOutputFile(base, "run-1", "S-1", "", 1, []byte(`{"status":201}`)); err != nil {
		t.Fatal(err)
	}
	out, err := NewCommandFunc(ExecConfig{ResultsRoot: root, ToolInstance: "inst-x"})(context.Background(), "get_output",
		json.RawMessage(`{"run_id":"run-1","scenario_id":"S-1","step":"","sample":1}`))
	if err != nil || string(out) != `{"status":201}` {
		t.Fatalf("got %q, %v", out, err)
	}
}

// ── custody ──────────────────────────────────────────────────────────────────────────────────

func TestReduceReport_ACompareRunGivesABuilderExactlyTheVerdictOnlyKeys(t *testing.T) {
	rep := &report.Report{RunID: "run-c1", Mode: "compare", Summary: report.Summary{Passed: 1, Failed: 1, Total: 2},
		BuildRecordObjectID: "obj-1",
		Layers: []report.Layer{{Layer: "Permissions", Scenarios: []report.ScenarioResult{
			{ID: "CHN-A", Status: "failed", CorrelationID: "tr-1", Outputs: sampleRows()},
			{ID: "CHN-B", Status: "passed", CorrelationID: "tr-2", Outputs: sampleRows()},
		}}}}
	out := reduceReport(rep, "http://grafana/d/x")
	b, _ := json.Marshal(out)
	var keys map[string]any
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range keys {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"degraded", "errored", "failed", "mode", "passed", "run_id", "status", "total"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("a compare run's relayed report has keys %v, want exactly %v", got, want)
	}
	if strings.Contains(string(b), "CHN-") || strings.Contains(string(b), "tr-1") || strings.Contains(string(b), "obj-1") {
		t.Errorf("per-scenario data reached the builder: %s", b)
	}
}

// ── retention hook ───────────────────────────────────────────────────────────────────────────

func TestOutputRetention_PrunesOnlyAfterACompareResultsPush_RecordingFake(t *testing.T) {
	root := t.TempDir()
	base := argus.OutputsBase(filepath.Join(root, "local"))
	now := time.Now()
	for i := 1; i <= 23; i++ {
		d := filepath.Join(base, "r"+string(rune('a'+i-1)))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(d, ts, ts)
	}
	var calls []string
	hook := OutputRetention(ExecConfig{ResultsRoot: root}, func(p string) error { calls = append(calls, p); return nil })

	hook(federation.ResultsPush{RunID: "not-a-compare-run"}) // not a compare push (no outputs, no stored directory): nothing
	if len(calls) != 0 {
		t.Fatalf("a push with no outputs pruned %v", calls)
	}
	hook(federation.ResultsPush{RunID: "rw", Outputs: json.RawMessage(`[]`), OutputsRoot: "x"})
	if len(calls) != 3 {
		t.Fatalf("a compare push must prune all but the newest 20 of 23: removed %v", calls)
	}
	for _, c := range calls {
		if filepath.Dir(c) != base {
			t.Errorf("the remover was given %q, outside the outputs directory", c)
		}
	}
	for i := 1; i <= 23; i++ {
		if _, err := os.Stat(filepath.Join(base, "r"+string(rune('a'+i-1)))); err != nil {
			t.Fatalf("the recording fake must delete nothing: %v", err)
		}
	}
}

func TestExecutor_AfterPushHookRunsOnlyOnASuccessfulPush(t *testing.T) {
	dir := t.TempDir()
	priv, err := LoadOrCreateKey(filepath.Join(dir, "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	e := &Executor{PushRetries: 0, Backoff: time.Millisecond, Log: func(string, ...any) {},
		AfterPush: func(p federation.ResultsPush) { got = append(got, p.RunID) }}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	e.Client = NewClient(bad.URL, "inst", priv)
	e.pushWithRetry(context.Background(), federation.ResultsPush{RunID: "r-bad", Status: "completed"})
	if len(got) != 0 {
		t.Fatalf("the hook ran after a FAILED push: %v", got)
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ok.Close()
	e.Client = NewClient(ok.URL, "inst", priv)
	e.pushWithRetry(context.Background(), federation.ResultsPush{RunID: "r-ok", Status: "completed"})
	if len(got) != 1 || got[0] != "r-ok" {
		t.Fatalf("the hook did not run exactly once after a successful push: %v", got)
	}
}

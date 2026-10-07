package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// TestMaterialize_EmptyScenarioSet is the regression for the M3-alpha JOIN finding (2026-07-14): a run
// whose scenario set is empty (a run-request whose scenario_ref matched nothing) must still materialize a
// clean empty dir, not fail the temp→final rename with "no such file or directory".
func TestMaterialize_EmptyScenarioSet(t *testing.T) {
	base := t.TempDir()
	dir, cleanup, failures, err := materialize(base, "20260714T100412", nil, nil)
	if err != nil {
		t.Fatalf("materialize(empty) errored: %v", err)
	}
	defer cleanup()
	if len(failures) != 0 {
		t.Fatalf("an empty scenario set must never report an open failure: %+v", failures)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("materialized dir missing: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected an empty materialized dir, got %d entries", len(entries))
	}
}

// TestMaterialize_WritesScenarioFilesAtomically confirms the normal path: bodies land at their registry
// path under the final dir, and no ".tmp" sibling remains.
func TestMaterialize_WritesScenarioFilesAtomically(t *testing.T) {
	base := t.TempDir()
	scs := []federation.ScenarioPayload{
		{Path: "http-ingestion/JOIN-001.md", Body: "# JOIN-001\nbody"},
		{Path: "error-path/ERRP-001.md", Body: "# ERRP-001\nbody"},
	}
	dir, cleanup, failures, err := materialize(base, "runX", scs, nil)
	if err != nil {
		t.Fatalf("materialize errored: %v", err)
	}
	defer cleanup()
	if len(failures) != 0 {
		t.Fatalf("a clear-body scenario set must never report an open failure: %+v", failures)
	}
	for _, sc := range scs {
		if _, err := os.Stat(filepath.Join(dir, sc.Path)); err != nil {
			t.Errorf("scenario %s not materialized: %v", sc.Path, err)
		}
	}
	if _, err := os.Stat(dir + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp dir was not renamed away")
	}
}

// R5-3 (2026-07-22, owner remark "why does Memstore have two runs, I requested one"): a CLOUD run wrote
// its report under /results/<registered-instance-id>/, but the in-env MCP surface (runner__get_report /
// get_sagas / get_tail_logs) reads /results/<TOOL identity>/ — which is "local", because the runner__*
// tools VALIDATE instance_id:"local". So the cloud run's per-scenario evidence was invisible to the
// test agent, which compensated by firing a SECOND, direct run to obtain it — two runs in the ledger
// for one request. The run must land where the tools read it; the telemetry label keeps the real id.
func TestExecEnv_ReportLandsWhereTheMCPToolsRead(t *testing.T) {
	cfg := ExecConfig{
		InstanceID: "memstore-compose-v1", ToolInstance: "local",
		ResultsRoot: "/results", Grafana: "http://g:3000",
	}
	env := execEnv(cfg, "/scen")
	if env.Instance != "local" {
		t.Errorf("results identity must be the TOOL identity the MCP surface reads (%q), got %q", "local", env.Instance)
	}
	if env.ObsInstance != "memstore-compose-v1" {
		t.Errorf("telemetry label must stay the REGISTERED id, got %q", env.ObsInstance)
	}
	// an unset ToolInstance must not silently write to /results/ — default to the tool identity.
	if e2 := execEnv(ExecConfig{InstanceID: "x-v1", ResultsRoot: "/results"}, "/s"); e2.Instance != "local" {
		t.Errorf("an empty ToolInstance must default to \"local\", got %q", e2.Instance)
	}
}

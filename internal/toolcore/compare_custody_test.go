package toolcore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// ARGUS-CMP-3 custody: the recorded outputs belong to the author and to the control
// plane's digest ledger. The product hat never gets them, and a compare run sends no per-check
// telemetry.

func reportWithOutputs(mode string) *report.Report {
	h := strings.Repeat("e", 64)
	return &report.Report{RunID: "run-cu", Mode: mode, Summary: report.Summary{Passed: 1, Total: 1},
		Layers: []report.Layer{{Layer: "Permissions", Scenarios: []report.ScenarioResult{{
			ID: "CHN-A", Status: "passed",
			Outputs: []compare.OutputRecord{{V: 1, Step: "create", Sample: 1, State: compare.StateRecorded, Status: 200, Hash: h,
				Parts: compare.Parts{Status: h, Body: h}, BodyKind: compare.KindJSON}},
		}}}}}
}

func TestRedactExpected_NilsOutputsByNameForTheProductHat(t *testing.T) {
	rep := reportWithOutputs("build")
	redactExpected(rep, role.Product)
	if rep.Layers[0].Scenarios[0].Outputs != nil {
		t.Errorf("the product hat kept the recorded outputs: %+v", rep.Layers[0].Scenarios[0].Outputs)
	}
	rep = reportWithOutputs("build")
	redactExpected(rep, role.Test)
	if len(rep.Layers[0].Scenarios[0].Outputs) != 1 {
		t.Errorf("the test (author) hat must keep the digests")
	}
}

func TestGetReport_ACompareRunIsVerdictOnlyForTheProductHatAndWholeForTheAuthor(t *testing.T) {
	root := t.TempDir()
	e := Env{ResultsRoot: root, Instance: "local"}
	dir := filepath.Join(root, "local", "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(reportWithOutputs("compare"))
	if err := os.WriteFile(filepath.Join(dir, "run-cu.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	p, _, err := GetReport(e, role.Product, "run-cu")
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := json.Marshal(p)
	if strings.Contains(string(pb), "CHN-A") || strings.Contains(string(pb), `"outputs"`) || strings.Contains(string(pb), strings.Repeat("e", 64)) {
		t.Errorf("a builder read per-check data of a compare run: %s", pb)
	}
	p, _, err = GetReport(e, role.Test, "run-cu")
	if err != nil {
		t.Fatal(err)
	}
	tb, _ := json.Marshal(p)
	if !strings.Contains(string(tb), `"outputs"`) {
		t.Errorf("the author's full report lost the digests: %s", tb)
	}
}

func TestRun_ModeCompareSendsNoPerCheckTelemetry_EveryOtherModeStillDoes(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	for _, tc := range []struct {
		mode      string
		wantPush  bool
		wantEvent bool
	}{
		{"compare", false, false},
		{"build", true, true},
		{"final", true, true},
		{"", true, true},
	} {
		var metricHits, lokiHits int32
		pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&metricHits, 1)
			w.WriteHeader(200)
		}))
		lk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&lokiHits, 1)
			w.WriteHeader(http.StatusNoContent)
		}))
		cfg := "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n"
		e := rlEnv(t, cfg, map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
		e.ResultsRoot = t.TempDir()
		e.Instance = "local"
		e.Pushgateway = pg.URL
		e.Loki = lk.URL
		e.RunMode = tc.mode
		if _, _, err := Run(e, "", "", "", ""); err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		pg.Close()
		lk.Close()
		if (metricHits > 0) != tc.wantPush || (lokiHits > 0) != tc.wantEvent {
			t.Errorf("mode %q: pushgateway hits %d (want push=%v), loki hits %d (want events=%v)",
				tc.mode, metricHits, tc.wantPush, lokiHits, tc.wantEvent)
		}
	}
}

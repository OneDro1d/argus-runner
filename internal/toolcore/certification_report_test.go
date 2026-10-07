package toolcore

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/role"
)

// (#414), part c: a product-hat read of an on-disk report withholds every per-scenario
// field of a run that is not a build run — the in-env MCP tool and `argus get-report` both reach this
// function. The test hat is unchanged. An unknown or empty mode fails closed.
func certReport(mode string) *report.Report {
	return &report.Report{
		Project: "p", RunID: "run-CERT", Mode: mode, Note: "n",
		Summary: report.Summary{Total: 2, Passed: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "http", Scenarios: []report.ScenarioResult{
			{ID: "CERT-001", Status: "failed", CorrelationID: "tr-run-CERT-CERT-001-abc", Failure: &report.Failure{}},
			{ID: "CERT-002", Status: "passed", CorrelationID: "tr-run-CERT-CERT-002-abc"},
		}}},
		BuildRecordObjectID: "obj-1",
	}
}

func TestGetReport_ProductHatNeverSeesAScenarioOfANonBuildRun(t *testing.T) {
	for _, mode := range []string{"final", "scheduled", "rehearsal", "", "mystery"} {
		for _, viaRunFile := range []bool{true, false} {
			name := "mode=" + mode
			if viaRunFile {
				name += "/per-run file"
			} else {
				name += "/latest file"
			}
			t.Run(name, func(t *testing.T) {
				e := Env{Instance: "local", ResultsRoot: t.TempDir()}
				path, runArg := e.reportPath(), ""
				if viaRunFile {
					path, runArg = runReportPath(e.resultsDir(), "run-CERT"), "run-CERT"
				}
				if err := writeReport(path, certReport(mode)); err != nil {
					t.Fatal(err)
				}
				got, _, err := GetReport(e, role.Product, runArg)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(got)
				for _, leak := range []string{"CERT-001", "CERT-002", "tr-run-CERT", "obj-1"} {
					if strings.Contains(string(b), leak) {
						t.Errorf("a product-hat read of a %q report leaks %q: %s", mode, leak, b)
					}
				}
				rep, ok := got.(*report.Report)
				if !ok || rep.Summary.Failed != 1 || rep.Summary.Total != 2 {
					t.Errorf("the verdict and tallies must survive; got %s", b)
				}
				// the test hat reads the same file whole
				full, _, err := GetReport(e, role.Test, runArg)
				if err != nil {
					t.Fatal(err)
				}
				fb, _ := json.Marshal(full)
				if !strings.Contains(string(fb), "CERT-001") {
					t.Errorf("the test hat must still get the full report: %s", fb)
				}
			})
		}
	}
}

// build and the local-run stamp ("ci") stay whole for the product hat: the builder's own runs.
func TestGetReport_ProductHatStillSeesItsOwnRuns(t *testing.T) {
	for _, mode := range []string{"build", "ci"} {
		e := Env{Instance: "local", ResultsRoot: t.TempDir()}
		if err := writeReport(e.reportPath(), certReport(mode)); err != nil {
			t.Fatal(err)
		}
		got, _, err := GetReport(e, role.Product, "")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(got)
		if !strings.Contains(string(b), "CERT-001") {
			t.Errorf("a %q run is the builder's own: its scenarios must stay visible: %s", mode, b)
		}
	}
}

// The run records its REAL mode on the report it writes (Env.RunMode), before the file is visible to any
// reader; a run with no mode (a local or direct run) keeps the "ci" stamp.
func TestRun_StampsTheRealModeOnTheReportItWrites(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	for _, tc := range []struct{ runMode, want string }{{"final", "final"}, {"build", "build"}, {"", "ci"}} {
		e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n",
			map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
		e.ResultsRoot = t.TempDir()
		e.Instance = "local"
		e.RunMode = tc.runMode
		if _, _, err := Run(e, "run-stamp", "", "", ""); err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, p := range []string{e.reportPath(), runReportPath(e.resultsDir(), "run-stamp")} {
			rep, err := readReport(p)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Mode != tc.want {
				t.Errorf("RunMode %q: %s records mode %q, want %q", tc.runMode, p, rep.Mode, tc.want)
			}
		}
	}
}

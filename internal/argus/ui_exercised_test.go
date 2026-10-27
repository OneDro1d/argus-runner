package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// "the SUT was exercised" is read from Playwright's own JSON report
// (argus-out/results.json, stats), not from whether a per-test directory exists, because a spec that
// turns tracing off leaves no directory. These tests run the REAL uiRun and the REAL uiExercised and
// stand in only for the browser (the uiProcess seam): the stand-in writes the files Playwright would
// have written, so no browser, no node and no network is involved.

// uiFiles is what one fake Playwright run leaves behind. A nil field writes nothing.
type uiFiles struct {
	resultsJSON *string // the body of argus-out/results.json
	perTestDir  bool    // a per-test directory under test-results/ (what tracing on leaves)
	lastRunOnly bool    // test-results/.last-run.json alone (a harness failure)
}

func statsJSON(expected, unexpected, flaky, skipped string) *string {
	s := `{"config":{},"suites":[],"errors":[],"stats":{"startTime":"2026-10-11T00:00:00.000Z","duration":5,"expected":` +
		expected + `,"skipped":` + skipped + `,"unexpected":` + unexpected + `,"flaky":` + flaky + `}}`
	return &s
}

// fakeBrowser swaps uiProcess for one that writes `f` and exits `exit`. It returns the dir the run
// happens in. `stale`, when set, is written BEFORE the run, as a previous run would have left it.
func fakeBrowser(t *testing.T, exit int, f uiFiles, stale *string) string {
	t.Helper()
	dir := t.TempDir()
	if stale != nil {
		mustWrite(t, filepath.Join(dir, uiOutDirName, "results.json"), *stale)
	}
	orig := uiProcess
	t.Cleanup(func() { uiProcess = orig })
	uiProcess = func(vendorDir string, parts, env []string) (int, string) {
		if f.resultsJSON != nil {
			mustWrite(t, filepath.Join(vendorDir, uiOutDirName, "results.json"), *f.resultsJSON)
		}
		if f.perTestDir {
			if err := os.MkdirAll(filepath.Join(vendorDir, "test-results", "smoke-live"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if f.lastRunOnly {
			mustWrite(t, filepath.Join(vendorDir, "test-results", ".last-run.json"), `{}`)
		}
		return exit, "stderr line"
	}
	return dir
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// both ui doors: the standalone scenario and a one-step chain.
func runUIStandalone(dir string) string {
	s := &scenario.Scenario{ID: "UI-EX", Tags: []string{"ui"}, Expect: []string{"the dashboard renders"}}
	s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://localhost:5190"}`
	return runUIScenario(s, "tr-ex", dir).Status
}

func runUIChainStep(dir string) string {
	s := &scenario.Scenario{ID: "CHAIN-UI-EX", Tags: []string{"chain"}}
	s.Trigger.Payload = `{"steps":[{"type":"ui","name":"only","spec":"tests/live/x.spec.ts"}]}`
	res := runChainScenario(&config.Config{}, s, "tr-ex", dir)
	if len(res.Steps) != 1 {
		return "no-step"
	}
	// the chain executor names a step whose harness did not run `not-measured`, where the standalone
	// ui scenario names the same thing `error`: one verdict, two vocabularies.
	if res.Steps[0].Status == report.StepNotMeasured {
		return report.StatusError
	}
	return res.Steps[0].Status
}

func TestUIExercised_ReadFromPlaywrightsReport(t *testing.T) {
	stale := statsJSON("3", "0", "0", "0") // an earlier run that DID run tests
	cases := []struct {
		name  string
		exit  int
		files uiFiles
		stale *string
		want  string
	}{
		// the finding: tracing off, the test passed, no per-test directory
		{"tracing-off pass", 0, uiFiles{resultsJSON: statsJSON("1", "0", "0", "0")}, nil, "passed"},
		{"tracing-off failure", 1, uiFiles{resultsJSON: statsJSON("0", "1", "0", "0")}, nil, "failed"},
		{"flaky counts as ran", 0, uiFiles{resultsJSON: statsJSON("0", "0", "1", "0")}, nil, "passed"},
		// nothing was exercised: still the execution failure, as today
		{"exit 0, zero tests", 0, uiFiles{resultsJSON: statsJSON("0", "0", "0", "0")}, nil, report.StatusError},
		{"all skipped", 0, uiFiles{resultsJSON: statsJSON("0", "0", "0", "2")}, nil, report.StatusError},
		{"exit 1, zero tests", 1, uiFiles{resultsJSON: statsJSON("0", "0", "0", "0")}, nil, report.StatusError},
		// the report says nothing ran, a leftover directory does not outvote it
		{"report says none, a directory exists", 0, uiFiles{resultsJSON: statsJSON("0", "0", "0", "1"), perTestDir: true}, nil, report.StatusError},
		// THIS run's file only: an earlier run's report plus a run that produced nothing
		{"stale report, run produced nothing", 0, uiFiles{}, stale, report.StatusError},
		{"stale report, run produced nothing, exit 1", 1, uiFiles{}, stale, report.StatusError},
		{"stale report, run wrote its own zero-test report", 0, uiFiles{resultsJSON: statsJSON("0", "0", "0", "0")}, stale, report.StatusError},
		// no report: today's rule, byte for byte
		{"no report, per-test dir, exit 0", 0, uiFiles{perTestDir: true}, nil, "passed"},
		{"no report, per-test dir, exit 1", 1, uiFiles{perTestDir: true}, nil, "failed"},
		{"no report, nothing, exit 0", 0, uiFiles{}, nil, report.StatusError},
		{"no report, nothing, exit 1", 1, uiFiles{}, nil, report.StatusError},
		{"no report, .last-run.json only", 1, uiFiles{lastRunOnly: true}, nil, report.StatusError},
		// an unreadable report is no report
		{"unreadable report + per-test dir", 0, uiFiles{resultsJSON: ptr(`{not json`), perTestDir: true}, nil, "passed"},
		{"unreadable report, nothing else", 0, uiFiles{resultsJSON: ptr(`{not json`)}, nil, report.StatusError},
		{"report without stats + per-test dir", 0, uiFiles{resultsJSON: ptr(`{"suites":[]}`), perTestDir: true}, nil, "passed"},
		{"report without stats, nothing else", 0, uiFiles{resultsJSON: ptr(`{"suites":[]}`)}, nil, report.StatusError},
	}
	doors := []struct {
		name string
		run  func(dir string) string
	}{{"standalone ui scenario", runUIStandalone}, {"chain ui step", runUIChainStep}}
	for _, d := range doors {
		for _, c := range cases {
			t.Run(d.name+"/"+c.name, func(t *testing.T) {
				dir := fakeBrowser(t, c.exit, c.files, c.stale)
				if got := d.run(dir); got != c.want {
					t.Fatalf("status = %q; want %q", got, c.want)
				}
			})
		}
	}
}

func ptr(s string) *string { return &s }

// uiRun itself removes the previous run's report before it starts the process — the line the chain
// `ui` step depends on, because the chain step clears nothing else in argus-out/.
func TestUIRun_RemovesTheEarlierReportBeforeStartingTheProcess(t *testing.T) {
	dir := t.TempDir()
	rp := filepath.Join(dir, uiOutDirName, "results.json")
	mustWrite(t, rp, *statsJSON("3", "0", "0", "0"))
	mustWrite(t, filepath.Join(dir, uiOutDirName, "assertions.json"), `[]`)
	var sawAtStart bool
	orig := uiProcess
	t.Cleanup(func() { uiProcess = orig })
	uiProcess = func(vendorDir string, parts, env []string) (int, string) {
		_, err := os.Stat(rp)
		sawAtStart = err == nil
		return 0, ""
	}
	_, exercised, _ := uiRun(dir, []string{"tests/live/x.spec.ts"}, "http://localhost:5190", "tr-x", nil, "", 0)
	if sawAtStart {
		t.Fatal("the earlier results.json was still there when the process started")
	}
	if exercised {
		t.Fatal("a run that wrote nothing was read as exercised on the strength of an earlier report")
	}
	// only results.json is uiRun's to clear: assertions.json is runUIScenario's (outside the seam).
	if _, err := os.Stat(filepath.Join(dir, uiOutDirName, "assertions.json")); err != nil {
		t.Fatalf("uiRun removed more than results.json: %v", err)
	}
}

// The ordering the standalone door relies on: runUIScenario clears argus-out/ (assertions.json
// included) BEFORE uiRun, which clears test-results/ and results.json BEFORE the process starts.
func TestRunUIScenario_ClearOrder(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, uiOutDirName, "results.json"), *statsJSON("3", "0", "0", "0"))
	mustWrite(t, filepath.Join(dir, uiOutDirName, "assertions.json"), `[{"bullet":"x","ok":true}]`)
	mustWrite(t, filepath.Join(dir, "test-results", "old-run", "trace.zip"), "z")
	var left []string
	orig := uiProcess
	t.Cleanup(func() { uiProcess = orig })
	uiProcess = func(vendorDir string, parts, env []string) (int, string) {
		for _, p := range []string{
			filepath.Join(vendorDir, uiOutDirName, "results.json"),
			filepath.Join(vendorDir, uiOutDirName, "assertions.json"),
			filepath.Join(vendorDir, "test-results"),
		} {
			if _, err := os.Stat(p); err == nil {
				left = append(left, filepath.Base(p))
			}
		}
		return 0, ""
	}
	if got := runUIStandalone(dir); got != report.StatusError {
		t.Fatalf("a run that wrote nothing read %q", got)
	}
	if len(left) != 0 {
		t.Fatalf("earlier evidence still present when the process started: %s", strings.Join(left, ", "))
	}
}

// Classify's words: the error sentence no longer says "artifact" (the meaning is "no test ran"), and
// the failed sentence does not promise a trace that tracing-off never wrote.
func TestUIVerdictSentences_DoNotPromiseAFile(t *testing.T) {
	dir := fakeBrowser(t, 1, uiFiles{resultsJSON: statsJSON("0", "1", "0", "0")}, nil)
	s := &scenario.Scenario{ID: "UI-S", Tags: []string{"ui"}, Expect: []string{"the dashboard renders"}}
	s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://localhost:5190"}`
	res := runUIScenario(s, "tr-s", dir)
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "if tracing was on") {
		t.Fatalf("failed sentence must make trace.zip conditional: %+v", res.Failure)
	}
	dir = fakeBrowser(t, 0, uiFiles{}, nil)
	res = runUIScenario(s, "tr-s", dir)
	if res.Failure == nil || strings.Contains(res.Failure.Observed, "artifact") || !strings.Contains(res.Failure.Observed, "EXECUTION failure (no Playwright test ran") {
		t.Fatalf("error sentence: %+v", res.Failure)
	}
}

// The vendored config must keep writing the report uiExercised reads, at the path it reads.
func TestVendoredConfig_WritesTheReportUIExercisedReads(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testkit", "ui", "playwright.config.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "['json', { outputFile: '"+uiOutDirName+"/results.json' }]") {
		t.Fatal("playwright.config.ts no longer writes argus-out/results.json: uiExercised would silently fall back to the per-test directory")
	}
}

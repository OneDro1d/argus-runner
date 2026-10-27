package argus

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// moneyallow_cleanup_ui_fault_run_test.go — at the RUN door, on the REAL path: with the entry the
// real cleanup runner / the ui step / the fault step is reached; without it nothing is executed. Nothing here
// starts a browser or touches a cluster: uiRun and the fault Kube/Toxi are the existing fakes.

func moneyCfgWith(allow ...config.MoneyWriteAllowEntry) *config.Config {
	c := httpConfig()
	c.MoneyHandling = true
	if len(allow) > 0 {
		c.MoneyWrites = &config.MoneyWritesConfig{Allow: allow}
	}
	return c
}

func isMoneyRefusal(res *report.ScenarioResult) bool {
	return res != nil && res.Failure != nil && strings.HasPrefix(res.Failure.Observed, MoneyGuardRefusal)
}

// ── cleanup ───────────────────────────────────────────────────────────────────────────────────────

func runCleanupRow(t *testing.T, cleanup string, c *config.Config) (*report.ScenarioResult, *recordingCleanupRunner) {
	t.Helper()
	dir := t.TempDir()
	sc := writeCleanupScenario(t, dir, cleanup)
	r := &recordingCleanupRunner{fakeRunner: fakeRunner{pass: map[string]bool{"CLN-001": true}}}
	rr, err := RunAll(c, sc, dir+"/results", "p", "", "", "", "", r)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("CLN-001")
	if res == nil {
		t.Fatal("scenario missing from the report")
	}
	return res, r
}

func TestMoneyAllowCleanup_RunDoor_RealRunnerIsReachedOnlyWithTheEntryOfThatForm(t *testing.T) {
	sqlBlock := "```sql\nDELETE FROM t WHERE c = 1;\n```"
	bashBlock := "```bash\nrm -f /tmp/marker\n```"
	both := sqlBlock + "\n\n" + bashBlock
	sqlE := config.MoneyWriteAllowEntry{Engine: "cleanup", Form: "sql"}
	bashE := config.MoneyWriteAllowEntry{Engine: "cleanup", Form: "bash"}
	for _, tc := range []struct {
		name      string
		cleanup   string
		allow     []config.MoneyWriteAllowEntry
		wantCalls []string
		refused   bool
	}{
		{"sql block, sql entry", sqlBlock, []config.MoneyWriteAllowEntry{sqlE}, []string{"sql:DELETE FROM t WHERE c = 1;"}, false},
		{"bash block, bash entry", bashBlock, []config.MoneyWriteAllowEntry{bashE}, []string{"bash:rm -f /tmp/marker"}, false},
		{"both blocks, both entries (in order)", both, []config.MoneyWriteAllowEntry{sqlE, bashE},
			[]string{"sql:DELETE FROM t WHERE c = 1;", "bash:rm -f /tmp/marker"}, false},
		{"sql block, only the bash entry", sqlBlock, []config.MoneyWriteAllowEntry{bashE}, nil, true},
		{"both blocks, only the sql entry", both, []config.MoneyWriteAllowEntry{sqlE}, nil, true},
		{"no entry at all", sqlBlock, nil, nil, true},
	} {
		res, r := runCleanupRow(t, tc.cleanup, moneyCfgWith(tc.allow...))
		if isMoneyRefusal(res) != tc.refused {
			t.Errorf("%s: refused = %v, want %v (%+v)", tc.name, isMoneyRefusal(res), tc.refused, res.Failure)
		}
		if strings.Join(r.calls, "|") != strings.Join(tc.wantCalls, "|") {
			t.Errorf("%s: the cleanup runner was handed %v, want %v", tc.name, r.calls, tc.wantCalls)
		}
		if !tc.refused {
			if res.Status != "passed" || len(res.Cleanup) != len(tc.wantCalls) {
				t.Errorf("%s: the scenario ran (%q) and each block is reported: %+v", tc.name, res.Status, res.Cleanup)
			}
			for _, cr := range res.Cleanup {
				if cr.Outcome != report.CleanupOK {
					t.Errorf("%s: an allowed cleanup reports %q", tc.name, cr.Outcome)
				}
			}
		}
	}
}

// An allowed cleanup DOES run after its scenario ran; it does NOT run when the scenario was refused for
// ANOTHER reason (an unlisted write), whatever cleanup entries the list holds.
func TestMoneyAllowCleanup_RunDoor_AnAllowedCleanupRunsAfterItsScenarioAndNotAfterAnotherRefusal(t *testing.T) {
	bashE := config.MoneyWriteAllowEntry{Engine: "cleanup", Form: "bash"}
	// the scenario ran: GET /health, cleanup allowed -> one call
	res, r := runCleanupRow(t, "```bash\nrm -f /tmp/marker\n```", moneyCfgWith(bashE))
	if res.Status != "passed" || len(r.calls) != 1 {
		t.Fatalf("the scenario must have run and cleaned up: status=%q calls=%v", res.Status, r.calls)
	}
	// refused for another reason: a POST nothing lists, with a cleanup the list DOES allow
	dir := t.TempDir()
	d := dir + "/scenarios/http-ingestion"
	mustMkdirAll(t, d)
	md := strings.Join([]string{"# Scenario: c", "", "## Metadata", "- **ID**: CLN-001", "- **Layer**: HTTP Ingestion", "- **Tags**: http, order", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/strategies`", "", "## EXPECT", "### Runnable", "- status=202", "",
		"## TIMEOUT", "30s", "", "## CLEANUP", "```bash\nrm -f /tmp/marker\n```", ""}, "\n")
	mustWriteFile(t, d+"/CLN-001.md", md)
	rec := &recordingCleanupRunner{fakeRunner: fakeRunner{pass: map[string]bool{"CLN-001": true}}}
	rr, err := RunAll(moneyCfgWith(bashE), dir+"/scenarios", dir+"/results", "p", "", "", "", "", rec)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	row, _ := rr.Report.Find("CLN-001")
	if !isMoneyRefusal(row) || !strings.Contains(row.Failure.Observed, "only GET may run") {
		t.Fatalf("precondition: the POST must be refused: %+v", row)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("a scenario refused for another reason ran its (allowed) cleanup: %v", rec.calls)
	}
	if len(row.Cleanup) != 1 || row.Cleanup[0].Outcome != report.CleanupNotRun || !strings.Contains(row.Cleanup[0].Observed, "refused before firing") {
		t.Errorf("the block must be recorded as not run, with the reason: %+v", row.Cleanup)
	}
}

// ── ui ────────────────────────────────────────────────────────────────────────────────────────────

const runDoorSpec = "tests/live/argus-declared.spec.ts"

func writeUIRow(t *testing.T, dir, spec string) string {
	t.Helper()
	d := dir + "/scenarios/web-ui"
	mustMkdirAll(t, d)
	md := strings.Join([]string{"# Scenario: u", "", "## Metadata", "- **ID**: UI-001", "- **Layer**: Web UI", "- **Tags**: ui", "",
		"## TRIGGER", "POST `x`", "", "```json", `{"spec":"` + spec + `"}`, "```", "",
		"## EXPECT", "### Runnable", "- no console errors", "", "## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", ""}, "\n")
	mustWriteFile(t, d+"/UI-001.md", md)
	return dir + "/scenarios"
}

func fakeUIRun(t *testing.T) *[]string {
	t.Helper()
	orig := uiRun
	t.Cleanup(func() { uiRun = orig })
	var specs []string
	uiRun = func(_ string, s []string, _, _ string, _ []scenario.UIAssert, _ string, _ int) (int, bool, string) {
		specs = append(specs, strings.Join(s, " "))
		return 0, true, ""
	}
	return &specs
}

func TestMoneyAllowUI_RunDoor_ScenarioReachesTheDriverOnlyWithTheEntryForItsSpec(t *testing.T) {
	uiE := config.MoneyWriteAllowEntry{Engine: "ui", Spec: runDoorSpec}
	for _, tc := range []struct {
		name    string
		spec    string
		allow   []config.MoneyWriteAllowEntry
		driven  bool
		refused bool
	}{
		{"the entry's spec", runDoorSpec, []config.MoneyWriteAllowEntry{uiE}, true, false},
		{"another spec", "tests/live/other.spec.ts", []config.MoneyWriteAllowEntry{uiE}, false, true},
		{"no entry", runDoorSpec, nil, false, true},
	} {
		specs := fakeUIRun(t)
		dir := t.TempDir()
		sc := writeUIRow(t, dir, tc.spec)
		rr, err := RunAll(moneyCfgWith(tc.allow...), sc, filepath.Join(dir, "results"), "p", "", "", "", "", &fakeRunner{})
		if err != nil {
			t.Fatalf("RunAll: %v", err)
		}
		res, _ := rr.Report.Find("UI-001")
		if res == nil {
			t.Fatalf("%s: no row", tc.name)
		}
		if isMoneyRefusal(res) != tc.refused {
			t.Errorf("%s: refused = %v, want %v (%+v)", tc.name, isMoneyRefusal(res), tc.refused, res.Failure)
		}
		if (len(*specs) == 1) != tc.driven || len(*specs) > 1 {
			t.Errorf("%s: the driver was handed %v, driven want %v", tc.name, *specs, tc.driven)
		}
		if tc.driven && (*specs)[0] != tc.spec {
			t.Errorf("%s: the driver got %q, want %q", tc.name, (*specs)[0], tc.spec)
		}
	}
}

func TestMoneyAllowUI_RunDoor_ChainStepReachesTheDriverOnlyWithTheEntryForItsSpec(t *testing.T) {
	uiE := config.MoneyWriteAllowEntry{Engine: "ui", Spec: runDoorSpec}
	for _, tc := range []struct {
		name   string
		spec   string
		allow  []config.MoneyWriteAllowEntry
		driven bool
	}{
		{"the entry's spec", runDoorSpec, []config.MoneyWriteAllowEntry{uiE}, true},
		{"another spec", "tests/live/other.spec.ts", []config.MoneyWriteAllowEntry{uiE}, false},
		{"no entry", runDoorSpec, nil, false},
	} {
		specs := fakeUIRun(t)
		s := scenario.Parse(chainRunMD("http://unused.invalid", `{"steps":[{"type":"ui","name":"u","spec":"`+tc.spec+`"}]}`, "### Runnable\n- step u: status=200\n"))
		res := runOneScenario(moneyCfgWith(tc.allow...), scenarioFile{s: s}, "tr-ui-1", t.TempDir(), &fakeRunner{},
			moneyCfgWith(tc.allow...).MoneyWriteAllowlist(), &scenario.MoneySpendLedger{}, "", nil)
		if isMoneyRefusal(&res) == tc.driven {
			t.Errorf("%s: refused = %v with driven = %v (%+v)", tc.name, isMoneyRefusal(&res), tc.driven, res.Failure)
		}
		if (len(*specs) == 1) != tc.driven || len(*specs) > 1 {
			t.Errorf("%s: the driver was handed %v, driven want %v", tc.name, *specs, tc.driven)
		}
	}
}

// ── fault ─────────────────────────────────────────────────────────────────────────────────────────

func TestMoneyAllowFault_RunDoor_StepReachesTheClusterFakeOnlyWithTheEntry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow []config.MoneyWriteAllowEntry
		hit   bool
	}{
		{"with the fault entry", []config.MoneyWriteAllowEntry{{Engine: "fault"}}, true},
		{"with entries of other kinds", []config.MoneyWriteAllowEntry{{Engine: "logs"}, {Engine: "ui", Spec: runDoorSpec}}, false},
		{"without a list", nil, false},
	} {
		k, _ := withFakes(t)
		srv := sutFor(k)
		defer srv.Close()
		c := faultCfg()
		c.MoneyHandling = true
		if len(tc.allow) > 0 {
			c.MoneyWrites = &config.MoneyWritesConfig{Allow: tc.allow}
		}
		s := scenario.Parse(chainRunMD(srv.URL, stopReadTrig, "### Runnable\n- step while-down: status=503\n"))
		res := runOneScenario(c, scenarioFile{s: s}, "tr-fault-money", t.TempDir(), &fakeRunner{},
			c.MoneyWriteAllowlist(), &scenario.MoneySpendLedger{}, "", nil)
		if tc.hit {
			if isMoneyRefusal(&res) || res.Status != "passed" {
				t.Errorf("%s: want the chain run: %q %+v", tc.name, res.Status, res.Failure)
			}
			if len(k.calls) == 0 {
				t.Errorf("%s: the cluster fake was never reached", tc.name)
			}
			if k.up() != 2 {
				t.Errorf("%s: the deployment was left at %d replicas", tc.name, k.up())
			}
			continue
		}
		if !isMoneyRefusal(&res) || res.Status != "error" {
			t.Errorf("%s: want a money refusal, got %q %+v", tc.name, res.Status, res.Failure)
		}
		if len(k.calls) != 0 {
			t.Errorf("%s: a refused fault step touched the cluster fake: %v", tc.name, k.calls)
		}
	}
}

// What a fault may touch is still bounded by fault_allowed_targets: the entry does not widen it.
func TestMoneyAllowFault_RunDoor_TheEntryDoesNotWidenFaultAllowedTargets(t *testing.T) {
	k, _ := withFakes(t)
	srv := sutFor(k)
	defer srv.Close()
	c := faultCfg()
	c.FaultAllowedTargets.Deployments = map[string]config.FaultDeployment{"other": {Namespace: "calm-demo"}} // "trades" is no longer listed
	c.MoneyHandling = true
	c.MoneyWrites = &config.MoneyWritesConfig{Allow: []config.MoneyWriteAllowEntry{{Engine: "fault"}}}
	s := scenario.Parse(chainRunMD(srv.URL, stopReadTrig, "### Runnable\n- step while-down: status=503\n"))
	res := runOneScenario(c, scenarioFile{s: s}, "tr-fault-money2", t.TempDir(), &fakeRunner{},
		c.MoneyWriteAllowlist(), &scenario.MoneySpendLedger{}, "", nil)
	if isMoneyRefusal(&res) {
		t.Fatalf("precondition: the money guard lets the step through: %+v", res.Failure)
	}
	if res.Status == "passed" || len(k.calls) != 0 {
		t.Errorf("the fault preflight must still refuse an unlisted deployment: status=%q calls=%v failure=%+v", res.Status, k.calls, res.Failure)
	}
}

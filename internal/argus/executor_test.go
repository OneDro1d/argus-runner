package argus

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

func httpConfig() *config.Config {
	c := &config.Config{}
	c.Project.Name = "p"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	return c
}

// execErrorRunner fails to LAUNCH JMeter (the rig is down) for every scenario.
type execErrorRunner struct{ msg string }

func (r execErrorRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	return errors.New(r.msg)
}

// RO-04: a down executor must produce `errored` scenarios, never N SUT `failed`.
func TestRunAll_ExecutorDown_ErroredNotFailed(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=202")

	r := execErrorRunner{msg: `jmeter exec failed: exit status 1: service "jmeter" is not running`}
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Report.Summary.Errored != 2 || rr.Report.Summary.Failed != 0 {
		t.Fatalf("executor-down must be errored (not failed): %+v", rr.Report.Summary)
	}
	res, _ := rr.Report.Find("ORD-001")
	if res == nil || res.Status != report.StatusError {
		t.Fatalf("ORD-001 should be errored, got %+v", res)
	}
	// errored = harness failure, NOT a SUT mismatch — must not carry an echoed expected.
	if res.Failure != nil && res.Failure.Expected != nil {
		t.Error("an errored (harness) scenario must not carry an `expected`")
	}
}

// RO-04 fix (adversary gate): the JMeter preflight must NOT abort a run whose scenarios all
// run on native runners (mcp/ui/chain). allNeedJMeter gates the short-circuit.
func TestAllNeedJMeter(t *testing.T) {
	mk := func(tags ...string) []scenarioFile {
		return []scenarioFile{{s: &scenario.Scenario{ID: "X-1", Tags: tags}}}
	}
	if !allNeedJMeter(mk()) { // an untagged (http) scenario needs JMeter
		t.Error("an untagged scenario needs JMeter")
	}
	for _, tag := range []string{MCPTag, UITag, ChainTag} {
		if allNeedJMeter(mk(tag)) {
			t.Errorf("a %s-only run must NOT need the JMeter preflight", tag)
		}
	}
	if allNeedJMeter(nil) {
		t.Error("an empty run does not need JMeter")
	}
}

// healthFailRunner would PASS every scenario, but its preflight reports the rig down.
type healthFailRunner struct{ *fakeRunner }

func (healthFailRunner) Healthcheck() error {
	return errors.New("executor unavailable: bring up the stack (docker compose up -d)")
}

// RO-04: a failing runtime preflight short-circuits the whole run to `errored` — it never
// runs blind against a dead rig and calls it a pass/fail.
func TestRunAll_PreflightFail_ShortCircuitsErrored(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "http-ingestion", "ORD-001", "status=202")
	writeScenario(t, scDir, "http-ingestion", "ORD-002", "status=202")

	r := healthFailRunner{&fakeRunner{pass: map[string]bool{"ORD-001": true, "ORD-002": true}}}
	rr, err := RunAll(httpConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Report.Summary.Errored != 2 || rr.Report.Summary.Passed != 0 {
		t.Fatalf("preflight failure must short-circuit to errored (not run): %+v", rr.Report.Summary)
	}
}

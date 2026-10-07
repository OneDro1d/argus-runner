package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// GETTING-STARTED tells a tester to run `argus doctor --control-plane <url>` with no
// --scenarios. On a machine with no bundled demo folder that FAILED the scenarios-dir check only because
// the flag was missing, and its hint named a folder (examples/<product>/scenarios) that an onboarded
// kit does not have (it has test-agent/scenarios). A flag nobody passed is "not checked", not a failure.
func TestDoctor_NoScenariosFlagIsNotAFailureAndTheHintMatchesTheKit(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)
	// A directory with no demo folder in it: where a tester's shell is, not the repo.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	rep, rc, _ := runDoctor(t, "--control-plane", "https://argus-dev.onedroid.ai")
	c := checkByID(t, rep, "scenarios-dir")
	if c.Status == doctor.StatusFail {
		t.Errorf("scenarios-dir FAILED for a doctor run that was never given --scenarios: %+v", c)
	}
	if rc == exitFailed {
		t.Errorf("exit = exitFailed: the documented no-flag command must not fail on a missing --scenarios")
	}
	if !strings.Contains(c.Fix, "--scenarios") {
		t.Errorf("the fix must still say how to check scenarios: %q", c.Fix)
	}
	if strings.Contains(c.Fix, "examples/<product>/scenarios") {
		t.Errorf("the hint names a folder the onboarded kit does not have: %q", c.Fix)
	}
	if !strings.Contains(c.Fix, "test-agent/scenarios") {
		t.Errorf("the hint must name the kit's real folder, test-agent/scenarios: %q", c.Fix)
	}
}

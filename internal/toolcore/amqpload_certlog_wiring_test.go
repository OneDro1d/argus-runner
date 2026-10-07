package toolcore

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// (#447), the WIRING half: internal/argus pins that runAMQPLoad drops the scenario id on a
// certifying mode, but only if it is GIVEN the mode. Run is the one production caller, and it must pass
// Env.RunMode down (argus.RunAllWithMode). Passing "" there would leave every certification run logging
// the id while the argus-level test stayed green. This drives Run itself.
//
// No JMeter is reachable: JMeterLocal with an empty PATH, so the runner fails at once and nothing is
// dialled. The progress line is still written for the step, with its status.
const certLogCfg = "project:\n  name: lab\ntargets:\n  http:\n    base_url: http://sut.invalid\n" +
	"  message_broker_targets:\n    load-lab:\n      type: amqp\n      url: amqp://loaduser:lab-only@load-lab.invalid:5672/\n" +
	"      management_url: http://load-lab.invalid:15672\n      exchanges:\n        load: argus.load\n" +
	"load_allowed_targets:\n  load-lab:\n    max_sessions: 1000\n"

const certLogScenario = "# Scenario: AL-WIRE\n\n## Metadata\n- **ID**: AL-WIRE\n- **Layer**: AMQP Load\n- **Tags**: load\n- **Target**: load-lab\n\n" +
	"## TRIGGER\nPOST `/amqp-load`\n\n## EXPECT\n### Runnable\n- broker is not blocked\n- every step is measured\n\n" +
	"## LOAD\n- **Steps**: 2\n- **Step Duration Seconds**: 20\n- **Ramp Seconds**: 10\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n" +
	"\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A — the sampler deletes its queues.\n"

func runCertLog(t *testing.T, mode string) string {
	t.Helper()
	e := rlEnv(t, certLogCfg, map[string]string{"AL-WIRE": certLogScenario})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"
	e.JMeterLocal = true
	e.TemplatesDir = t.TempDir()
	e.RunMode = mode

	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) }()

	if _, _, err := Run(e, "20261002T120000000", "", "", ""); err != nil {
		t.Fatalf("mode %q: Run: %v", mode, err)
	}
	// Say why the row ended as it did: a refusal or preflight error before the step loop would mean no
	// progress line, which the positive control turns into a loud failure rather than a silent pass.
	if rep, rerr := readReport(e.reportPath()); rerr == nil {
		for _, l := range rep.Layers {
			for _, s := range l.Scenarios {
				if s.Failure != nil {
					t.Logf("mode %q: %s %s: %s", mode, s.ID, s.Status, s.Failure.Observed)
				}
			}
		}
	}
	return buf.String()
}

func TestRun_PassesTheRunModeToTheAMQPLoadLog(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	// A stub jmeter that does nothing: the runner's preflight finds it, every step "runs" and measures
	// nothing (no JTL), and no broker is dialled. The progress line is written per step regardless.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "jmeter"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// Positive control first: a build run writes the line WITH the id, so its absence below is the check
	// firing, not a line that was never written (or a scenario that never ran).
	out := runCertLog(t, "build")
	if !strings.Contains(out, "amqp-load AL-WIRE step") {
		t.Fatalf("build run: the progress line with the scenario id is missing, so this test cannot prove anything; log: %q", out)
	}
	out = runCertLog(t, "final")
	if !strings.Contains(out, "amqp-load") {
		t.Fatalf("final run: no amqp-load progress line at all; log: %q", out)
	}
	if strings.Contains(out, "AL-WIRE") {
		t.Errorf("final run through toolcore.Run logged the certification scenario id (is Env.RunMode passed to argus.RunAllWithMode?): %q", out)
	}
}

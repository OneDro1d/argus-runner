package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// a tester's machine, 2026-10-08: argus-config declared one target and most scenarios called two more engines by full URL.
// sut-reachable said "ready" — true of the one target the executor probes, silent about the others. Doctor
// must name the hosts the probe never dials (argus-config) and say "ready" does not cover them (sut-reachable).
func TestDoctor_ScenarioHostsTheProbeNeverDials(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)
	cfg := writeConfig(t, "project:\n  name: pilot\ntargets:\n  http:\n    base_url: http://pilot:8080\n")
	scen := t.TempDir()
	twin := func(id, second string) {
		body := strings.Join([]string{
			"# Scenario: " + id, "",
			"## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: chain", "",
			"## TRIGGER", "POST `chain`", "",
			"```json",
			`{"steps":[{"type":"http","name":"a","method":"POST","url":"${INGESTION_URL}/v1/run","body":{}},` +
				`{"type":"http","name":"b","method":"POST","url":"` + second + `","body":{}}]}`,
			"```", "",
			"## EXPECT", "### Runnable", "- step a: status=200", "- step b: status=200", "",
			"## TIMEOUT", "120s", "",
			"## CLEANUP", "N/A — creates nothing.", "",
		}, "\n")
		if err := os.WriteFile(filepath.Join(scen, id+".md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	twin("DM-VDIFF-001", "http://pilot-old.pilot.svc.cluster.local:8080/v1/run")
	twin("DM-VDIFF-002", "http://pilot-old.pilot.svc.cluster.local:8080/v1/run")
	twin("DM-DIFF-001", "http://pilot-go.pilot.svc.cluster.local:8080/v1/run")

	rep, _, _ := runDoctor(t, "--config", cfg, "--scenarios", scen, "--control-plane", "https://argus-dev.onedroid.ai")
	c := checkByID(t, rep, "argus-config")
	if c.Status != doctor.StatusWarn {
		t.Errorf("argus-config = %q, want warn: %s", c.Status, c.Detail)
	}
	for _, want := range []string{"3 scenarios", "pilot-old.pilot.svc.cluster.local:8080 (2 scenarios, e.g. DM-VDIFF-001)",
		"pilot-go.pilot.svc.cluster.local:8080 (1 scenario, e.g. DM-DIFF-001)"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("argus-config detail lacks %q: %s", want, c.Detail)
		}
	}
	s := checkByID(t, rep, "sut-reachable")
	if s.Status != doctor.StatusOK || !strings.Contains(s.Detail, "does not cover the 2 hosts") {
		t.Errorf("sut-reachable = %q %q, want ok with the 2-host caveat", s.Status, s.Detail)
	}
}

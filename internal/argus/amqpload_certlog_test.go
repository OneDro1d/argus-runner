package argus

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"
)

// (GitHub #447): the amqp-load progress line used to name the scenario id and the step
// outcome on EVERY mode. On a certification run that log reaches Loki, which a builder can query, and
// certification scenario ids are withheld from builders. The executor must not depend on the
// control plane's refusal of AMQP Load on those modes for this.

// runLoadWithMode runs one AMQP Load scenario in the given run mode and returns what the standard logger
// printed, with the logger restored.
func runLoadWithMode(t *testing.T, mode string) string {
	t.Helper()
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	c := loadCfg(t, loadConfig(t, allowLab))

	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) }()

	if _, err := RunAllWithMode(c, filepath.Join(e.dir, "scenarios"), e.results, "lab", "", "", "", "20261002T120000000", f, nil, mode); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) == 0 {
		t.Fatal("the scenario never ran, so no progress line could have been logged: the test proves nothing")
	}
	return buf.String()
}

// THE NAMED TEST of the wiring mutation: removing the mode check in runAMQPLoad, or passing "" instead of
// the real mode at the call site, must turn this red.
func TestAMQPLoad_CertificationRunLogsNoScenarioID(t *testing.T) {
	// Positive control: a mode that may show scenarios keeps the line, id included, so an absent id below
	// means the check fired and not that the line was never written.
	for _, mode := range []string{"build", "ci", ""} {
		out := runLoadWithMode(t, mode)
		if !strings.Contains(out, "amqp-load") || !strings.Contains(out, "AL-001") {
			t.Errorf("mode %q: the progress line must keep the scenario id; log was: %q", mode, out)
		}
	}
	// Certifying modes, and an unknown non-empty one (fail closed): the scenario id is absent.
	for _, mode := range []string{"final", "scheduled", "rehearsal", "some-future-mode"} {
		out := runLoadWithMode(t, mode)
		if strings.Contains(out, "AL-001") {
			t.Errorf("mode %q: the executor log names the certification scenario id: %q", mode, out)
		}
	}
}

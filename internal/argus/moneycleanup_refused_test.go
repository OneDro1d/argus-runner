package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// A scenario the money guard refused at the run door runs NO cleanup block.
// The scenario is a plain GET /health (the trigger is fine); only its ## CLEANUP is what the
// guard refuses, so the refusal is the one that used to leave the cleanup running.

func TestMoneyRefusedScenario_RunsNoCleanupBlock(t *testing.T) {
	dir := t.TempDir()
	sc := writeCleanupScenario(t, dir, "```sql\nDELETE FROM t WHERE c = 1;\n```\n\n```bash\nrm -f /tmp/marker\n```")
	r := &recordingCleanupRunner{fakeRunner: fakeRunner{pass: map[string]bool{"CLN-001": true}}}
	c := httpConfig()
	c.MoneyHandling = true
	rr, err := RunAll(c, sc, dir+"/results", "p", "", "", "", "", r)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("CLN-001")
	if res == nil {
		t.Fatal("scenario missing from the report")
	}
	if res.Status != report.StatusError || res.Failure == nil || !strings.HasPrefix(res.Failure.Observed, MoneyGuardRefusal) {
		t.Fatalf("precondition: the run door must refuse it: status=%q failure=%+v", res.Status, res.Failure)
	}
	if len(r.calls) != 0 {
		t.Fatalf("a scenario refused before firing must run NO cleanup block, but the runner executed: %v", r.calls)
	}
	if len(res.Cleanup) != 2 {
		t.Fatalf("each block must still be RECORDED: %+v", res.Cleanup)
	}
	for i, cr := range res.Cleanup {
		if cr.Outcome != report.CleanupNotRun {
			t.Errorf("block %d outcome = %q, want not-run", i, cr.Outcome)
		}
		if !strings.Contains(cr.Observed, "refused before firing") {
			t.Errorf("block %d must say the scenario was refused before firing: %q", i, cr.Observed)
		}
	}
}

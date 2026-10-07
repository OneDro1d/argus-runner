package toolcore

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// C10 (M25-FX4): a scenario discovered on disk but absent from the run report is a SILENT
// skip — surface it (only for an unfiltered run; a layer/tag/id filter legitimately runs a
// subset).
func TestReconcileRun_SurfacesSilentSkip(t *testing.T) {
	disc := []scenario.Discovered{
		{Scenario: &scenario.Scenario{ID: "A"}},
		{Scenario: &scenario.Scenario{ID: "B"}},
		{Scenario: &scenario.Scenario{ID: "C"}},
	}
	rep := &report.Report{Layers: []report.Layer{{Scenarios: []report.ScenarioResult{{ID: "A"}, {ID: "C"}}}}}

	cfg, missed := reconcileRun(disc, rep, false) // unfiltered
	if cfg != 3 {
		t.Errorf("configured = %d, want 3", cfg)
	}
	if len(missed) != 1 || missed[0] != "B" {
		t.Errorf("not_executed = %v, want [B]", missed)
	}
	// a filtered run runs a subset by design — never flag.
	if _, m := reconcileRun(disc, rep, true); m != nil {
		t.Errorf("filtered run must not flag a skip, got %v", m)
	}
}

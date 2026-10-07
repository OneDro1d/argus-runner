package argus

import (
	"os"
	"strings"
	"testing"
)

// RO-09 B1: an absent control-action saga still FAILS (a mandated saga is a CODE_BUG, req 3),
// but the live saga-presence.jmx FAIL message (the wording the user actually sees) must be
// ingestion-neutral — "not found in Loki" — never "absent", which implies the SUT failed to
// EMIT. Emission vs ingestion is indistinguishable to triage; the VALIDATE INPUT gate proves
// promtail ingests this SUT before the run. (The Go fallback at argus.go is updated to match.)
func TestSagaPresenceTemplate_AbsentWordingIngestionNeutral(t *testing.T) {
	b, err := os.ReadFile("../../templates/saga-presence.jmx")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "saga absent for correlation_id") {
		t.Error("B1: saga-presence FAIL message must not say 'absent' (implies the SUT didn't emit; emission vs ingestion indistinguishable)")
	}
	if !strings.Contains(s, "not found in Loki") {
		t.Error("B1: saga-presence FAIL message should say 'not found in Loki for <corr>'")
	}
}

// RO-09 A3 (CONFLICT-1): the argus-owned database-state template default must NOT be the
// bare `SELECT 1` tautology (returns one row regardless of SUT state). A missing query must
// fail loudly, not produce a false green. (This file is argus-owned, in-repo — not the
// argus submodule; no upstream PR.)
func TestDatabaseStateTemplate_NoBareSelect1Tautology(t *testing.T) {
	b, err := os.ReadFile("../../templates/database-state.jmx")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	if strings.Contains(string(b), "${__P(db.query,SELECT 1)}") {
		t.Error("A3/CONFLICT-1: db.query default must not be the bare `SELECT 1` tautology (false green); make it non-discriminating-safe or correlation-scoped")
	}
}

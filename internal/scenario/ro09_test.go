package scenario

import (
	"strings"
	"testing"
)

func dbScenario(verify string) string {
	return strings.Join([]string{
		"# Scenario: DBX-001",
		"## Metadata",
		"- **ID**: DBX-001",
		"- **Layer**: Database State",
		"- **Tags**: http",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`",
		"## VERIFY",
		verify,
		"## EXPECT",
		"### Runnable", // VR12-E1: position decides whether a bullet is a claim
		"- row_count == 1",
		"## TIMEOUT",
		"30s",
		"## CLEANUP",
		"N/A — a unit-test fixture; it creates nothing.",
	}, "\n")
}

// RO-09 (A1/A2): a Database State VERIFY with no executable query (prose only) silently
// degrades to the runner's `SELECT 1` default at run time — a tautology returning one row
// regardless of SUT state → FALSE GREEN (ORDE-007). validate_scenario must REJECT it so it
// never reaches a run (DESIGN.md principle #6 "executable artifacts only"; spec 02 strict
// validation = Fail).
func TestValidate_ContentVerifyMustBeExecutable(t *testing.T) {
	_, errs := Validate(dbScenario("Check that the order row was persisted with status pending."))
	if len(errs) == 0 {
		t.Fatal("a prose-only Database State VERIFY must FAIL validation (RO-09 false-green gate)")
	}
	var found bool
	for _, e := range errs {
		m := strings.ToLower(e.Message)
		if strings.Contains(m, "executable") || strings.Contains(e.Message, "SELECT 1") {
			found = true
		}
	}
	if !found {
		t.Errorf("error should explain the missing executable query / SELECT 1 tautology: %+v", errs)
	}
}

func TestValidate_ContentVerifyWithSQL_OK(t *testing.T) {
	withSQL := dbScenario("```sql\nSELECT status FROM orders WHERE correlation_id = '${correlation_id}'\n```")
	if _, errs := Validate(withSQL); len(errs) != 0 {
		t.Fatalf("a Database State scenario WITH a sql VERIFY must validate clean: %+v", errs)
	}
}

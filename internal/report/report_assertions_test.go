package report

import (
	"encoding/json"
	"strings"
	"testing"
)

// VR10-S2-12 (S2-c): a chain step records which content assertions were ENFORCED — the TEXT when it
// evaluated some, the COUNT always. The count is what survives the product-hat redaction.
//
// ⛔ AMENDED BY V31-005. This used to assert that a step with no checks gained NEITHER key, which
// made the count `omitempty` — and a count of 0 then emitted nothing, indistinguishable from an
// executor too old to write the field. "This step declared no checks" is a fact a consumer needs,
// so the zero is written and only the text is omitted.
func TestStepResult_AssertionsEnforcedIsOmittedWhenAbsentAndCarriedWhenSet(t *testing.T) {
	b, _ := json.Marshal(&StepResult{Name: "s", Status: "passed"})
	if strings.Contains(string(b), `"assertions_enforced":`) {
		t.Fatalf("a step without content assertions must not gain the TEXT key: %s", b)
	}
	if !strings.Contains(string(b), `"assertions_enforced_count":0`) {
		t.Fatalf("a step without content assertions must still report the count as 0 — absence is not health: %s", b)
	}
	b, _ = json.Marshal(&StepResult{Name: "s", Status: "passed",
		AssertionsEnforced: []string{"content contains x"}, AssertionsEnforcedCount: 1})
	if !strings.Contains(string(b), `"assertions_enforced":["content contains x"]`) || !strings.Contains(string(b), `"assertions_enforced_count":1`) {
		t.Fatalf("an enforced assertion must be recorded with its text and count: %s", b)
	}
}

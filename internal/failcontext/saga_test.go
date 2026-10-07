package failcontext

import (
	"encoding/json"
	"strings"
	"testing"
)

// C6 (M25-FX4): a saga step with no provenance must OMIT the nullable fields, not render
// them as misleading `null` (only control-action steps populate what/why/by-whom).
func TestSagaStep_OmitsNullProvenance(t *testing.T) {
	b, _ := json.Marshal(SagaStep{StepName: "pg_insert", Service: "order-processor", StepStatus: "ok"})
	s := string(b)
	for _, bad := range []string{`"hmac_verified":null`, `"chain_anchor":null`, `"what":null`, `"why":null`, `"by_whom":null`} {
		if strings.Contains(s, bad) {
			t.Errorf("null provenance field must be omitted, found %s in: %s", bad, s)
		}
	}
	// a populated control-action step still carries them
	what := "rate_limit changed"
	b2, _ := json.Marshal(SagaStep{StepName: "control_action", StepStatus: "ok", Fields: SagaFields{What: &what}})
	if !strings.Contains(string(b2), `"what":"rate_limit changed"`) {
		t.Errorf("a populated what must still serialize: %s", b2)
	}
}

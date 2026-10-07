package argus

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/obsquery"
)

// obsquery.ValidateCorrelationID accepts exactly the shape this package mints. If the
// generator changes (run id width, hash length, scenario id charset), this fails — instead of every
// get_sagas / get_tail_logs call refusing real ids in the field.
func TestMintedCorrelationIDs_PassTheLogQueryValidator(t *testing.T) {
	for _, sid := range []string{"ORD-006", "A", "ord_017", "Mixed-Case_and-dashes-9", "9X", "PERM-001-b"} {
		for i := 0; i < 50; i++ {
			c := newScenarioCorrelationID(NewRunID(), sid)
			if err := obsquery.ValidateCorrelationID(c); err != nil {
				t.Fatalf("a minted id was refused: %v", err)
			}
		}
	}
}

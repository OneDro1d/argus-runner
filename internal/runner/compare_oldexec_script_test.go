package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// ── unit level, runs everywhere ───────────────────────────────────────────────────────────────

// The premise of the check: a tree that does not know compare_scenarios decodes the assignment with no
// ordinary scenarios (an unknown JSON key is ignored), so there is nothing for it to run.
func TestCompareAssignment_OldShapeDecodesWithNoScenarios(t *testing.T) {
	type oldAssignment struct {
		RunID     string                       `json:"run_id"`
		Mode      string                       `json:"mode"`
		Scenarios []federation.ScenarioPayload `json:"scenarios,omitempty"`
	}
	wire := compareWireAssignment(t, "run-old") // a SEALED body, as the real check sends it
	var old oldAssignment
	if err := json.Unmarshal([]byte(wire), &old); err != nil {
		t.Fatal(err)
	}
	if old.Mode != "compare" || len(old.Scenarios) != 0 {
		t.Fatalf("old shape decoded %+v", old)
	}
	var now federation.RunAssignment
	if err := json.Unmarshal([]byte(wire), &now); err != nil {
		t.Fatal(err)
	}
	if len(now.Scenarios) != 0 || len(now.CompareScenarios) != 1 {
		t.Errorf("this tree must keep the set in compare_scenarios only: scenarios=%d compare_scenarios=%d", len(now.Scenarios), len(now.CompareScenarios))
	}
	if sp := now.CompareScenarios[0]; sp.Sealed == nil || sp.Body != "" {
		t.Errorf("the check's compare assignment must carry a sealed body and no clear one: %+v", sp)
	}
}

func nonCommentLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, t)
		}
	}
	return out
}

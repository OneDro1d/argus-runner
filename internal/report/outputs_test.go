package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// ARGUS-CMP-3: ScenarioResult.Outputs. nil everywhere except a compare run's `## COMPARE` check, so
// report.json and the evidence hash (sha256 of this struct's JSON) are unchanged for every other row.

func TestScenarioResult_BytesUnchangedWithoutOutputs(t *testing.T) {
	row := goldenRow()
	row.WindowStart = time.Unix(1775014130, 0)
	row.WindowEnd = time.Unix(1775014140, 0)
	row.Steps = nil
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != goldenRowWithoutSandboxPolicy {
		t.Fatalf("a row with no outputs changed its bytes:\n got  %s\n want %s", b, goldenRowWithoutSandboxPolicy)
	}
	if strings.Contains(string(b), `"outputs"`) {
		t.Errorf("outputs key present on a row with none: %s", b)
	}
}

func TestScenarioResult_OutputsAreDigestsOnly(t *testing.T) {
	row := goldenRow()
	h := strings.Repeat("c", 64)
	row.Outputs = []compare.OutputRecord{{V: 1, Step: "create", Sample: 1, State: compare.StateRecorded, Status: 200,
		Parts: compare.Parts{Status: h, Body: h}, Hash: h, BodyKind: compare.KindJSON, BodyBytes: 7}}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"outputs":[{"v":1,"step":"create","sample":1,"state":"recorded"`) {
		t.Errorf("record not serialised under outputs: %s", b)
	}
	var back ScenarioResult
	if err := json.Unmarshal(b, &back); err != nil || len(back.Outputs) != 1 || back.Outputs[0].Hash != h {
		t.Errorf("round trip: %+v %v", back.Outputs, err)
	}
	// the stored-form pointer on a step is runtime-only: it can never reach the bytes
	step := StepResult{Name: "create", Status: "passed", Output: &RecordedOutput{Record: row.Outputs[0]}}
	sb, _ := json.Marshal(step)
	if strings.Contains(string(sb), "Output") || strings.Contains(string(sb), "output") || strings.Contains(string(sb), h) {
		t.Errorf("StepResult.Output was serialised: %s", sb)
	}
}

func TestWithholdScenarios_DropsOutputsWithEveryRow(t *testing.T) {
	r := &Report{Mode: "compare", Layers: []Layer{{Layer: "L", Scenarios: []ScenarioResult{
		{ID: "S", Status: "passed", Outputs: []compare.OutputRecord{{V: 1, State: compare.StateRecorded, Hash: strings.Repeat("d", 64)}}}}}}}
	r.WithholdScenarios()
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), `"outputs"`) || strings.Contains(string(b), strings.Repeat("d", 64)) {
		t.Errorf("a withheld report still carries outputs: %s", b)
	}
	if !ModeIsCertifying(ModeCompare) || (&Report{Mode: ModeCompare}).ScenariosVisibleToBuilder() {
		t.Errorf("mode compare must be certifying and invisible to a builder")
	}
}

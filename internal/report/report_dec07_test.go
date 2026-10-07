package report

import (
	"encoding/json"
	"strings"
	"testing"
)

// DEC-07: the extension is forward-compatible — an existing single-step report
// (no steps / mcp_envelope) round-trips unchanged; omitempty keeps the new keys absent.
func TestDEC07_ForwardCompatible(t *testing.T) {
	old := `{"project":"p","summary":{"total":1,"passed":1,"failed":0,"skipped":0},` +
		`"layers":[{"layer":"http-ingestion","scenarios":[{"id":"ORD-1","status":"passed","duration_ms":5}]}]}`
	var r Report
	if err := json.Unmarshal([]byte(old), &r); err != nil {
		t.Fatalf("old report no longer parses: %v", err)
	}
	if r.Layers[0].Scenarios[0].Steps != nil || r.Layers[0].Scenarios[0].MCPEnvelope != nil {
		t.Error("old report must yield nil Steps/MCPEnvelope")
	}
	b, _ := json.Marshal(&r)
	if strings.Contains(string(b), "steps") || strings.Contains(string(b), "mcp_envelope") {
		t.Fatalf("omitempty broken — an old report gained new keys: %s", b)
	}
}

// DEC-07 / VR-K4/K5: a chained scenario carries per-step status + the shared correlation id.
func TestDEC07_ChainedScenarioCarriesSteps(t *testing.T) {
	r := ScenarioResult{ID: "MCP-1", Status: "failed", CorrelationID: "tr-7", Steps: []StepResult{
		{Name: "mcp", Status: "passed", Observed: "isError:false", CorrelationID: "tr-7"},
		{Name: "db", Status: "failed", Observed: "0 rows", CorrelationID: "tr-7"},
	}}
	b, _ := json.Marshal(&r)
	if !strings.Contains(string(b), `"steps"`) || strings.Count(string(b), `"tr-7"`) < 3 {
		t.Fatalf("chained scenario must carry per-step status + correlation across all steps: %s", b)
	}
}

// DEC-07 / VR-L3: the execution-failure status is distinct from passed/failed.
func TestDEC07_ExecutionErrorStatusDistinct(t *testing.T) {
	if StatusError == "failed" || StatusError == "passed" {
		t.Fatal("execution-error status must be distinct from SUT pass/fail")
	}
	b, _ := json.Marshal(&ScenarioResult{ID: "UI-1", Status: StatusError})
	if !strings.Contains(string(b), `"status":"error"`) {
		t.Fatalf("execution-error status not serialized: %s", b)
	}
}

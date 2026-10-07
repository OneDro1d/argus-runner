package argus

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// RecordsComparedOutput is the facts-builder in front of compare.RecordsOutput: the control plane asks it which
// checks of a set can ever be compared. It must follow the executor's own dispatch (runOneScenario).
func TestRecordsComparedOutput_FollowsTheExecutorsDispatch(t *testing.T) {
	http := func(layer string, tags ...string) *scenario.Scenario {
		return &scenario.Scenario{Layers: []string{layer}, Tags: tags}
	}
	withSecond := http("HTTP Ingestion")
	withSecond.ExpectRunnable = []string{"status=200", "status2=409"}
	withSecond.Expect = withSecond.ExpectRunnable
	withLoad := http("HTTP Ingestion")
	withLoad.Load = &scenario.LoadProfile{}
	chainHTTP := http("HTTP Ingestion", ChainTag)
	chainHTTP.Trigger.Payload = `{"steps":[{"type":"mcp","name":"a"},{"type":"http","name":"b"}]}`
	chainNoHTTP := http("HTTP Ingestion", ChainTag)
	chainNoHTTP.Trigger.Payload = `{"steps":[{"type":"mcp","name":"a"}]}`

	cases := []struct {
		name string
		s    *scenario.Scenario
		want bool
	}{
		{"HTTP Ingestion", http("HTTP Ingestion"), true},
		{"HTTP Ingestion with a second status (http-idempotency)", withSecond, true},
		{"Error Path (shares the http-ingestion template)", http("Error Path"), true},
		{"Rate Limiting", http("Rate Limiting"), true},
		{"Permissions", http("Permissions"), true},
		{"Database State", http("Database State"), false},
		{"Message Flow", http("Message Flow"), false},
		{"External Delivery", http("External Delivery"), false},
		{"saga-presence on HTTP Ingestion", http("HTTP Ingestion", SagaPresenceTag), false},
		{"an http check with ## LOAD", withLoad, false},
		{"an mcp check", http("HTTP Ingestion", MCPTag), false},
		{"a ui check", http("Web UI", UITag), false},
		{"AMQP Load", http(scenario.AMQPLoadLayer), false},
		{"a chain with an http step", chainHTTP, true},
		{"a chain with no http step", chainNoHTTP, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := RecordsComparedOutput(tc.s); got != tc.want {
			t.Errorf("%s: RecordsComparedOutput = %v, want %v", tc.name, got, tc.want)
		}
	}
	// the executor's capture list and the control plane's are ONE map
	for base := range jmeterCaptureTemplates {
		if !compare.CaptureTemplateBases[base] {
			t.Errorf("template %s is captured by the executor and unknown to compare.CaptureTemplateBases", base)
		}
	}
}

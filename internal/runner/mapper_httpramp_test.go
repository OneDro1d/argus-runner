package runner

// mapper_httpramp_test.go -- promise 2: an HTTP Load row reaches ResultsPush.load_ramp as ONE
// entry with driver "http", its per-step record (response quantiles and error reasons included) and the step it
// stopped at, beside an AMQP entry whose bytes are unchanged.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

func httpRampSteps() []report.LoadStep {
	return []report.LoadStep{
		{Step: 1, Sessions: 10, Status: report.LoadStepMeasured, WindowSeconds: 50, OfferedPerS: 400, SentPerS: 400, DeliveredPerS: 400, DeliveredRatio: 1,
			ResponseUs: &report.Quantiles{Min: 2000, P50: 9000, P75: 12000, P95: 20000, P99: 31000, Max: 80000}, Comfortable: true},
		{Step: 2, Sessions: 20, Status: report.LoadStepMeasured, WindowSeconds: 50, OfferedPerS: 600, SentPerS: 590, DeliveredPerS: 300, DeliveredRatio: 0.5,
			ResponseUs:   &report.Quantiles{Min: 2000, P50: 40000, P75: 90000, P95: 400000, P99: 900000, Max: 1200000},
			Errors:       map[string]int{"status:503": 14500, "transport": 500},
			ErrorReasons: []report.LoadError{{Reason: "org.apache.http.conn.HttpHostConnectException: Connection refused", Count: 500}}},
		{Step: 3, Sessions: 40, Status: report.LoadStepNotRun},
	}
}

func TestMapReport_AnHTTPRampRidesTheWireWithDriverHTTP(t *testing.T) {
	rep := loadReport(ramp2Steps())
	rep.Layers = append(rep.Layers, report.Layer{Layer: "http-load", Scenarios: []report.ScenarioResult{
		{ID: "HL-001", Status: "passed", LoadDriver: "http", LoadTarget: "api-lab", LoadSteps: httpRampSteps(), LoadStoppedAtStep: 2},
	}})
	push := mapLoad(rep)
	var got []report.LoadRampEntry
	if err := json.Unmarshal(push.LoadRamp, &got); err != nil {
		t.Fatalf("load_ramp: %v: %s", err, push.LoadRamp)
	}
	if len(got) != 2 || got[0].ScenarioID != "AMQL-001" || got[1].ScenarioID != "HL-001" {
		t.Fatalf("entries = %+v, want the AMQP and the HTTP ramp in scenario-id order", got)
	}
	h := got[1]
	if h.Driver != "http" || h.Target != "api-lab" || h.StoppedAtStep != 2 || len(h.Steps) != 3 {
		t.Fatalf("http entry = %+v", h)
	}
	if s := h.Steps[1]; s.ResponseUs == nil || s.ResponseUs.P95 != 400000 || s.Errors["status:503"] != 14500 || len(s.ErrorReasons) != 1 {
		t.Errorf("the breaking step lost its facts on the wire: %+v", s)
	}
	if got[0].Driver != "amqp" || got[0].StoppedAtStep != 0 {
		t.Errorf("the AMQP entry changed: %+v", got[0])
	}
	if strings.Contains(string(push.LoadRamp), `"stopped_at_step":0`) || strings.Count(string(push.LoadRamp), "stopped_at_step") != 1 {
		t.Errorf("stopped_at_step must appear on the HTTP entry only: %s", push.LoadRamp)
	}
	if strings.Contains(string(push.LoadRamp), "response_us\":null") {
		t.Errorf("an absent response_us must be absent, not null: %s", push.LoadRamp)
	}
}

package httpload

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpload"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// AC-F2.2: the issue's two rows, through the real step aggregation. The step keeps the load tool's message (the full
// report), carries the word protocol on each entry, and what leaves (report.LoadStepWithoutTexts) holds protocol + the
// count and no part of either message.
func TestAggregateStep_TheIssueRowsCarryTheWordProtocol(t *testing.T) {
	const marker = "AC-F22-CHUNK-9d4c"
	t0 := int64(1_000_000)
	rs := []amqpload.Row{row(t0, 5, "200", "OK")}
	for i := 0; i < 3; i++ {
		rs = append(rs, row(t0+1000+int64(i)*100, 2, "Non HTTP response code: org.apache.http.ProtocolException",
			"Non HTTP response message: Invalid header: session=hunter2"))
	}
	rs = append(rs, row(t0+2000, 2, "Non HTTP response code: org.apache.http.MalformedChunkCodingException",
		"Non HTTP response message: Bad chunk header: "+marker))
	rs = append(rs, row(t0+3000, 5, "200", "OK"))
	st := AggregateStep(rs, StepInput{Step: 1, Users: 1, ScenarioID: "HL-1", TargetP95Ms: 1000, MaxErrorRate: 1})

	want := []report.LoadError{
		{Reason: "org.apache.http.ProtocolException: Invalid header: session=hunter2", Kind: "protocol", Count: 3},
		{Reason: "org.apache.http.MalformedChunkCodingException: Bad chunk header: " + marker, Kind: "protocol", Count: 1},
	}
	if !reflect.DeepEqual(st.ErrorReasons, want) {
		t.Fatalf("error_reasons = %+v\nwant %+v", st.ErrorReasons, want)
	}
	out, _ := json.Marshal(report.LoadStepWithoutTexts(st))
	if !strings.Contains(string(out), `"error_reasons":[{"kind":"protocol","count":4}]`) {
		t.Errorf("leaving step = %s, want protocol x 4", out)
	}
	for _, bad := range []string{"hunter2", "session=", "Invalid header", "Bad chunk", marker, `"reason":`} {
		if strings.Contains(string(out), bad) {
			t.Errorf("%q left the environment: %s", bad, out)
		}
	}
}

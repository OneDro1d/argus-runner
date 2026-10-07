package compare_test

// cmp14_executor_contract_test.go -- ARGUS-CMP-14. What the control plane relies on and does not own: a check whose own
// EXPECT failed STILL records its output in run mode `compare`, so a measured cell of a run that ran with a failing
// check reads the recorded output exactly as for a passing run. The executor sites (read, not changed here):
//
//	internal/chain/http.go:298   a step whose judge failed: st.Output = recordOutput(...)  (:278 too, a failed save)
//	internal/argus/argus.go:1026 JMeter: capture.collect runs right after r.Run, BEFORE the .jtl is judged (:1040
//	                             res := ScenarioResult{..., Status: "failed", Outputs: recorded})
//	internal/argus/chain_scenario.go:357 attachChainOutputs copies every step's Output, failed ones included

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/compare"
)

func TestCMP14_AChainStepWhoseExpectFailedStillRecordsItsOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	oc := &chain.OutputCapture{Spec: compare.Spec{Status: true, Body: true}}
	// the step EXPECTs 200 and gets 500: its own check fails
	res := chain.HTTPStepWithOutput("s", "GET", srv.URL, nil, "", 200, nil, nil, nil, false, nil, nil, oc).Run("c", map[string]string{})
	if res.Status != "failed" {
		t.Fatalf("setup: the step's status = %q, want failed", res.Status)
	}
	if res.Output == nil || res.Output.Record.State != compare.StateRecorded || res.Output.Record.Status != 500 {
		t.Fatalf("a failed step recorded %+v, want a recorded output of the 500 it judged", res.Output)
	}
}

package chain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// the backstop count is set by EVERY step type that lists failed
// claims, not only http. Each step type wires failedBodyClaims' second result into the step itself, so
// each needs its own test: dropping the assignment at one call site leaves the others green.

// falseClaims returns n `field v > N` claims that all fail against {"v": 1}.
func falseClaims(n int) []mcp.BodyAssert {
	out := make([]mcp.BodyAssert, n)
	for i := range out {
		out[i] = mcp.BodyAssert{Field: "v", Op: mcp.BodyGTOp, Value: fmt.Sprintf("%d", 1000+i)}
	}
	return out
}

func assertBackstop(t *testing.T, st report.StepResult, listed, omitted int) {
	t.Helper()
	if st.Status != "failed" {
		t.Fatalf("want failed, got %+v", st.Status)
	}
	if got := len(failedClaimsOf(t, st)); got != listed {
		t.Errorf("want %d listed, got %d", listed, got)
	}
	if st.FailedClaimsOmitted != omitted {
		t.Errorf("want failed_claims_omitted %d, got %d", omitted, st.FailedClaimsOmitted)
	}
	b, _ := json.Marshal(st)
	if has := strings.Contains(string(b), `"failed_claims_omitted"`); has != (omitted > 0) {
		t.Errorf("the key must appear exactly when something was left out (omitted=%d): %s", omitted, b)
	}
}

func TestFailedClaims_Backstop_HTTP_ExactlyAtTheBackstopIsComplete(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"v": 1}`}})
	st := httpStepWith(srv.URL, 200, nil, falseClaims(report.MaxFailedClaims)...).Run("cid", map[string]string{})
	assertBackstop(t, st, report.MaxFailedClaims, 0)
}

func TestFailedClaims_Backstop_HTTP_OnePastTheBackstopIsCounted(t *testing.T) {
	srv := newSeqServer(t, map[string][]string{"/b": {`{"v": 1}`}})
	st := httpStepWith(srv.URL, 200, nil, falseClaims(report.MaxFailedClaims+1)...).Run("cid", map[string]string{})
	assertBackstop(t, st, report.MaxFailedClaims, 1)
}

func TestFailedClaims_Backstop_MCP_PastTheBackstopIsCounted(t *testing.T) {
	ts := mcpServerReturning(`{"v": 1}`)
	defer ts.Close()
	cl := &mcp.Client{ServerURL: ts.URL + "/mcp", Transport: mcp.Streamable, Token: "x"}
	expect := mcp.Expect{ErrorPlane: mcp.PlaneNone, Body: falseClaims(report.MaxFailedClaims + 1)}
	st := MCPStep("m", cl, "t", `{}`, expect, nil).Run("cid", map[string]string{})
	assertBackstop(t, st, report.MaxFailedClaims, 1)
}

func TestFailedClaims_Backstop_AMQP_PastTheBackstopIsCounted(t *testing.T) {
	spec := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "q", Wait: time.Second}
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`{"v": 1}`)}
	dial, _ := dialerFor(f)
	want := scenario.AMQPStepWant{Broker: accepts, Body: falseClaims(report.MaxFailedClaims + 1)}
	st := AMQPStep("take", spec, want, dial).Run("cid", map[string]string{})
	assertBackstop(t, st, report.MaxFailedClaims, 1)
}

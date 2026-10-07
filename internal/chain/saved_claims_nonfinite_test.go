package chain

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// TestSavedThreshold_NonFiniteSavedValue_FailsByName: strconv.ParseFloat accepts "NaN", "Inf" and
// "-Infinity", so "parses as a float" is not "is a number" here. A SUT that answers `Inf` must not turn
// `count < ${saved.n}` into a pass (every finite count is below it), nor `NaN` into a comparison that
// can only ever say false. Each FAILS the step by name, before anything is sent, and never prints it.
func TestSavedThreshold_NonFiniteSavedValue_FailsByName(t *testing.T) {
	for _, saved := range []string{"Inf", "+Inf", "-Infinity", "NaN"} {
		t.Run(saved, func(t *testing.T) {
			srv := newSeqServer(t, map[string][]string{
				"/a": {`{"state": "` + saved + `"}`}, "/b": {`{"count": 5}`}})
			want := []mcp.BodyAssert{{Field: "count", Op: mcp.BodyLTOp, Value: "${saved.n}"}}
			lt := HTTPStep("read2", "GET", srv.URL+"/b", nil, "", 200, want, nil, nil, false, nil, &scenario.MoneySpendLedger{})
			res := Run("cid", []Step{readStep(srv.URL, saveN("state")), lt})
			st := res.Steps[1]
			if st.Status != "failed" {
				t.Fatalf("a saved %q must FAIL a numeric claim against it, got %+v", saved, st)
			}
			if !strings.Contains(st.Observed, "the saved value of `n` is not a number") {
				t.Errorf("the failure must say so by name, got %q", st.Observed)
			}
			if srv.count("/b") != 0 {
				t.Errorf("nothing may be sent once the claim cannot be bound; /b was hit %d time(s)", srv.count("/b"))
			}
		})
	}
}

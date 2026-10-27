package argus

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// THE WIRING of a db step's `save` and `always` at the executor's call site
// (chain_scenario.go: chain.DBStep(…, st.Save, …, st.Always, …)). chain.DBStep's own tests prove both work when they
// are passed; these prove the executor passes them. Found by mutation: with `nil` and `false` at the call site every
// other test in this package stayed green.

func TestDBChain_SaveOnADBStepReachesALaterStep(t *testing.T) {
	f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"status"}, Rows: [][]any{{"PAID"}}}}
	withFakeDB(t, f)
	steps := `{"type":"db","name":"q1","query":"SELECT status FROM orders WHERE id = $1","params":["${saved.oid}"],"save":{"st":"rows.0.status"}},` +
		`{"type":"db","name":"q2","query":"SELECT status FROM orders WHERE status = $1","params":["${saved.st}"]}`
	s, _, stop := dbChain(t, steps, "- step q1: body has row_count == 1\n- step q2: body has row_count == 1\n")
	defer stop()
	res := runChainScenario(dbConfig(), s, "tr-db-save", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("want passed: %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if len(f.params) != 2 || len(f.params[1]) != 1 || f.params[1][0] != "PAID" {
		t.Fatalf("the value the first db step saved must reach the second as its parameter: %q", f.params)
	}
}

// A step that failed saves nothing, so a later db step whose param needs that value is `not-measured` and never
// attempted - unless it is `always`, which skips that gate (ChainStep.Always) and so is attempted and fails on the
// unbound value by name. The two outcomes differ only if the executor passes `always` through.
func TestDBChain_AlwaysOnADBStepSkipsTheNeedsGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		always       string
		wantMeasured bool
	}{
		{"always", `,"always":true`, true},
		{"not always", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &argusFakeDB{rows: chain.DBRows{Columns: []string{"n"}, Rows: [][]any{{"x"}}}}
			withFakeDB(t, f)
			step := `{"type":"db","name":"q","query":"SELECT n FROM t WHERE id = $1","params":["${saved.oid}"]` + tc.always + `}`
			// the http step before it FAILS its claim (the server answers {"id":"o-1"}), so `oid` is never saved
			s, _, stop := dbChain(t, step, "- step mk: body has id equals something-else\n- step q: body has row_count == 1\n")
			defer stop()
			res := runChainScenario(dbConfig(), s, "tr-db-always", "testkit/ui")
			if res.Status != "failed" || len(res.Steps) != 2 {
				t.Fatalf("the chain fails on its first step: %q %+v %+v", res.Status, res.Failure, res.Steps)
			}
			if len(f.calls) != 0 {
				t.Fatalf("no query may be sent with an unbound parameter: %d", len(f.calls))
			}
			if measured := res.Steps[1].Status != report.StepNotMeasured; measured != tc.wantMeasured {
				t.Fatalf("step q status %q: attempted=%v want %v", res.Steps[1].Status, measured, tc.wantMeasured)
			}
		})
	}
}

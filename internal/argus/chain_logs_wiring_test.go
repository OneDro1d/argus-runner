package argus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// THE WIRING of a logs step's `save` and `always` at the executor's call site
// (chain_scenario.go: chain.LogsStep(…, st.Save, …, st.Always, …)). chain.LogsStep's own tests prove both work when they
// are passed; these prove the executor passes them. The logs twin of chain_db_wiring_test.go. Found by mutation: with
// `nil` and `false` at the call site every other test in this package stayed green.

func TestLogsChain_SaveOnALogsStepReachesALaterStep(t *testing.T) {
	loki := newTestLoki(t, 2) // the lines it answers are "line-0", "line-1"
	step := `{"type":"logs","name":"l1","scope":"correlation","contains":"first","settle":"0s","save":{"tok":"lines.0"}},` +
		`{"type":"logs","name":"l2","scope":"correlation","contains":"again ${saved.tok}","settle":"0s"}`
	s, _, stop := logsChain(t, step, "- step l1: body has count >= 1\n- step l2: body has count >= 1\n")
	defer stop()
	res := runLogsChain(&config.Config{}, s, loki.reader())
	if res.Status != "passed" {
		t.Fatalf("want passed: %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if loki.requests() != 2 {
		t.Fatalf("one Loki query per logs step: %d %q", loki.requests(), loki.queries)
	}
	if !strings.HasSuffix(loki.queries[1], `|= "again line-0"`) {
		t.Fatalf("the value the first logs step saved must reach the second step's `contains`: %q", loki.queries)
	}
}

// A step that failed saves nothing, so a later logs step whose `contains` needs that value is `not-measured` and never
// attempted - unless it is `always`, which skips that gate (ChainStep.Always) and so is attempted (and refused at once
// on the unbound value, with no query sent). The two outcomes differ only if the executor passes `always` through.
func TestLogsChain_AlwaysOnALogsStepSkipsTheNeedsGate(t *testing.T) {
	for _, tc := range []struct {
		name         string
		always       string
		wantMeasured bool
	}{
		{"always", `,"always":true`, true},
		{"not always", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loki := newTestLoki(t, 1)
			var hits int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				_, _ = w.Write([]byte(`{"id":"o-1"}`))
			}))
			defer api.Close()
			// the http step FAILS its claim (the server answers {"id":"o-1"}), so `oid` is never saved
			trig := `{"steps":[{"type":"http","name":"mk","method":"GET","url":"` + api.URL + `/o","save":{"oid":"id"}},` +
				`{"type":"logs","name":"l","scope":"correlation","contains":"order ${saved.oid}","settle":"0s"` + tc.always + `}]}`
			expect := "### Runnable\n- step mk: body has id equals something-else\n- step l: body has count >= 1\n"
			s := scenario.Parse(chainRunMD("", trig, expect))
			res := runLogsChain(&config.Config{}, s, loki.reader())
			if res.Status != "failed" || len(res.Steps) != 2 {
				t.Fatalf("the chain fails on its first step: %q %+v %+v", res.Status, res.Failure, res.Steps)
			}
			if loki.requests() != 0 {
				t.Fatalf("no query may be sent with an empty filter: %d %q", loki.requests(), loki.queries)
			}
			if measured := res.Steps[1].Status != report.StepNotMeasured; measured != tc.wantMeasured {
				t.Fatalf("step l status %q: attempted=%v want %v", res.Steps[1].Status, measured, tc.wantMeasured)
			}
		})
	}
}

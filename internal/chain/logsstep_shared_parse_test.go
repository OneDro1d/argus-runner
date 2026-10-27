package chain

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// vector answers the parse-health probe's `sum(count_over_time(...))` with n (an empty vector for 0).
func vector(n int) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		res := ""
		if n > 0 {
			res = fmt.Sprintf(`{"metric":{},"value":[1760868000.0,"%d"]}`, n)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[`+res+`]}}`)
	}
}

// THE LIVE FINDING (2026-10-19): the SUT logs plain text, `| json` parses none of its lines, and
// `labels: {level: error}` + `count == 0` must not pass on that.
func TestLogsStep_SharedLokiUnparsedLinesAreNotZeroLines(t *testing.T) {
	zero := logsClaims(t, "body has count == 0")
	spec := LogsSpec{Scope: scenario.LogsScopeSource, Labels: map[string]string{"service": "main-engine", "level": "error"}}

	t.Run("lines exist, none parse: error naming the format and the count, claim not judged", func(t *testing.T) {
		// the query (empty), the bare-selector probe (a line), the total count, the parsed count
		f := newFakeLoki(t, nil, nLines(0), nLines(1), vector(9521), vector(0))
		res := LogsStep("l", logsSpec(newClock(), spec), zero, nil, nil, false, sharedLoki(f)).Run(testCID, map[string]string{})
		if res.Status != report.StatusError {
			t.Fatalf("want error, got %s %q", res.Status, res.Observed)
		}
		for _, need := range []string{"9521", "none of them parses as json"} {
			if !strings.Contains(res.Observed, need) {
				t.Errorf("the sentence must contain %q: %s", need, res.Observed)
			}
		}
		if len(res.FailedClaims) != 0 {
			t.Fatalf("a claim was judged on lines nothing could read: %+v", res)
		}
		reqs := f.requests()
		if len(reqs) != 4 {
			t.Fatalf("want query, probe, total, parsed; got %d requests", len(reqs))
		}
		for _, r := range reqs[2:] {
			if !strings.Contains(r.query, "count_over_time") {
				t.Errorf("request %q is not a count", r.query)
			}
		}
		t.Logf("step result: %s", res.Observed)
	})

	t.Run("some lines parse: the zero is real and passes", func(t *testing.T) {
		f := newFakeLoki(t, nil, nLines(0), nLines(1), vector(9521), vector(40))
		res := LogsStep("l", logsSpec(newClock(), spec), zero, nil, nil, false, sharedLoki(f)).Run(testCID, map[string]string{})
		if res.Status != "passed" || res.Observed != "log query matched 0 line(s)" {
			t.Fatalf("want passed with 0 lines, got %s %q", res.Status, res.Observed)
		}
	})

	t.Run("a poll keeps asking while nothing parses, then errors; it never passes", func(t *testing.T) {
		f := newFakeLoki(t, nil, nLines(0), nLines(1), vector(9521), vector(0))
		poll := &Poll{Timeout: 5 * 1e9, Interval: 1e9}
		res := LogsStep("l", logsSpec(newClock(), spec), zero, nil, poll, false, sharedLoki(f)).Run(testCID, map[string]string{})
		if res.Status != report.StatusError || len(f.requests()) < 8 {
			t.Fatalf("want error after several polls, got %s %q after %d requests", res.Status, res.Observed, len(f.requests()))
		}
	})

	t.Run("an own Loki is unchanged: the empty answer is a real zero, no count asked", func(t *testing.T) {
		f := newFakeLoki(t, nil, nLines(0))
		res := LogsStep("l", logsSpec(newClock(), spec), zero, nil, nil, false, f.loki()).Run(testCID, map[string]string{})
		if res.Status != "passed" || len(f.requests()) != 1 {
			t.Fatalf("own-Loki behaviour changed: %s %q after %d requests", res.Status, res.Observed, len(f.requests()))
		}
	})
}

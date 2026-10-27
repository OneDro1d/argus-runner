package argus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// (a tester's acceptance run) — THE SAGA-PRESENCE QUERY WINDOW'S END WAS FIXED BEFORE
// THE CHECK WAITED.
//
// templates/saga-presence.jmx computed `endNs = now` once, THEN slept settle_ms, THEN polled with
// sleep(interval) between attempts, and every query sent that same `end`. A saga line that Loki stamped
// even 0.7 s after the script began (a SUT that logs after it answers, then promtail's hop) lay outside
// every attempt's window: "saga not found", a false FAIL, however many attempts and however long the wait.
// The chain `logs` step already took `at := now()` per read; the fix mirrors that intent — the START stays
// anchored at trigger time - 15 min, the END is now() on EACH attempt, immediately before its query.
//
// ⛔ THE FAKE LOKI HONOURS `start` AND `end`. The existing fake (runVerify) serves its lines whatever the
// window says, so it could never have shown this defect — a fake that ignores the parameter the defect
// lives in is a test of nothing. These tests run the REAL script (execVerifyScript: the verify-saga
// JSR223 body extracted from the template, run by Groovy) against a Loki that returns the saga line ONLY
// when start <= its timestamp <= end.

type lokiQuery struct {
	at         time.Time
	start, end int64
}

// windowedLoki serves one saga line whose timestamp is chosen from the moment the FIRST query arrives
// (tsFromFirst), because the script's own start time is not known to the test: the old code fixed its
// end at script start, and the first query arrives settle_ms later, so a timestamp a fraction of
// settle_ms BEFORE the first query is, by construction, AFTER the moment the old code fixed its end.
type windowedLoki struct {
	t         *testing.T
	mu        sync.Mutex
	queries   []lokiQuery
	firstAt   time.Time
	tsFromFst time.Duration // line timestamp = first query's arrival + this (may be negative)
	line      string
}

func (w *windowedLoki) handler() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
		end, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
		w.mu.Lock()
		if len(w.queries) == 0 {
			w.firstAt = time.Now()
		}
		w.queries = append(w.queries, lokiQuery{at: time.Now(), start: start, end: end})
		ts := w.firstAt.Add(w.tsFromFst).UnixNano()
		w.mu.Unlock()

		values := [][]string{}
		if ts >= start && ts <= end { // THE PARAMETER UNDER TEST: the window decides whether the line is returned
			values = append(values, []string{strconv.FormatInt(ts, 10), w.line})
		}
		body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "streams",
			"result":     []any{map[string]any{"stream": map[string]string{"service": "order-api"}, "values": values}},
		}})
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write(body)
	}
}

func TestSagaPresenceWindow_EndIsRecomputedOnEachAttempt(t *testing.T) {
	def := sagaPropsFor(t, sagaScenario(t))
	real := line(map[string]any{"event_type": "saga", "correlation_id": sagaCID, "step_name": "control_action", "step_status": "ok"})

	cases := []struct {
		name     string
		props    map[string]string
		tsFromFt time.Duration
		// minQueries: the line is first inside the window on a LATER attempt, so the check must have polled again
		minQueries int
	}{
		{
			// the live report: the line landed in Loki AFTER the script began (0.67 s after the trigger's
			// response) but before the first query. The old end (script start) excluded it on every attempt.
			name:     "a line stamped after the script began, before the first query, is found",
			props:    map[string]string{"loki.poll_attempts": "3", "loki.settle_ms": "800", "loki.poll_interval_ms": "100"},
			tsFromFt: -400 * time.Millisecond, minQueries: 1,
		},
		{
			// the line is stamped after the FIRST query's window closed: only an end recomputed for a LATER
			// attempt can contain it. A fix that moved the one computation to after the settle would still fail this.
			name:     "a line stamped after the first attempt's query is found by a later attempt's window",
			props:    map[string]string{"loki.poll_attempts": "4", "loki.settle_ms": "0", "loki.poll_interval_ms": "400"},
			tsFromFt: 250 * time.Millisecond, minQueries: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := &windowedLoki{t: t, tsFromFst: tc.tsFromFt, line: real}
			srv := httptest.NewServer(fl.handler())
			defer srv.Close()

			props := map[string]string{}
			for k, v := range def {
				props[k] = v
			}
			for k, v := range tc.props {
				props[k] = v
			}
			v := execVerifyScript(t, props, srv.URL)

			fl.mu.Lock()
			qs := append([]lokiQuery(nil), fl.queries...)
			fl.mu.Unlock()
			if !v.Successful {
				t.Fatalf("a saga line that Loki holds, stamped after the script began, was reported absent (message %q) after %d quer(ies): the window's END was fixed before the wait —", v.Message, len(qs))
			}
			if len(qs) < tc.minQueries {
				t.Errorf("queries = %d, want at least %d", len(qs), tc.minQueries)
			}

			// the window's START stays anchored: the same on every attempt, 15 minutes before the first end
			// (trigger time), never sliding forward with the retries.
			for i, q := range qs {
				if q.start != qs[0].start {
					t.Errorf("attempt %d start %d != attempt 1 start %d: the START must stay anchored at the trigger time", i+1, q.start, qs[0].start)
				}
				if i > 0 && q.end <= qs[i-1].end {
					t.Errorf("attempt %d end %d is not later than attempt %d end %d: the END must be 'now' on each attempt", i+1, q.end, i, qs[i-1].end)
				}
				// each end is "now, immediately before its query" — within a second of the moment the query arrived
				if d := time.Duration(q.at.UnixNano() - q.end); d < -time.Second || d > time.Second {
					t.Errorf("attempt %d: end is %v away from the moment its query arrived; it must be computed right before the query", i+1, d)
				}
			}
			if win := time.Duration(qs[len(qs)-1].end - qs[0].start); win < 15*time.Minute {
				t.Errorf("window at the last attempt = %v, want at least the 15 minute look-back", win)
			}
		})
	}
}

// GUARD — the fail-closed wording stays: a line that is NEVER in the window (older than the look-back) is
// still "not found", with the unchanged message. Fixing the end must not turn "absent" into "present".
func TestSagaPresenceWindow_ALineOutsideTheLookBackIsStillNotFound(t *testing.T) {
	def := sagaPropsFor(t, sagaScenario(t))
	real := line(map[string]any{"event_type": "saga", "correlation_id": sagaCID, "step_name": "control_action"})
	fl := &windowedLoki{t: t, tsFromFst: -20 * time.Minute, line: real}
	srv := httptest.NewServer(fl.handler())
	defer srv.Close()
	props := map[string]string{}
	for k, v := range def {
		props[k] = v
	}
	props["loki.poll_attempts"], props["loki.settle_ms"], props["loki.poll_interval_ms"] = "2", "0", "50"
	v := execVerifyScript(t, props, srv.URL)
	if v.Successful {
		t.Fatalf("a line 20 minutes old is outside the 15 minute look-back and must not be found; got %q", v.Message)
	}
	if want := "control_action saga not found in Loki for correlation_id " + sagaCID + " (saga-presence FAIL)"; v.Message != want {
		t.Errorf("failure wording changed:\n got %q\nwant %q", v.Message, want)
	}
}

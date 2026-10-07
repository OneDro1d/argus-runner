package obsquery

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// PushRequestEvents ships one Loki line per fired request (r3, 1a′): grouped into ≤3 streams by
// outcome, each stream's values ascending by timestamp, the line body carrying
// run_id/scenario_id/correlation_id/outcome, and the stream LABELS bounded
// (job/event_type/outcome/instance/project/cluster) — NFR-3: never run_id/scenario_id/cid.
func TestPushRequestEvents_StreamsLabelsAndBody(t *testing.T) {
	var gotURL, gotCT string
	var captured lokiPushBody
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL, gotCT = r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	// Two scenarios, deliberately out of fire-order so we can assert per-stream sorting.
	rep := &report.Report{RunID: "20260101T101500", Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{
		{ID: "ORD-001", CorrelationID: "tr-20260101T101500-ORD-001-aa", Requests: []report.RequestSample{
			{AtMs: 2000, Outcome: report.OutcomeSuccess},
			{AtMs: 1000, Outcome: report.OutcomeSuccess}, // earlier — must sort BEFORE the 2000 sample
		}},
		{ID: "PERM-001", CorrelationID: "tr-20260101T101500-PERM-001-bb", Requests: []report.RequestSample{
			{AtMs: 1500, Outcome: report.OutcomeFailed}, // a 4xx response on a PASSING scenario
		}},
		{ID: "DWN-001", CorrelationID: "tr-20260101T101500-DWN-001-cc", Requests: []report.RequestSample{
			{AtMs: 1800, Outcome: report.OutcomeError}, // unreachable
		}},
	}}}}

	if err := PushRequestEvents(&Loki{BaseURL: ts.URL}, "local", "order-service", "local", rep); err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotURL != "/loki/api/v1/push" {
		t.Errorf("must POST the Loki push API, got %q", gotURL)
	}
	if gotCT != "application/json" {
		t.Errorf("Loki push must be application/json, got %q", gotCT)
	}
	if len(captured.Streams) != 3 {
		t.Fatalf("want 3 outcome streams (success/failed/error), got %d: %+v", len(captured.Streams), captured.Streams)
	}

	for _, st := range captured.Streams {
		lbl := st.Stream
		// fixed + bounded labels
		if lbl["job"] != "argus-runner" || lbl["event_type"] != "request" {
			t.Errorf("stream missing fixed labels: %+v", lbl)
		}
		if lbl["argus_instance"] != "local" || lbl["project"] != "order-service" || lbl["cluster"] != "local" {
			t.Errorf("stream missing instance/project/cluster labels: %+v", lbl)
		}
		if lbl["outcome"] == "" {
			t.Errorf("stream missing outcome label: %+v", lbl)
		}
		// NFR-3: high-cardinality ids must NOT be stream labels.
		for _, banned := range []string{"run_id", "scenario_id", "correlation_id"} {
			if _, ok := lbl[banned]; ok {
				t.Errorf("NFR-3 violation: %q must not be a stream label: %+v", banned, lbl)
			}
		}
		// per-stream values must be strictly ascending by ns timestamp.
		var last int64 = -1
		for _, v := range st.Values {
			ns, _ := strconv.ParseInt(v[0], 10, 64)
			if ns <= last {
				t.Errorf("stream %q values not strictly ascending: %v", lbl["outcome"], st.Values)
			}
			last = ns
			// body carries the ids + outcome.
			var body map[string]string
			if err := json.Unmarshal([]byte(v[1]), &body); err != nil {
				t.Fatalf("line body not json: %q", v[1])
			}
			if body["run_id"] != "20260101T101500" || body["outcome"] != lbl["outcome"] || body["scenario_id"] == "" || body["correlation_id"] == "" {
				t.Errorf("line body missing ids/outcome: %+v", body)
			}
		}
	}

	// the success stream has 2 samples sorted 1000 then 2000 → ns = atMs*1e6.
	for _, st := range captured.Streams {
		if st.Stream["outcome"] != report.OutcomeSuccess {
			continue
		}
		if len(st.Values) != 2 || st.Values[0][0] != strconv.FormatInt(1000*1_000_000, 10) || st.Values[1][0] != strconv.FormatInt(2000*1_000_000, 10) {
			t.Errorf("success samples must be sorted with ns=atMs*1e6, got %v", st.Values)
		}
	}
}

// AC-D32: a refusal carries Loki's REASON, not just the status. A 429 reads the same for a rate limit
// and a full stream cap; only the body says which, and it is the body an operator needs.
func TestPushRequestEvents_ErrorCarriesLokisReason(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("maximum active stream limit exceeded when trying to create stream " + strings.Repeat("x", 1000)))
	}))
	defer ts.Close()
	rep := &report.Report{RunID: "r", Layers: []report.Layer{{Layer: "l", Scenarios: []report.ScenarioResult{{
		ID: "X", CorrelationID: "tr-r-X-00", Requests: []report.RequestSample{{AtMs: 1, Outcome: report.OutcomeSuccess}},
	}}}}}
	err := PushRequestEvents(&Loki{BaseURL: ts.URL}, "local", "p", "local", rep)
	if err == nil || !strings.Contains(err.Error(), "429: maximum active stream limit exceeded") {
		t.Fatalf("err = %v; want the status AND Loki's reason", err)
	}
	if len(err.Error()) > 400 {
		t.Errorf("the reason must be bounded (it is a log line); got %d bytes", len(err.Error()))
	}
}

// Best-effort no-ops: an empty Loki URL, a nil report, or a run that fired no request must
// not error and must not POST.
func TestPushRequestEvents_Noops(t *testing.T) {
	if err := PushRequestEvents(&Loki{}, "local", "p", "local", &report.Report{}); err != nil {
		t.Errorf("empty url must be a no-op, got %v", err)
	}
	posted := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posted = true }))
	defer ts.Close()
	// a report with scenarios but NO request samples (e.g. a dead rig) → no POST.
	rep := &report.Report{RunID: "r", Layers: []report.Layer{{Layer: "l", Scenarios: []report.ScenarioResult{{ID: "X", Status: "error"}}}}}
	if err := PushRequestEvents(&Loki{BaseURL: ts.URL}, "local", "p", "local", rep); err != nil {
		t.Errorf("zero-sample run must be a no-op, got %v", err)
	}
	if posted {
		t.Error("a run that fired no request must NOT POST to Loki")
	}
}

package obsquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// /. A real load run fired ~3.85M requests; one JSON body of one line per
// request came to ~730 MB and Loki cut it off mid-stream ("unexpected end of input"), so the dashboard
// lost the whole run. The fix aggregates (one line per scenario/correlation/outcome/second, with a
// count) and splits the lines over several pushes.

// fakeLokiBudget is the fake's own limit. It is written out here, NOT read from the package, so the test
// judges the pushes against an independent number: Loki's smallest default on the path is the 4 MiB
// gRPC limit, and the package promises to stay well under it.
const fakeLokiBudget = 2 << 20

type pushSink struct {
	mu       sync.Mutex
	srv      *httptest.Server
	bodies   int
	biggest  int
	rejected int
	// counts is the sum of "count" per outcome label across every accepted push; lines the number of lines.
	counts map[string]int64
	lines  int
	// lastNs tracks, per outcome stream, the newest timestamp accepted, across pushes.
	lastNs     map[string]int64
	orderBreak string
	tenants    map[string]bool
}

func newPushSink(t *testing.T) *pushSink {
	t.Helper()
	f := &pushSink{counts: map[string]int64{}, lastNs: map[string]int64{}, tenants: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tenants[r.Header.Get("X-Scope-OrgID")] = true
		// Loki's distributor reads at most max_recv_msg_size+1 bytes of a JSON body and then reports the
		// cut-off body as "unexpected end of input". Reject the same way: over the budget, 400.
		b, _ := io.ReadAll(io.LimitReader(r.Body, fakeLokiBudget+1))
		f.bodies++
		if len(b) > f.biggest {
			f.biggest = len(b)
		}
		if len(b) > fakeLokiBudget {
			f.rejected++
			http.Error(w, "loghttp.PushRequest.Streams: readStringSlowPath: unexpected end of input", http.StatusBadRequest)
			return
		}
		var body lokiPushBody
		if err := json.Unmarshal(b, &body); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		for _, st := range body.Streams {
			o := st.Stream["outcome"]
			for _, v := range st.Values {
				ns, _ := strconv.ParseInt(v[0], 10, 64)
				if last, ok := f.lastNs[o]; ok && ns <= last && f.orderBreak == "" {
					f.orderBreak = fmt.Sprintf("stream %q: ns %d after %d", o, ns, last)
				}
				f.lastNs[o] = ns
				var line struct {
					Count *int64 `json:"count"`
				}
				if err := json.Unmarshal([]byte(v[1]), &line); err != nil || line.Count == nil {
					http.Error(w, "line without a count: "+v[1], http.StatusBadRequest)
					return
				}
				f.counts[o] += *line.Count
				f.lines++
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// bigReport fires n requests spread over `scenarios` scenarios and `seconds` seconds, with a
// deterministic mix of outcomes, and returns the per-outcome truth counted while generating.
func bigReport(n, scenarios, seconds int) (*report.Report, map[string]int64) {
	want := map[string]int64{}
	scs := make([]report.ScenarioResult, scenarios)
	for i := range scs {
		scs[i] = report.ScenarioResult{ID: fmt.Sprintf("LOAD-%03d", i), CorrelationID: fmt.Sprintf("tr-20261019T154802160-LOAD-%03d-ab", i)}
		scs[i].Requests = make([]report.RequestSample, 0, n/scenarios+1)
	}
	const base = int64(1_792_425_000_000)
	for i := 0; i < n; i++ {
		outcome := report.OutcomeSuccess
		switch i % 7 {
		case 5:
			outcome = report.OutcomeFailed
		case 6:
			outcome = report.OutcomeError
		}
		sc := i % scenarios
		at := base + int64((i/scenarios)%seconds)*1000 + int64(i%997)
		scs[sc].Requests = append(scs[sc].Requests, report.RequestSample{AtMs: at, Outcome: outcome})
		want[outcome]++
	}
	return &report.Report{RunID: "20261019T154802160", Layers: []report.Layer{{Layer: "load", Scenarios: scs}}}, want
}

// The promise: a run of >= 2,000,000 samples is pushed as several bodies, each under the budget, and the
// counts that reach Loki add up to the samples the runner fired, per outcome. The fake REJECTS (400) any
// body over its budget, exactly as Loki does.
func TestPushRequestEvents_TwoMillionSamplesArriveInBudgetedBatches(t *testing.T) {
	f := newPushSink(t)
	rep, want := bigReport(2_000_000, 100, 120)

	if err := PushRequestEvents(&Loki{BaseURL: f.srv.URL, Tenant: "shop-dev"}, "shop-dev", "shop", "cluster-a", rep); err != nil {
		t.Fatalf("push of 2M samples: %v (bodies=%d rejected=%d biggest=%d)", err, f.bodies, f.rejected, f.biggest)
	}
	if f.rejected != 0 {
		t.Errorf("%d bodies were over the %d budget", f.rejected, fakeLokiBudget)
	}
	if f.bodies < 2 {
		t.Errorf("a run this size must be split over several pushes, got %d", f.bodies)
	}
	for _, o := range []string{report.OutcomeSuccess, report.OutcomeFailed, report.OutcomeError} {
		if f.counts[o] != want[o] {
			t.Errorf("outcome %s: summed counts = %d, runner fired %d", o, f.counts[o], want[o])
		}
	}
	if f.lines >= 2_000_000/10 {
		t.Errorf("%d lines for 2,000,000 samples: events are not aggregated", f.lines)
	}
	if f.orderBreak != "" {
		t.Errorf("timestamps must stay strictly increasing per stream across pushes: %s", f.orderBreak)
	}
	if len(f.tenants) != 1 || !f.tenants["shop-dev"] {
		t.Errorf("every batch must carry the tenant header; saw %v", f.tenants)
	}
	t.Logf("bodies=%d biggest=%d bytes lines=%d counts=%v", f.bodies, f.biggest, f.lines, f.counts)
}

// Aggregation keys: same scenario+corr+outcome+second -> ONE line with count; a different second, scenario
// or outcome -> another. The line is stamped at the bucket start.
func TestPushRequestEvents_AggregatesPerSecondAndStampsBucketStart(t *testing.T) {
	var captured lokiPushBody
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	rep := &report.Report{RunID: "r1", Layers: []report.Layer{{Layer: "l", Scenarios: []report.ScenarioResult{
		{ID: "A", CorrelationID: "tr-r1-A-00", Requests: []report.RequestSample{
			{AtMs: 5001, Outcome: report.OutcomeSuccess}, {AtMs: 5999, Outcome: report.OutcomeSuccess}, {AtMs: 5500, Outcome: report.OutcomeSuccess},
			{AtMs: 6000, Outcome: report.OutcomeSuccess}, {AtMs: 5100, Outcome: report.OutcomeFailed},
		}},
		{ID: "B", CorrelationID: "tr-r1-B-00", Requests: []report.RequestSample{{AtMs: 5200, Outcome: report.OutcomeSuccess}}},
	}}}}
	if err := PushRequestEvents(&Loki{BaseURL: ts.URL}, "i", "p", "c", rep); err != nil {
		t.Fatal(err)
	}
	type row struct {
		ns    string
		scen  string
		count int64
	}
	got := map[string][]row{}
	for _, st := range captured.Streams {
		for _, v := range st.Values {
			var l requestEventLine
			if err := json.Unmarshal([]byte(v[1]), &l); err != nil {
				t.Fatal(err)
			}
			if l.RunID != "r1" || l.Outcome != st.Stream["outcome"] || l.CorrelationID == "" {
				t.Errorf("bad line %+v", l)
			}
			got[st.Stream["outcome"]] = append(got[st.Stream["outcome"]], row{v[0], l.ScenarioID, l.Count})
		}
	}
	// success: A@5s x3, B@5s x1 (same bucket, nudged +1ns so the stream stays strictly increasing), A@6s x1.
	want := []row{{"5000000000", "A", 3}, {"5000000001", "B", 1}, {"6000000000", "A", 1}}
	if fmt.Sprint(got[report.OutcomeSuccess]) != fmt.Sprint(want) {
		t.Errorf("success rows = %v, want %v", got[report.OutcomeSuccess], want)
	}
	if fmt.Sprint(got[report.OutcomeFailed]) != fmt.Sprint([]row{{"5000000000", "A", 1}}) {
		t.Errorf("failed rows = %v", got[report.OutcomeFailed])
	}
}

// A later batch that fails says how far the push got.
func TestPushRequestEvents_LaterBatchFailureSaysHowManyLinesGotThrough(t *testing.T) {
	var mu sync.Mutex
	calls, lines := 0, 0
	acceptedLines := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		b, _ := io.ReadAll(r.Body)
		var body lokiPushBody
		_ = json.Unmarshal(b, &body)
		n := 0
		for _, s := range body.Streams {
			n += len(s.Values)
		}
		if calls == 3 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		acceptedLines += n
		lines += n
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	rep, _ := bigReport(300_000, 60, 400) // 60*400 keys x outcomes, several MiB of lines
	err := PushRequestEvents(&Loki{BaseURL: ts.URL}, "i", "p", "c", rep)
	if err == nil {
		t.Fatal("a refused batch must be an error")
	}
	if calls != 3 {
		t.Errorf("must stop at the first failing batch, saw %d calls", calls)
	}
	var pe *PushStatusError
	if !errors.As(err, &pe) || pe.Status != 500 {
		t.Errorf("the cause must stay reachable as a *PushStatusError 500: %v", err)
	}
	prefix := fmt.Sprintf("%d/", acceptedLines)
	if acceptedLines == 0 || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), "lines sent (batch 3/") {
		t.Errorf("error %q must start with %q lines accepted and name batch 3", err.Error(), prefix)
	}
}

// The whole push is bounded: a Loki that never answers in time cannot hold the run's result upload.
func TestPushRequestEvents_WholePushIsTimeBounded(t *testing.T) {
	old := lokiPushTotalBudget
	lokiPushTotalBudget = 300 * time.Millisecond
	defer func() { lokiPushTotalBudget = old }()
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	defer ts.Close()
	defer close(release) // runs first: let the stuck handler go so Close does not wait for it
	rep, _ := bigReport(300_000, 60, 400)
	start := time.Now()
	err := PushRequestEvents(&Loki{BaseURL: ts.URL}, "i", "p", "c", rep)
	if err == nil || !strings.Contains(err.Error(), "lines sent (batch 1/") {
		t.Fatalf("err = %v; want a timeout that says 0 lines of N were sent", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("push took %v with a 300ms budget", d)
	}
}

// Loki's per-tenant ingestion rate limit is a 429 that clears: retry it. A 429 about anything else is not
// retried (AC-D32: the stream cap does not clear).
func TestPushRequestEvents_RetriesOnlyTheRateLimit(t *testing.T) {
	old := lokiRateLimitBackoff
	lokiRateLimitBackoff = time.Millisecond
	defer func() { lokiRateLimitBackoff = old }()
	rep := &report.Report{RunID: "r", Layers: []report.Layer{{Layer: "l", Scenarios: []report.ScenarioResult{{
		ID: "X", CorrelationID: "tr-r-X-00", Requests: []report.RequestSample{{AtMs: 1, Outcome: report.OutcomeSuccess}},
	}}}}}

	var n int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			http.Error(w, "Ingestion rate limit exceeded for user fake (limit: 4194304 bytes/sec)", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := PushRequestEvents(&Loki{BaseURL: ts.URL}, "i", "p", "c", rep); err != nil || n != 3 {
		t.Errorf("rate limit: err=%v after %d posts, want success on the 3rd", err, n)
	}
	ts.Close()

	n = 0
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Error(w, "maximum active stream limit exceeded", http.StatusTooManyRequests)
	}))
	defer ts2.Close()
	if err := PushRequestEvents(&Loki{BaseURL: ts2.URL}, "i", "p", "c", rep); err == nil || n != 1 {
		t.Errorf("stream cap: err=%v after %d posts, want an error after exactly 1", err, n)
	}
}

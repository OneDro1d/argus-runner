package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/federation"
)

func promServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func vec(vals ...string) string {
	r := `{"status":"success","data":{"resultType":"vector","result":[`
	for i, v := range vals {
		if i > 0 {
			r += ","
		}
		r += `{"metric":{"instance":"a"},"value":[1790000000.1,"` + v + `"]}`
	}
	return r + `]}}`
}

func TestReadPrometheus_ParsesOneFiniteSample(t *testing.T) {
	srv := promServer(t, vec("212"), 200)
	v, err := readPrometheus(context.Background(), srv.Client(), config.SummarySource{Type: "prometheus", URL: srv.URL}, "sum(x)")
	if err != nil || v != 212 {
		t.Fatalf("v=%v err=%v, want 212", v, err)
	}
	// scalar result type
	srv2 := promServer(t, `{"status":"success","data":{"resultType":"scalar","result":[1790000000,"3.5"]}}`, 200)
	if v, err := readPrometheus(context.Background(), srv2.Client(), config.SummarySource{URL: srv2.URL}, "scalar(x)"); err != nil || v != 3.5 {
		t.Fatalf("scalar: v=%v err=%v", v, err)
	}
}

func TestReadPrometheus_SendsTheQueryAndBasicAuth(t *testing.T) {
	var gotQ, gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQ = r.URL.Query().Get("query")
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = w.Write([]byte(vec("1")))
	}))
	defer srv.Close()
	_, err := readPrometheus(context.Background(), srv.Client(), config.SummarySource{URL: srv.URL + "/", Credential: "svc:p@ss:word"}, `sum(rate(a_total[5m]))`)
	if err != nil {
		t.Fatal(err)
	}
	if gotQ != `sum(rate(a_total[5m]))` || gotUser != "svc" || gotPass != "p@ss:word" {
		t.Errorf("query=%q user=%q pass=%q", gotQ, gotUser, gotPass)
	}
}

func TestReadPrometheus_NotOneFiniteSampleIsAnError(t *testing.T) {
	for name, c := range map[string]struct {
		body   string
		status int
	}{
		"no series":    {vec(), 200},
		"two series":   {vec("1", "2"), 200},
		"NaN":          {vec("NaN"), 200},
		"+Inf":         {vec("+Inf"), 200},
		"not a number": {vec("abc"), 200},
		"garbage":      {`<html>sign in</html>`, 200},
		"empty":        {``, 200},
		"http 503":     {`upstream down`, 503},
		"prom error":   {`{"status":"error","errorType":"bad_data","error":"parse error"}`, 400},
		"matrix":       {`{"status":"success","data":{"resultType":"matrix","result":[]}}`, 200},
	} {
		srv := promServer(t, c.body, c.status)
		v, err := readPrometheus(context.Background(), srv.Client(), config.SummarySource{URL: srv.URL}, "q")
		if err == nil {
			t.Errorf("%s: returned %v with no error; an unreadable source must be an error, never a number", name, v)
		}
	}
}

const metricsBody = `# HELP agents_online connected agents
# TYPE agents_online gauge
agents_online{region="eu",tier="a"} 100
agents_online{region="eu",tier="b"} 50
agents_online{region="us",tier="a"} 7 1790000000000
agents_online_total 999
plain_gauge 4.25
quoted{path="/a b",note="say \"hi\" {x}"} 11
broker_memory_bytes{node="n1"} 1.5e9
nan_metric NaN
`

func metricsServer(t *testing.T, body string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return srv
}

func TestReadMetricsText_SumsMatchingSeries(t *testing.T) {
	srv := metricsServer(t, metricsBody)
	src := config.SummarySource{Type: "metrics_endpoint", URL: srv.URL + "/metrics"}
	for q, want := range map[string]float64{
		`agents_online`:                       157, // summed over every label set; agents_online_total is another series
		`agents_online{region="eu"}`:          150,
		`agents_online{region="eu",tier="b"}`: 50,
		`plain_gauge`:                         4.25,
		`quoted{path="/a b"}`:                 11,
		`broker_memory_bytes`:                 1.5e9,
	} {
		v, err := readMetricsText(context.Background(), srv.Client(), src, q)
		if err != nil || v != want {
			t.Errorf("%s: v=%v err=%v, want %v", q, v, err, want)
		}
	}
	for _, q := range []string{`absent_series`, `agents_online{region="mars"}`, `nan_metric`} {
		if v, err := readMetricsText(context.Background(), srv.Client(), src, q); err == nil {
			t.Errorf("%s: returned %v with no error", q, v)
		}
	}
	garbage := metricsServer(t, "<html>not metrics</html>\n\x00\x01")
	if v, err := readMetricsText(context.Background(), garbage.Client(), config.SummarySource{URL: garbage.URL}, "x"); err == nil {
		t.Errorf("garbage body read as %v", v)
	}
}

func TestReadSummary_UnreachableAndGarbageAreReportedUnreadableNotZero(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	sm := &config.SummaryMetrics{
		Every:  "3m",
		Source: config.SummarySource{Type: "prometheus", URL: deadURL, Credential: "svc:topsecret"},
		Readings: []config.SummaryReadingDecl{
			{Name: "agents_connected", Target: "live", Unit: "agents", Query: "sum(x)"},
		},
	}
	got := ReadSummary(context.Background(), &http.Client{Timeout: time.Second}, sm, now)
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	r := got[0] // (Fatalf above stops here, so no panic on an empty result)
	if r.Value != nil {
		t.Fatalf("an unreachable source was reported as the number %v", *r.Value)
	}
	if r.Error == "" {
		t.Error("an unreadable reading must say why")
	}
	for _, banned := range []string{deadURL, "127.0.0.1", "topsecret", "svc:"} {
		if strings.Contains(r.Error, banned) {
			t.Errorf("error %q leaks %q", r.Error, banned)
		}
	}
	if r.Name != "agents_connected" || r.Target != "live" || r.Unit != "agents" || !r.ObservedAt.Equal(now) || r.EverySeconds != 180 {
		t.Errorf("metadata not carried: %+v", r)
	}

	garbage := promServer(t, "<html>login</html>", 200)
	sm.Source.URL = garbage.URL
	if got := ReadSummary(context.Background(), garbage.Client(), sm, now); len(got) != 1 || got[0].Value != nil || got[0].Error == "" {
		t.Errorf("garbage source: %+v", got)
	}
}

func TestReadSummary_OneBadReadingDoesNotHideTheGoodOnes_AndTheBoundHolds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "bad" {
			_, _ = w.Write([]byte(vec()))
			return
		}
		_, _ = w.Write([]byte(vec("42")))
	}))
	defer srv.Close()
	sm := &config.SummaryMetrics{Source: config.SummarySource{Type: "prometheus", URL: srv.URL}}
	sm.Readings = append(sm.Readings, config.SummaryReadingDecl{Name: "bad", Query: "bad"})
	for i := 0; i < 30; i++ {
		sm.Readings = append(sm.Readings, config.SummaryReadingDecl{Name: "r" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Query: "good"})
	}
	got := ReadSummary(context.Background(), srv.Client(), sm, time.Now())
	if len(got) != federation.SummaryMaxReadings {
		t.Fatalf("read %d readings; the bound is %d even if a config slipped past validation", len(got), federation.SummaryMaxReadings)
	}
	if len(got) < 2 || got[0].Value != nil || got[1].Value == nil || *got[1].Value != 42 {
		t.Errorf("bad/good mixed wrongly: %+v %+v", got[0], got[1])
	}
}

func TestReadSummary_EachReadingIsBoundedInTime(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	sm := &config.SummaryMetrics{Source: config.SummarySource{Type: "prometheus", URL: srv.URL},
		Readings: []config.SummaryReadingDecl{{Name: "slow", Query: "q"}}}
	old := summaryReadTimeout
	summaryReadTimeout = 200 * time.Millisecond
	defer func() { summaryReadTimeout = old }()
	start := time.Now()
	got := ReadSummary(context.Background(), srv.Client(), sm, time.Now())
	if time.Since(start) > 3*time.Second {
		t.Fatalf("a hung source blocked the reader for %v", time.Since(start))
	}
	if len(got) != 1 || got[0].Value != nil || got[0].Error == "" {
		t.Errorf("a timed-out reading must be unreadable with a reason: %+v", got)
	}
}

func writeSummaryConfig(t *testing.T, srvURL string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: x\ntargets:\n  http:\n    base_url: http://sut:8080\n" +
		"summary_metrics:\n  every: 1m\n  source: { type: prometheus, url: \"" + srvURL + "\" }\n" +
		"  readings:\n    - { name: agents_connected, unit: agents, query: 'sum(x)' }\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSummaryTicker_TakeIsOncePerBatch_UnsendRearms(t *testing.T) {
	srv := promServer(t, vec("9"), 200)
	tk := NewSummaryTicker(writeSummaryConfig(t, srv.URL), t.Logf)
	if got := tk.Take(); got != nil {
		t.Fatalf("nothing read yet, Take = %+v", got)
	}
	now := time.Now()
	tk.Cycle(context.Background(), now)
	b := tk.Take()
	if len(b) != 1 || b[0].Value == nil || *b[0].Value != 9 {
		t.Fatalf("batch = %+v", b)
	}
	if again := tk.Take(); again != nil {
		t.Errorf("the same batch was handed to a second poll: %+v", again)
	}
	tk.Unsend()
	if re := tk.Take(); len(re) != 1 {
		t.Errorf("after a failed poll the batch must be handed again, got %+v", re)
	}
}

func TestSummaryTicker_MinimumIntervalIsHonoured(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte(vec("1")))
	}))
	defer srv.Close()
	tk := NewSummaryTicker(writeSummaryConfig(t, srv.URL), t.Logf) // every: 1m
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	tk.Cycle(context.Background(), t0)
	tk.Cycle(context.Background(), t0.Add(10*time.Second))
	tk.Cycle(context.Background(), t0.Add(59*time.Second))
	mu.Lock()
	n := hits
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d reads inside one interval, want 1", n)
	}
	tk.Cycle(context.Background(), t0.Add(61*time.Second))
	mu.Lock()
	n = hits
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d reads after the interval, want 2", n)
	}
}

func TestSummaryTicker_NoBlockMeansNothingToSend(t *testing.T) {
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	_ = os.WriteFile(p, []byte("project:\n  name: x\ntargets:\n  http:\n    base_url: http://sut:8080\n"), 0o600)
	tk := NewSummaryTicker(p, t.Logf)
	tk.Cycle(context.Background(), time.Now())
	if got := tk.Take(); got != nil {
		t.Errorf("no summary_metrics block but Take = %+v", got)
	}
}

// The poll carries the batch once, and the body is byte-identical to today's when there is none.
func TestClientPoll_AttachesSummaryReadingsOnce_AndResendsAfterAFailure(t *testing.T) {
	var mu sync.Mutex
	var bodies []federation.PollRequest
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b federation.PollRequest
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		f := fail
		mu.Unlock()
		if f {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(federation.PollResponse{})
	}))
	defer srv.Close()
	priv, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	c := NewClient(srv.URL, "inst-sum", priv)

	// no ticker: nothing on the wire
	if _, err := c.Poll(context.Background(), "1.0.0", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(bodies[0].SummaryReadings) != 0 {
		t.Fatalf("no ticker but readings were sent: %+v", bodies[0].SummaryReadings)
	}

	prom := promServer(t, vec("5"), 200)
	c.Summary = NewSummaryTicker(writeSummaryConfig(t, prom.URL), t.Logf)
	c.Summary.Cycle(context.Background(), time.Now())
	poll := func() error {
		_, err := c.Poll(context.Background(), "1.0.0", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil)
		return err
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	if err := poll(); err == nil {
		t.Fatal("control: the failing CP should error the poll")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := poll(); err != nil {
		t.Fatal(err)
	}
	if err := poll(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// bodies: [0] none, [1] failed with batch, [2] resent with batch, [3] none again
	if len(bodies) != 4 {
		t.Fatalf("%d polls seen", len(bodies))
	}
	if len(bodies[1].SummaryReadings) != 1 || len(bodies[2].SummaryReadings) != 1 {
		t.Errorf("the failed poll's batch was not resent: %d, %d", len(bodies[1].SummaryReadings), len(bodies[2].SummaryReadings))
	}
	if len(bodies[3].SummaryReadings) != 0 {
		t.Errorf("a delivered batch was sent again: %+v", bodies[3].SummaryReadings)
	}
	if len(bodies[2].SummaryReadings) == 0 {
		return
	}
	if r := bodies[2].SummaryReadings[0]; r.Name != "agents_connected" || r.Value == nil || *r.Value != 5 || r.Unit != "agents" {
		t.Errorf("reading on the wire = %+v", r)
	}
}

// Loop wiring: a config with summary_metrics makes the running executor's polls carry the numbers.
func TestExecutorLoop_PollsCarrySummaryReadings(t *testing.T) {
	prom := promServer(t, vec("77"), 200)
	cfgPath := writeSummaryConfig(t, prom.URL)
	var mu sync.Mutex
	var seen []federation.SummaryReading
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b federation.PollRequest
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		seen = append(seen, b.SummaryReadings...)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(federation.PollResponse{})
	}))
	defer srv.Close()
	priv, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "id.key"))
	ex := &Executor{
		Client: NewClient(srv.URL, "inst-sum-loop", priv), RunnerVersion: "1.0.0", ConfigPath: cfgPath,
		PollGap: 10 * time.Millisecond, Backoff: 10 * time.Millisecond,
		Run: func(context.Context, *federation.RunAssignment) (federation.ResultsPush, error) {
			return federation.ResultsPush{}, nil
		},
		Log: func(f string, a ...any) { t.Logf(f, a...) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ex.Loop(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[0].Name != "agents_connected" || seen[0].Value == nil || *seen[0].Value != 77 {
		t.Fatalf("the loop's polls never carried the reading: %+v", seen)
	}
}

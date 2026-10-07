package argus

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envcapture"
	"github.com/OneDro1d/argus-runner/internal/httpload"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// HTTP Load. The fakes here write JTL fixtures; the two end-to-end tests at the
// bottom send REAL requests to an httptest.Server that breaks above a known load: one through an in-process
// generator that honours the step's properties (it always runs), one through real JMeter (it skips without one).

func httpLoadMD(id, steps, extraLoad string) string {
	return "# Scenario: " + id + "\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: HTTP Load\n- **Tags**: http, load\n- **Target**: api-lab\n\n" +
		"## TRIGGER\nGET `/api/items`\n\n## EXPECT\n### Runnable\n- every step is measured\n- the smallest step is comfortable\n\n" +
		"## LOAD\n- **Steps**: " + steps + "\n" + extraLoad +
		"\n## TIMEOUT\n5s\n\n## CLEANUP\nN/A — GET only, it creates nothing.\n"
}

const httpLoadDefaultLoad = "- **Step Duration Seconds**: 20\n- **Ramp Seconds**: 0\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.05\n"

func writeHTTPLoad(t *testing.T, dir, id, steps, extraLoad string) {
	t.Helper()
	d := filepath.Join(dir, "http-load")
	_ = os.MkdirAll(d, 0o755)
	md := httpLoadMD(id, steps, extraLoad)
	if _, errs := scenario.Validate(md); len(errs) != 0 {
		t.Fatalf("fixture is not a valid HTTP Load scenario: %v", errs)
	}
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func httpLoadConfig(baseURL, allow string) string {
	return "project:\n  name: lab\ntargets:\n  http:\n    base_url: http://sut.invalid\n  http_targets:\n    api-lab:\n      base_url: " + baseURL + "\n" + allow
}

const allowAPILab = "load_allowed_targets:\n  api-lab:\n    max_sessions: 100\n"

// httpJTL: `users` users, one request a second each over a 20 s step, every one answered `code` in `el` ms.
func httpJTL(id string, props map[string]string, code string, el int) string {
	users, _ := strconv.Atoi(props["load.users"])
	var b strings.Builder
	b.WriteString(jtlHead)
	t0 := int64(1_000_000)
	for sec := 0; sec <= 20; sec++ {
		for u := 0; u < users; u++ {
			fmt.Fprintf(&b, "%d,%d,%s,%s,msg,T %d-1,%v,\n", t0+int64(sec)*1000, el, id, code, u, code == "200")
		}
	}
	return b.String()
}

type httpLoadEnv struct {
	dir, results string
	sleeps       []time.Duration
	envCalls     int
}

func newHTTPLoadEnv(t *testing.T) *httpLoadEnv {
	t.Helper()
	e := &httpLoadEnv{dir: t.TempDir()}
	e.results = filepath.Join(e.dir, "results")
	oldSleep, oldEnv, oldCPU := httpLoadSleep, httpLoadEnvCapture, httpLoadCPUStat
	httpLoadSleep = func(d time.Duration) { e.sleeps = append(e.sleeps, d) }
	httpLoadEnvCapture = func(string) *envcapture.Capture { e.envCalls++; return nil }
	// #615: the host's real cgroup is never read by these tests (a throttled CI box would flip them); a test that
	// wants a reading swaps this again.
	httpLoadCPUStat = func() (httpload.CPUStat, bool) { return httpload.CPUStat{}, false }
	t.Cleanup(func() { httpLoadSleep, httpLoadEnvCapture, httpLoadCPUStat = oldSleep, oldEnv, oldCPU })
	return e
}

func (e *httpLoadEnv) run(t *testing.T, cfg string, f Runner) report.ScenarioResult {
	t.Helper()
	c := loadCfg(t, cfg)
	rr, err := RunAll(c, filepath.Join(e.dir, "scenarios"), e.results, "lab", "", "", "", "20261007T120000000", f)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range rr.Report.Layers {
		for _, s := range l.Scenarios {
			if strings.HasPrefix(s.ID, "HL-") {
				return s
			}
		}
	}
	t.Fatalf("no HTTP Load row in the report: %+v", rr.Report)
	return report.ScenarioResult{}
}

// countingServer counts every request that reaches it: the "no request was sent" half of a refusal test.
func countingServer(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// ── promise 4: S9 and the hard caps ──────────────────────────────────────────────────────────────────

// THE NAMED TEST of the wiring mutation: deleting the httpLoadRefusal CALL in runOneScenario turns this red.
func TestHTTPLoad_NotAllowedIsRefusedBeforeAnyRequest(t *testing.T) {
	e := newHTTPLoadEnv(t)
	srv, hits := countingServer(t)
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-001", "2, 4", httpLoadDefaultLoad)
	f := &loadFake{gen: func(n int, p map[string]string) string { return httpJTL("HL-001", p, "200", 5) }}
	res := e.run(t, httpLoadConfig(srv.URL, ""), f)

	if len(f.calls) != 0 || atomic.LoadInt64(hits) != 0 {
		t.Fatalf("a target not under load_allowed_targets got %d JMeter run(s) and %d request(s)", len(f.calls), atomic.LoadInt64(hits))
	}
	if e.envCalls != 0 || len(e.sleeps) != 0 {
		t.Errorf("a refused ramp touched the environment (%d) or slept (%d)", e.envCalls, len(e.sleeps))
	}
	if res.Status != report.StatusError || res.Failure == nil {
		t.Fatalf("status = %q, want error (nothing was measured)", res.Status)
	}
	t.Logf("REFUSAL TEXT: %s", res.Failure.Observed)
	for _, want := range []string{"refused before firing", `http target "api-lab"`, "load_allowed_targets", "Nothing was sent", "preflight"} {
		if !strings.Contains(res.Failure.Observed, want) {
			t.Errorf("refusal lacks %q", want)
		}
	}
	if strings.Contains(res.Failure.Observed, srv.URL) || strings.Contains(res.Failure.Observed, "127.0.0.1") {
		t.Errorf("refusal echoes the target's address: %s", res.Failure.Observed)
	}
	if len(res.LoadSteps) != 0 || res.LoadDriver != "" {
		t.Errorf("a refused ramp carries load results: %+v", res)
	}
}

func TestHTTPLoad_CeilingRefusedBeforeAnyRequest(t *testing.T) {
	e := newHTTPLoadEnv(t)
	srv, hits := countingServer(t)
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-001", "2, 500", httpLoadDefaultLoad)
	f := &loadFake{gen: func(n int, p map[string]string) string { return httpJTL("HL-001", p, "200", 5) }}
	res := e.run(t, httpLoadConfig(srv.URL, allowAPILab), f)
	if len(f.calls) != 0 || atomic.LoadInt64(hits) != 0 || res.Status != report.StatusError ||
		!strings.Contains(res.Failure.Observed, "its Steps reach 500 users and load_allowed_targets.api-lab.max_sessions is 100") {
		t.Fatalf("calls=%d hits=%d status=%s failure=%+v", len(f.calls), atomic.LoadInt64(hits), res.Status, res.Failure)
	}
}

// The caps are enforced at RUN time too: a profile that reached the door without the parser (built by hand) is
// refused before anything is sent, by the door and by the run loop itself.
func TestHTTPLoad_HardCapsAreEnforcedAtRunTime(t *testing.T) {
	srv, hits := countingServer(t)
	c := loadCfg(t, httpLoadConfig(srv.URL, "load_allowed_targets:\n  api-lab:\n    max_sessions: 10000\n"))
	base := scenario.Parse(httpLoadMD("HL-009", "2", httpLoadDefaultLoad))
	cases := map[string]scenario.HTTPLoadProfile{
		"13 steps":            {Steps: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}, StepDurationSeconds: 20, TargetP95Ms: 1, MaxErrorRate: 1},
		"5000 users":          {Steps: []int{10, 5000}, StepDurationSeconds: 20, TargetP95Ms: 1, MaxErrorRate: 1},
		"a 5 s step":          {Steps: []int{10}, StepDurationSeconds: 5, TargetP95Ms: 1, MaxErrorRate: 1},
		"a 2 h step":          {Steps: []int{10}, StepDurationSeconds: 7200, TargetP95Ms: 1, MaxErrorRate: 1},
		"steps that fall":     {Steps: []int{40, 20}, StepDurationSeconds: 20, TargetP95Ms: 1, MaxErrorRate: 1},
		"a ramp over half":    {Steps: []int{10}, StepDurationSeconds: 20, RampSeconds: 15, TargetP95Ms: 1, MaxErrorRate: 1},
		"a settle over 10min": {Steps: []int{10}, StepDurationSeconds: 20, SettleSeconds: 601, TargetP95Ms: 1, MaxErrorRate: 1},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			s := *base
			pp := p
			s.HTTPLoad = &pp
			ref := httpLoadRefusal(c, &s, "tr-20261007T120000000-HL-009-00000000")
			if ref == nil || ref.Status != report.StatusError || !strings.Contains(ref.Failure.Observed, "refused before firing") {
				t.Fatalf("door: %+v", ref)
			}
			t.Logf("%s: %s", name, ref.Failure.Observed)
			f := &loadFake{gen: func(int, map[string]string) string { return "" }}
			res := runHTTPLoad(c, &s, "tr-x", t.TempDir(), f, "build")
			if len(f.calls) != 0 || res.Status != report.StatusError {
				t.Fatalf("run loop: calls=%d status=%s", len(f.calls), res.Status)
			}
		})
	}
	if atomic.LoadInt64(hits) != 0 {
		t.Fatalf("%d request(s) reached the target", atomic.LoadInt64(hits))
	}
}

// ── promise 2/3: the run loop ────────────────────────────────────────────────────────────────────────

func TestHTTPLoad_OneJMeterRunPerStep_WithTheScenariosRequest(t *testing.T) {
	e := newHTTPLoadEnv(t)
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-001", "2, 4", httpLoadDefaultLoad+"- **Settle Seconds**: 15\n")
	f := &loadFake{gen: func(n int, p map[string]string) string { return httpJTL("HL-001", p, "200", 5) }}
	res := e.run(t, httpLoadConfig("http://api-lab.invalid:8081", allowAPILab), f)

	if len(f.calls) != 2 {
		t.Fatalf("runner calls = %d, want one per step (2)", len(f.calls))
	}
	for i, want := range []string{"2", "4"} {
		c := f.calls[i]
		if c.base != "http-ingestion" || c.props["load.users"] != want {
			t.Errorf("call %d: base=%s users=%s", i, c.base, c.props["load.users"])
		}
		if !strings.HasSuffix(c.jtl, fmt.Sprintf("http-load__HL-001__step%d.jtl", i+1)) {
			t.Errorf("call %d jtl = %s", i, c.jtl)
		}
		for k, v := range map[string]string{
			"load.scheduler": "true", "load.duration": "20", "load.ramp": "0", "load.loops": "-1", "http.keepalive": "true",
			"http.host": "api-lab.invalid", "http.port": "8081", "trigger.method": "GET", "trigger.path": "/api/items",
			"scenario.id": "HL-001", "jmeter.save.saveservice.output_format": "csv", "jmeter.save.saveservice.print_field_names": "true",
			"jmeter.save.saveservice.timestamp_format": "ms",
		} {
			if c.props[k] != v {
				t.Errorf("call %d props[%s] = %q, want %q", i, k, c.props[k], v)
			}
		}
		if c.timeout != 20*time.Second+2*5*time.Second+httpLoadStepMargin+jmeterProcessGrace {
			t.Errorf("backstop = %v", c.timeout)
		}
	}
	if len(e.sleeps) != 1 || e.sleeps[0] != 15*time.Second {
		t.Errorf("settle sleeps = %v, want one 15s pause between the two steps", e.sleeps)
	}
	if res.Status != "passed" || res.LoadDriver != "http" || res.LoadTarget != "api-lab" || len(res.LoadSteps) != 2 || res.LoadStoppedAtStep != 0 {
		t.Fatalf("row = status %s driver %q target %q steps %d stopped %d (%+v)", res.Status, res.LoadDriver, res.LoadTarget, len(res.LoadSteps), res.LoadStoppedAtStep, res.Failure)
	}
	s := res.LoadSteps[1]
	if s.Step != 2 || s.Sessions != 4 || s.Status != "measured" || !s.Comfortable || s.ResponseUs == nil || s.ResponseUs.P95 != 5000 || s.DeliveredPerS != 4.2 {
		t.Errorf("step 2 = %+v", s)
	}
	if res.ReqSuccess != 2*21+4*21 || res.ReqFailed != 0 || res.AssertionsEnforcedCount != 2 {
		t.Errorf("req ok/failed = %d/%d, enforced = %d", res.ReqSuccess, res.ReqFailed, res.AssertionsEnforcedCount)
	}
	if u := UnexecutedRunnable(scenario.Parse(httpLoadMD("HL-001", "2, 4", httpLoadDefaultLoad))); len(u) != 0 {
		t.Errorf("the HTTP Load vocabulary is reported as unexecuted: %+v", u)
	}
}

func TestHTTPLoad_RampStopsAtTheFirstStepThatIsNotComfortable_AndNamesIt(t *testing.T) {
	e := newHTTPLoadEnv(t)
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-001", "2, 4, 8, 16", httpLoadDefaultLoad)
	f := &loadFake{gen: func(n int, p map[string]string) string {
		if n == 2 {
			return httpJTL("HL-001", p, "503", 2) // the 8-user step breaks
		}
		return httpJTL("HL-001", p, "200", 5)
	}}
	res := e.run(t, httpLoadConfig("http://api-lab.invalid", allowAPILab), f)
	if len(f.calls) != 3 {
		t.Fatalf("the ramp went on past the step that broke: %d runs, want 3", len(f.calls))
	}
	got := []string{}
	for _, s := range res.LoadSteps {
		got = append(got, fmt.Sprintf("%d:%s:%v", s.Sessions, s.Status, s.Comfortable))
	}
	if strings.Join(got, " ") != "2:measured:true 4:measured:true 8:measured:false 16:not_run:false" {
		t.Errorf("steps = %v", got)
	}
	if res.LoadStoppedAtStep != 3 {
		t.Errorf("stopped at = %d, want 3 (the 8-user step)", res.LoadStoppedAtStep)
	}
	if res.Status != "passed" {
		t.Errorf("status = %s (%+v): breaking above the first step is a measured limit", res.Status, res.Failure)
	}
	if s := res.LoadSteps[2]; s.Errors["status:503"] != 8*21 || s.DeliveredRatio != 0 {
		t.Errorf("the broken step's record = %+v", s)
	}
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"load_stopped_at_step":3`) || !strings.Contains(string(b), `"load_driver":"http"`) {
		t.Errorf("report row lacks the stop or the driver: %s", b)
	}
}

func TestHTTPLoad_SmallestStepNotComfortable_Fails(t *testing.T) {
	e := newHTTPLoadEnv(t)
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-001", "2, 4", httpLoadDefaultLoad)
	f := &loadFake{gen: func(n int, p map[string]string) string { return httpJTL("HL-001", p, "200", 900) }} // p95 900 ms > 250
	res := e.run(t, httpLoadConfig("http://api-lab.invalid", allowAPILab), f)
	if len(f.calls) != 1 || res.Status != "failed" || !strings.Contains(res.Failure.Observed, "even the smallest (2 users) was not (response p95 900.0 ms") {
		t.Fatalf("calls=%d status=%s failure=%+v", len(f.calls), res.Status, res.Failure)
	}
	if strings.Contains(res.Failure.Observed, "250") {
		t.Errorf("the observed text carries the declared threshold: %s", res.Failure.Observed)
	}
}

// ── promise 3, end to end: a real ramp against a server that breaks above a known load ─────────────────

// breakingServer answers 200 after 20 ms while at most `limit` requests are in flight, and 503 above that. It
// records when each request arrived and the most it ever had in flight.
type breakingServer struct {
	*httptest.Server
	inflight, maxInflight, total int64
	mu                           sync.Mutex
	arrivals                     []time.Time
}

func newBreakingServer(t *testing.T, limit int64) *breakingServer {
	t.Helper()
	b := &breakingServer{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.arrivals = append(b.arrivals, time.Now())
		b.mu.Unlock()
		atomic.AddInt64(&b.total, 1)
		n := atomic.AddInt64(&b.inflight, 1)
		defer atomic.AddInt64(&b.inflight, -1)
		for {
			m := atomic.LoadInt64(&b.maxInflight)
			if n <= m || atomic.CompareAndSwapInt64(&b.maxInflight, m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		if n > limit {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"overloaded"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(b.Server.Close)
	return b
}

func (b *breakingServer) arrivedAfter(at time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, a := range b.arrivals {
		if a.After(at) {
			n++
		}
	}
	return n
}

// stepClockRunner records when each Run (one step) returned.
type stepClockRunner struct {
	inner Runner
	ends  []time.Time
	users []string
}

func (r *stepClockRunner) Run(base, jtl string, props map[string]string, timeout time.Duration) error {
	err := r.inner.Run(base, jtl, props, timeout)
	r.ends = append(r.ends, time.Now())
	r.users = append(r.users, props["load.users"])
	return err
}

// genRunner is an in-process stand-in for JMeter's http-ingestion thread group: load.users users, each a
// keep-alive client looping the scenario's request back to back until load.duration seconds have passed, every
// request written to the JTL exactly as JMeter writes it (start ms, elapsed ms, the scenario id as the label).
type genRunner struct{}

func (genRunner) Run(base, jtl string, props map[string]string, timeout time.Duration) error {
	users, _ := strconv.Atoi(props["load.users"])
	dur, _ := strconv.Atoi(props["load.duration"])
	url := props["http.protocol"] + "://" + props["http.host"] + ":" + props["http.port"] + props["trigger.path"]
	deadline := time.Now().Add(time.Duration(dur) * time.Second)
	var mu sync.Mutex
	var b strings.Builder
	b.WriteString(jtlHead)
	w := csv.NewWriter(&b) // a transport error's text carries quotes: write it as JMeter does, CSV-escaped
	var wg sync.WaitGroup
	for u := 0; u < users; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 1}}
			defer client.CloseIdleConnections()
			for time.Now().Before(deadline) {
				start := time.Now()
				req, _ := http.NewRequest(props["trigger.method"], url, nil)
				req.Header.Set("X-Correlation-Id", props["correlation.id"])
				resp, err := client.Do(req)
				el := time.Since(start).Milliseconds()
				code, msg := "", ""
				if err != nil {
					code, msg = "Non HTTP response code: java.net.ConnectException", "Non HTTP response message: "+err.Error()
				} else {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					code, msg = strconv.Itoa(resp.StatusCode), resp.Status
				}
				mu.Lock()
				_ = w.Write([]string{strconv.FormatInt(start.UnixMilli(), 10), strconv.FormatInt(el, 10), props["scenario.id"], code, msg,
					fmt.Sprintf("T 1-%d", u), strconv.FormatBool(code == "200"), ""})
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	w.Flush()
	return os.WriteFile(jtl, []byte(b.String()), 0o600)
}

// endToEndHTTPRamp runs Steps 2, 4, 8, 16 users (10 s each, no ramp, no settle) against a server that answers 503
// above 5 requests in flight, and checks: the 2- and 4-user steps are comfortable, the 8-user step broke and is
// named, the 16-user step never ran, the recorded limit is 4 users, and no request reached the server after the
// 8-user step's hold ended.
func endToEndHTTPRamp(t *testing.T, inner Runner) {
	e := newHTTPLoadEnv(t)
	httpLoadSleep = time.Sleep // the real settle (0 s here)
	srv := newBreakingServer(t, 5)
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-E2E", "2, 4, 8, 16",
		"- **Step Duration Seconds**: 10\n- **Ramp Seconds**: 0\n- **Settle Seconds**: 0\n- **Target P95 Ms**: 2000\n- **Max Error Rate**: 0.05\n")
	r := &stepClockRunner{inner: inner}
	res := e.run(t, httpLoadConfig(srv.URL, allowAPILab), r)
	time.Sleep(500 * time.Millisecond) // any straggler would arrive now

	for _, s := range res.LoadSteps {
		p95 := int64(-1)
		if s.ResponseUs != nil {
			p95 = s.ResponseUs.P95
		}
		t.Logf("step %d: %d users, %s, comfortable=%v, offered %.1f/s, served %.1f/s, served/started %.3f, p95 %d us, errors %v",
			s.Step, s.Sessions, s.Status, s.Comfortable, s.OfferedPerS, s.DeliveredPerS, s.DeliveredRatio, p95, s.Errors)
	}
	t.Logf("row: status=%s stopped_at_step=%d req ok/failed=%d/%d; server: %d requests, at most %d in flight; runner calls (users) %v",
		res.Status, res.LoadStoppedAtStep, res.ReqSuccess, res.ReqFailed, atomic.LoadInt64(&srv.total), atomic.LoadInt64(&srv.maxInflight), r.users)

	if strings.Join(r.users, ",") != "2,4,8" {
		t.Fatalf("steps run = %v, want 2,4,8: the ramp must stop at the step that broke and never send 16 users", r.users)
	}
	if len(res.LoadSteps) != 4 || res.LoadDriver != "http" {
		t.Fatalf("row = %+v", res)
	}
	if !res.LoadSteps[0].Comfortable || !res.LoadSteps[1].Comfortable {
		t.Errorf("the 2- and 4-user steps stay under the server's limit and must be comfortable")
	}
	if b := res.LoadSteps[2]; b.Status != report.LoadStepMeasured || b.Comfortable || b.Errors["status:503"] == 0 {
		t.Errorf("the 8-user step must be measured, not comfortable, with 503s: %+v", b)
	}
	if res.LoadSteps[3].Status != report.LoadStepNotRun || res.LoadStoppedAtStep != 3 {
		t.Errorf("step 4 = %s, stopped at %d; want not_run and 3", res.LoadSteps[3].Status, res.LoadStoppedAtStep)
	}
	// the recorded limit: the largest comfortable step whose every smaller step is comfortable (the control
	// plane's rule, internal/control capacityLimit)
	limit := 0
	for _, s := range res.LoadSteps {
		if !(s.Status == report.LoadStepMeasured && s.Comfortable && !s.GeneratorLimited) {
			break
		}
		limit = s.Sessions
	}
	if limit != 4 {
		t.Errorf("recorded limit = %d users, want 4", limit)
	}
	if res.Status != "passed" {
		t.Errorf("status = %s (%+v), want passed: a limit was measured", res.Status, res.Failure)
	}
	if n := srv.arrivedAfter(r.ends[2]); n != 0 {
		t.Errorf("%d request(s) reached the server after the breaking step's hold ended", n)
	}
	if m := atomic.LoadInt64(&srv.maxInflight); m > 8 {
		t.Errorf("the server saw %d requests in flight: more than the 8-user step can send", m)
	}
}

func TestHTTPLoad_EndToEnd_InProcessGenerator(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: a 30 s end-to-end ramp")
	}
	endToEndHTTPRamp(t, genRunner{})
}

func TestHTTPLoad_EndToEnd_RealJMeter(t *testing.T) {
	bin := jmeterBinForTest()
	if bin == "" {
		t.Skip("no JMeter: set ARGUS_TEST_JMETER or put jmeter on PATH")
	}
	tpl, _ := filepath.Abs("../../templates")
	endToEndHTTPRamp(t, &LocalJMeterRunner{TemplatesDir: tpl, JMeterBin: bin})
}

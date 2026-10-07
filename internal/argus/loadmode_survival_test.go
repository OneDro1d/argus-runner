package argus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// timedRunner writes a .jtl with samples at EXACT, CALLER-CHOSEN timeStamp values (seconds apart,
// not ms) — the survival-plane windowed-delta test needs the run window and the baseline window to
// land on DIFFERENT Prometheus instant-query seconds, which millisecond-apart samples would not
// reliably give.
type timedRunner struct {
	tsMs []int64 // one per sample, all 2xx
}

func (f *timedRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	id := props["scenario.id"]
	var b []byte
	b = append(b, "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"...)
	for i, ts := range f.tsMs {
		b = append(b, []byte(fmt.Sprintf("%d,10,%s,202,OK,%s %s 1-%d,true,\n", ts, id, templateBase, id, i+1))...)
	}
	return os.WriteFile(jtlPath, b, 0o644)
}

// fakePrometheusServer answers the instant-query API: throttleAtSec (unix seconds) reports a high
// value, every other `time=` reports 0 — simulating a pod that throttled DURING the run window but
// not during the baseline.
func fakePrometheusServer(t *testing.T, throttleAtSec int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		at, _ := strconv.ParseInt(r.URL.Query().Get("time"), 10, 64)
		val := "0"
		if at == throttleAtSec && strings.Contains(q, "throttl") {
			val = "12"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"result": []map[string]any{{"value": []any{at, val}}},
			},
		})
	}))
}

// AC-11 end to end: a load scenario that PASSES its own assertions, under a survival-plane read
// showing the SUT pod throttled during the run window, is reported DEGRADED — through RunAll,
// summarize and the Report.Summary tally, in one pass.
func TestRunAll_ThrottledPodDuringLoadRunYieldsDegraded(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-003", loadScenarioMD("LOAD-003", loadBodyGenerous))

	// Samples 4 seconds apart in wall-clock seconds: run window [T, T+4s], baseline [T-4s, T].
	const baseSec = int64(1780000000)
	tsMs := []int64{baseSec * 1000, (baseSec + 4) * 1000}
	fr := &timedRunner{tsMs: tsMs}

	srv := fakePrometheusServer(t, baseSec+4) // the RUN window's end — not the baseline's
	defer srv.Close()

	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	c.Deploy.ComposeProject = "orderservice"
	enabled := true
	c.Observability.Prometheus.Enabled = &enabled
	c.Observability.Prometheus.Endpoint = srv.URL

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("LOAD-003")
	if res == nil {
		t.Fatal("LOAD-003 not found")
	}
	if res.Status != "degraded" {
		t.Fatalf("status = %q, want degraded: %+v", res.Status, res)
	}
	if res.Load == nil || !res.Load.Degraded || res.Load.DegradedNote == "" {
		t.Fatalf("Load must carry Degraded=true + a note: %+v", res.Load)
	}
	if rr.Report.Summary.Degraded != 1 {
		t.Errorf("Summary.Degraded = %d, want 1", rr.Report.Summary.Degraded)
	}
	if rr.Report.Summary.Passed != 0 {
		t.Errorf("Summary.Passed = %d, want 0 (moved to Degraded)", rr.Report.Summary.Passed)
	}
	if rr.Report.Failed() {
		t.Errorf("Report.Failed() must be false — DEGRADED is not a failure")
	}
}

// The counterpart: when the survival plane shows NO distress (the run window's throttle delta ==
// the baseline's), the scenario stays `passed` — proves the read is not unconditionally pessimistic.
func TestRunAll_QuietPodDuringLoadRunStaysPassed(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-004", loadScenarioMD("LOAD-004", loadBodyGenerous))

	const baseSec = int64(1780000100)
	tsMs := []int64{baseSec * 1000, (baseSec + 4) * 1000}
	fr := &timedRunner{tsMs: tsMs}

	srv := fakePrometheusServer(t, -1) // never matches — every query answers 0
	defer srv.Close()

	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	c.Deploy.ComposeProject = "orderservice"
	enabled := true
	c.Observability.Prometheus.Enabled = &enabled
	c.Observability.Prometheus.Endpoint = srv.URL

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("LOAD-004")
	if res == nil {
		t.Fatal("LOAD-004 not found")
	}
	if res.Status != "passed" {
		t.Fatalf("status = %q, want passed (no distress signal): %+v", res.Status, res)
	}
	if res.Load != nil && res.Load.Degraded {
		t.Errorf("Load.Degraded must be false: %+v", res.Load)
	}
}

// AC-11 / ratelimit.go:143: a Rate Limiting scenario that DECLARES `status=429` (testing the
// limiter, not tripping it) carries no LOAD profile, so applySurvivalPlane's `res.Load == nil`
// guard makes it skip it even with the survival plane fully wired up and reachable — the existing
// "EXPECTS 429 => passed" verdict is untouched by AC-11's addition.
func TestRunAll_RateLimitingExpected429StaysPassedWithSurvivalPlaneWired(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenario(t, scDir, "rate-limiting", "RATE-001", "status=429")

	fr := &fakeRunner{pass: map[string]bool{}} // fakeRunner echoes expect.status on "pass"; RATE-001 wants 429
	// fakeRunner treats every id NOT in pass as a 400 — so mark it explicitly "passing" to get the
	// echoed expect.status (429) back, exactly like TestRunAll_ExpectedHTTP429IsAPassNotAThrottle.
	fr.pass["RATE-001"] = true

	srv := fakePrometheusServer(t, -1) // reachable, but must never even be asked (no Load profile)
	defer srv.Close()

	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	c.Deploy.ComposeProject = "orderservice"
	enabled := true
	c.Observability.Prometheus.Enabled = &enabled
	c.Observability.Prometheus.Endpoint = srv.URL

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("RATE-001")
	if res == nil || res.Status != "passed" {
		t.Fatalf("RATE-001 = %+v; a scenario that EXPECTS 429 must stay passed under AC-11", res)
	}
	if res.Load != nil {
		t.Errorf("a Rate Limiting scenario with no ## LOAD must carry nil Load, got %+v", res.Load)
	}
}

// Without PromEnabled the survival plane is never consulted — no network call, `passed` untouched.
// This is also the guard for the Rate Limiting layer's legitimate `passed` (ratelimit.go:143):
// a Rate Limiting scenario never declares a LOAD profile, so it is never even a candidate, and this
// proves the inert (opt-out) path independently of that fact.
func TestRunAll_PromDisabledNeverConsultsSurvivalPlane(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeScenarioMD(t, scDir, "http-ingestion", "LOAD-005", loadScenarioMD("LOAD-005", loadBodyGenerous))

	fr := &timedRunner{tsMs: []int64{1780000200000, 1780000204000}}
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
	// Prometheus NOT enabled, and no endpoint reachable — a live call here would fail/hang the test.

	rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := rr.Report.Find("LOAD-005")
	if res == nil || res.Status != "passed" {
		t.Fatalf("status = %+v, want passed", res)
	}
	if res.Load == nil || res.Load.SurvivalChecked {
		t.Errorf("SurvivalChecked must be false when Prometheus is not enabled: %+v", res.Load)
	}
}

package obsquery

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// r3: argus_sut_requests_total is RETIRED — the "Test requests sent" panel moved to the Loki
// request-event stream. PushMetrics must NO LONGER emit it (nor any request-count line), while
// the kept families (scenarios_total / scenario_duration_seconds with status / last_run_timestamp
// with run_id) remain intact, and NFR-3 (no correlation_id in the Prometheus exposition) holds.
func TestPushMetrics_RetiresSUTRequests(t *testing.T) {
	var bodies, urls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		urls = append(urls, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	rep := &report.Report{RunID: "20260101T000000", Layers: []report.Layer{{Layer: "permissions", Scenarios: []report.ScenarioResult{
		{ID: "PERM-001", Status: "passed", ReqFailed: 1}, // error-path: PASSES but its request returned a 4xx
		{ID: "ORD-001", Status: "passed", ReqSuccess: 2, DurationMs: 120},
		{ID: "DB-001", Status: "failed", ReqError: 1, DurationMs: 3400}, // distinct status for the duration-label test
	}}}}
	if err := PushMetrics(ts.URL, "local", "order-service", "local", rep, 0); err != nil {
		t.Fatalf("push: %v", err)
	}

	// FX-7 (R3-2/R3-3): PushMetrics now makes TWO pushes — the per-scenario metrics to a run_id-keyed
	// grouping key (so runs ACCUMULATE / are addressable per run), and the single-series
	// last_run_timestamp marker to the instance-only grouping key (REPLACED each run).
	const scenURL = "/metrics/job/argus/instance/local/run_id/20260101T000000"
	const instURL = "/metrics/job/argus/instance/local"
	var scenBody, markerBody string
	for i, u := range urls {
		switch u {
		case scenURL:
			scenBody = bodies[i]
		case instURL:
			markerBody = bodies[i]
		}
	}
	if scenBody == "" || markerBody == "" {
		t.Fatalf("expected two pushes (run_id-keyed scenario group + instance marker), got URLs %v", urls)
	}
	all := strings.Join(bodies, "\n")

	// RETIRED: neither the TYPE line nor any sample of argus_sut_requests_total may appear.
	if strings.Contains(all, "argus_sut_requests_total") {
		t.Errorf("argus_sut_requests_total must be RETIRED from the pushgateway exposition:\n%s", all)
	}
	// the single-series marker carries run_id and goes to the INSTANCE-only group (freshness relies on one series).
	if !strings.Contains(markerBody, `argus_last_run_timestamp{run_id="20260101T000000",argus_instance="local",project="order-service",cluster="local"}`) {
		t.Errorf("argus_last_run_timestamp must carry run_id on the instance-only group:\n%s", markerBody)
	}
	// the per-scenario metrics go to the RUN_ID-keyed group; the duration metric carries the scenario's status.
	if !strings.Contains(scenBody, `argus_scenario_duration_seconds{status="passed",layer="permissions",scenario_id="ORD-001",`) {
		t.Errorf("duration metric must carry status=passed on the run_id group:\n%s", scenBody)
	}
	if !strings.Contains(scenBody, `argus_scenario_duration_seconds{status="failed",layer="permissions",scenario_id="DB-001",`) {
		t.Errorf("duration metric must carry status=failed on the run_id group:\n%s", scenBody)
	}
	if !strings.Contains(scenBody, `argus_scenarios_total{status="passed",layer="permissions",scenario_id="ORD-001",`) {
		t.Errorf("argus_scenarios_total must remain on the run_id group:\n%s", scenBody)
	}
	// the scenario metrics must NOT be on the instance-only marker group (else they'd overwrite each run).
	if strings.Contains(markerBody, "argus_scenarios_total") {
		t.Errorf("per-scenario metrics must NOT ride the instance-only marker group:\n%s", markerBody)
	}
	// NFR-3: the Prometheus exposition must NEVER carry correlation_id (unbounded; lives in logs/chain only).
	if strings.Contains(all, "correlation_id") {
		t.Errorf("NFR-3 violation: correlation_id must not appear in the Prometheus exposition:\n%s", all)
	}
}

// TestPushMetrics_EmptyURLIsANoOp pins the T3.2 (--obs=adopt, no observability.pushgateway.url
// declared) "metrics push off" behaviour: k8srender renders an EXPLICIT `--pushgateway ""` rather
// than omitting the flag (which would fall through to the CLI's bundled-looking default
// http://localhost:9091 and push into a Pushgateway adopt never deploys). This test is what makes
// that choice SAFE — an empty url must make ZERO network calls and return no error, never a
// "connection refused" surfaced as a failed-push warning on every single run.
func TestPushMetrics_EmptyURLIsANoOp(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	rep := &report.Report{RunID: "20260101T000000", Layers: []report.Layer{{Layer: "permissions", Scenarios: []report.ScenarioResult{
		{ID: "PERM-001", Status: "passed"},
	}}}}
	if err := PushMetrics("", "local", "order-service", "local", rep, 0); err != nil {
		t.Fatalf("PushMetrics with an empty pushgatewayURL must be a silent no-op, got error: %v", err)
	}
	if called {
		t.Fatal("PushMetrics with an empty pushgatewayURL must make NO network call at all")
	}
}

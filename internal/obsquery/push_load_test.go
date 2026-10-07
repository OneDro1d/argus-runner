package obsquery

// push_load_test.go -- PR-E: an AMQP Load run's per-step numbers reach the Pushgateway
// as argus_load_step_* families, in the run's own group, with no address, user or password in any series.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

func loadPushReport() *report.Report {
	zero := 0
	return &report.Report{RunID: "20261002T100000", Layers: []report.Layer{
		{Layer: "AMQL", Scenarios: []report.ScenarioResult{{
			ID: "AMQL-001", Status: "passed", DurationMs: 70000, LoadDriver: "amqp", LoadTarget: "lab-broker",
			LoadSteps: []report.LoadStep{
				{Step: 1, Sessions: 10, Status: report.LoadStepMeasured, WindowSeconds: 30, OfferedPerS: 100, SentPerS: 100, ConfirmedPerS: 100,
					DeliveredPerS: 100, DeliveredRatio: 1,
					PublishConfirmUs: &report.Quantiles{Min: 300, P50: 800, P75: 1000, P95: 2000, P99: 3000, Max: 9000},
					PublishDeliverUs: &report.Quantiles{Min: 400, P50: 900, P75: 1200, P95: 2500, P99: 4000, Max: 12000},
					RestartsDelta:    &zero, Comfortable: true},
				{Step: 2, Sessions: 20, Status: report.LoadStepBlocked, WindowSeconds: 30, OfferedPerS: 200, SentPerS: 150, DeliveredPerS: 140,
					DeliveredRatio: 0.93, Errors: map[string]int{"blocked": 12, "confirm_timeout": 3},
					Blocked: []report.BlockedPeriod{{SinceMs: 1000, UntilMs: 4000, Reason: "memory"}}, BlockedSeconds: 3},
				{Step: 3, Sessions: 40, Status: report.LoadStepNotRun},
			},
		}}},
		{Layer: "http", Scenarios: []report.ScenarioResult{{ID: "HTTP-001", Status: "passed", DurationMs: 10}}},
	}}
}

func capturePush(t *testing.T, rep *report.Report) (runGroup string, urls []string) {
	t.Helper()
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		urls = append(urls, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := PushMetrics(ts.URL, "lab", "example", "example", rep, 0); err != nil {
		t.Fatalf("push: %v", err)
	}
	for i, u := range urls {
		if u == "/metrics/job/argus/instance/lab/run_id/"+rep.RunID {
			runGroup = bodies[i]
		}
	}
	if runGroup == "" {
		t.Fatalf("no push to the run's own group; got %v", urls)
	}
	return runGroup, urls
}

func TestPushMetrics_LoadStepFamilies(t *testing.T) {
	body, _ := capturePush(t, loadPushReport())
	t.Logf("PUSHGATEWAY BODY (run group):\n%s", body)
	const l = `argus_instance="lab",project="example",cluster="example"`
	want := []string{
		`argus_load_step_sessions{scenario_id="AMQL-001",step="1",` + l + `} 10`,
		`argus_load_step_sessions{scenario_id="AMQL-001",step="2",` + l + `} 20`,
		`argus_load_step_sent_per_second{scenario_id="AMQL-001",step="1",` + l + `} 100`,
		`argus_load_step_sent_per_second{scenario_id="AMQL-001",step="2",` + l + `} 150`,
		`argus_load_step_confirmed_per_second{scenario_id="AMQL-001",step="1",` + l + `} 100`,
		`argus_load_step_delivered_per_second{scenario_id="AMQL-001",step="1",` + l + `} 100`,
		`argus_load_step_delivered_per_second{scenario_id="AMQL-001",step="2",` + l + `} 140`,
		`argus_load_step_publish_confirm_seconds{scenario_id="AMQL-001",step="1",quantile="0.5",` + l + `} 0.0008`,
		`argus_load_step_publish_confirm_seconds{scenario_id="AMQL-001",step="1",quantile="0.75",` + l + `} 0.001`,
		`argus_load_step_publish_confirm_seconds{scenario_id="AMQL-001",step="1",quantile="0.95",` + l + `} 0.002`,
		`argus_load_step_publish_confirm_seconds{scenario_id="AMQL-001",step="1",quantile="0.99",` + l + `} 0.003`,
		`argus_load_step_publish_deliver_seconds{scenario_id="AMQL-001",step="1",quantile="0.5",` + l + `} 0.0009`,
		`argus_load_step_publish_deliver_seconds{scenario_id="AMQL-001",step="1",quantile="0.75",` + l + `} 0.0012`,
		`argus_load_step_publish_deliver_seconds{scenario_id="AMQL-001",step="1",quantile="0.95",` + l + `} 0.0025`,
		`argus_load_step_publish_deliver_seconds{scenario_id="AMQL-001",step="1",quantile="0.99",` + l + `} 0.004`,
		`argus_load_step_errors{scenario_id="AMQL-001",step="2",code="blocked",` + l + `} 12`,
		`argus_load_step_errors{scenario_id="AMQL-001",step="2",code="confirm_timeout",` + l + `} 3`,
		`argus_load_step_blocked_seconds{scenario_id="AMQL-001",step="1",` + l + `} 0`,
		`argus_load_step_blocked_seconds{scenario_id="AMQL-001",step="2",` + l + `} 3`,
		`argus_load_step_comfortable{scenario_id="AMQL-001",step="1",` + l + `} 1`,
		`argus_load_step_comfortable{scenario_id="AMQL-001",step="2",` + l + `} 0`,
		`argus_load_step_restarts_delta{scenario_id="AMQL-001",step="1",` + l + `} 0`,
	}
	for _, w := range want {
		if !strings.Contains(body, w+"\n") {
			t.Errorf("missing series line:\n  %s", w)
		}
	}
	// absent on purpose: a not_run step; confirm numbers when no confirm quantiles; a restart count never read
	for _, bad := range []string{`step="3"`, `argus_load_step_confirmed_per_second{scenario_id="AMQL-001",step="2"`,
		`argus_load_step_publish_confirm_seconds{scenario_id="AMQL-001",step="2"`, `argus_load_step_restarts_delta{scenario_id="AMQL-001",step="2"`} {
		if strings.Contains(body, bad) {
			t.Errorf("series that must be absent is present: %s", bad)
		}
	}
}

// pushgateway rejects a body whose family is split: every sample sits under its own # TYPE, once.
func TestPushMetrics_LoadStepFamiliesAreGrouped(t *testing.T) {
	body, _ := capturePush(t, loadPushReport())
	current := ""
	seen := map[string]bool{}
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "# TYPE ") {
			name := strings.Fields(ln)[2]
			if seen[name] {
				t.Errorf("family %s declared twice", name)
			}
			seen[name] = true
			current = name
			continue
		}
		if ln == "" {
			continue
		}
		name := ln[:strings.IndexAny(ln, "{ ")]
		if name != current {
			t.Errorf("sample %q sits under family %q", ln, current)
		}
	}
	for _, f := range []string{"argus_load_step_sessions", "argus_load_step_errors", "argus_load_step_comfortable"} {
		if !seen[f] {
			t.Errorf("family %s was never declared", f)
		}
	}
}

func TestPushMetrics_NoLoadStepsNoLoadFamilies(t *testing.T) {
	rep := loadPushReport()
	rep.Layers = rep.Layers[1:]
	body, _ := capturePush(t, rep)
	if strings.Contains(body, "argus_load_step_") {
		t.Fatalf("a run with no load scenario pushed load families:\n%s", body)
	}
}

// Custody: no URL, host, user or password in a series, whatever the target is called or the scenario says.
func TestPushMetrics_LoadSeriesCarryNoAddressOrCredential(t *testing.T) {
	rep := loadPushReport()
	rep.Layers[0].Scenarios[0].LoadTarget = "amqps://argus:hunter2@broker.internal:5671/vh"
	body, urls := capturePush(t, rep)
	for _, bad := range []string{"amqp", "hunter2", "argus:", "@", "broker.internal", "5671", "://", "vh", "lab-broker"} {
		if strings.Contains(body, bad) {
			t.Errorf("the pushed body contains %q", bad)
		}
	}
	for _, u := range urls {
		if strings.Contains(u, "hunter2") || strings.Contains(u, "broker") {
			t.Errorf("the push URL path carries target data: %s", u)
		}
	}
}

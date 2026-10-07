package httpload

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpload"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Promise 2: each step records the AMQP ramp's per-step facts: offered and achieved rate, latency
// percentiles, errors and their reasons, and the step's verdict.

func row(ts int64, el int, code, msg string) amqpload.Row {
	return amqpload.Row{TimeStampMs: ts, ElapsedMs: el, Label: "HL-1", Code: code, Message: msg}
}

// 10 s of ramp (excluded), then 20 s: 100 requests at 200 (10 ms .. 100 ms), 6 at 503, 4 refused.
func stepFixture() []amqpload.Row {
	t0 := int64(1_000_000)
	rs := []amqpload.Row{row(t0, 5, "200", "OK"), row(t0+5000, 5, "500", "ramp error, not counted")}
	for i := 0; i < 100; i++ {
		rs = append(rs, row(t0+10_000+int64(i)*200, 10+i, "200", "OK"))
	}
	for i := 0; i < 6; i++ {
		rs = append(rs, row(t0+12_000+int64(i)*1000, 3, "503", "Service Unavailable"))
	}
	for i := 0; i < 4; i++ {
		rs = append(rs, row(t0+15_000+int64(i)*1000, 1, "Non HTTP response code: org.apache.http.conn.HttpHostConnectException",
			"Non HTTP response message: Connect to sut.invalid:8080 [sut.invalid/10.0.0.9] failed: Connection refused"))
	}
	rs = append(rs, row(t0+30_000, 20, "200", "OK"))
	rs = append(rs, amqpload.Row{TimeStampMs: t0 + 20_000, ElapsedMs: 1, Label: "OTHER", Code: "500"}) // another sampler: ignored
	return rs
}

func TestAggregateStep_RecordsRatesLatencyErrorsAndReasons(t *testing.T) {
	st := AggregateStep(stepFixture(), StepInput{Step: 2, Users: 20, RampSeconds: 10, ScenarioID: "HL-1", TargetP95Ms: 1000, MaxErrorRate: 0.5})
	if st.Step != 2 || st.Sessions != 20 || st.Status != report.LoadStepMeasured {
		t.Fatalf("head = %+v", st)
	}
	if st.WindowSeconds != 20 {
		t.Errorf("window = %v, want 20 (first row + 10 s ramp .. last row)", st.WindowSeconds)
	}
	// 111 requests in the window: 101 served, 6 answered 503, 4 got no status
	if st.OfferedPerS != 111.0/20 || st.SentPerS != 107.0/20 || st.DeliveredPerS != 101.0/20 {
		t.Errorf("rates offered/sent/delivered = %v/%v/%v", st.OfferedPerS, st.SentPerS, st.DeliveredPerS)
	}
	if st.DeliveredRatio != 101.0/111 {
		t.Errorf("served/started = %v", st.DeliveredRatio)
	}
	if st.Errors["status:503"] != 6 || st.Errors["transport"] != 4 || len(st.Errors) != 2 {
		t.Errorf("errors = %v", st.Errors)
	}
	if len(st.ErrorReasons) != 1 || st.ErrorReasons[0].Count != 4 || !strings.Contains(st.ErrorReasons[0].Reason, "Connection refused") {
		t.Errorf("error reasons = %+v", st.ErrorReasons)
	}
	if st.ResponseUs == nil || st.ResponseUs.Min != 1000 || st.ResponseUs.Max != 109_000 || st.PublishDeliverUs != nil {
		t.Errorf("response quantiles = %+v (deliver %+v)", st.ResponseUs, st.PublishDeliverUs)
	}
	if !st.Comfortable {
		t.Error("p95 under 1000 ms and 9% errors under 50% must be comfortable")
	}
	// the same step against a 1% error budget is not comfortable; against a 50 ms p95 neither
	if AggregateStep(stepFixture(), StepInput{Users: 20, RampSeconds: 10, ScenarioID: "HL-1", TargetP95Ms: 1000, MaxErrorRate: 0.01}).Comfortable {
		t.Error("9% errors passed a 1% budget")
	}
	if AggregateStep(stepFixture(), StepInput{Users: 20, RampSeconds: 10, ScenarioID: "HL-1", TargetP95Ms: 50, MaxErrorRate: 0.5}).Comfortable {
		t.Error("a ~100 ms p95 passed a 50 ms target")
	}
}

func TestAggregateStep_ATimeoutIsItsOwnClass_AndNoRowIsNotMeasured(t *testing.T) {
	rs := []amqpload.Row{row(1000, 1, "200", "OK"), row(2000, 10000, "Non HTTP response code: java.net.SocketTimeoutException", "Non HTTP response message: Read timed out"), row(3000, 1, "200", "OK")}
	st := AggregateStep(rs, StepInput{Users: 1, ScenarioID: "HL-1", TargetP95Ms: 100000, MaxErrorRate: 1})
	if st.Errors["timeout"] != 1 {
		t.Errorf("errors = %v, want one timeout", st.Errors)
	}
	if e := AggregateStep(nil, StepInput{Step: 1, Users: 5, ScenarioID: "HL-1", TargetP95Ms: 1, MaxErrorRate: 1}); e.Status != report.LoadStepSetupFailed || e.Comfortable {
		t.Errorf("a step with no request = %+v, want setup_failed and not comfortable", e)
	}
}

func steps(comfortable ...bool) []report.LoadStep {
	var out []report.LoadStep
	for i, c := range comfortable {
		out = append(out, report.LoadStep{Step: i + 1, Sessions: 10 << i, Status: report.LoadStepMeasured, Comfortable: c,
			ResponseUs: &report.Quantiles{P95: 2000}, DeliveredRatio: 1})
	}
	return out
}

func TestVerdict(t *testing.T) {
	p := &scenario.HTTPLoadProfile{Steps: []int{10, 20, 40}}
	// broke at step 3: a measured limit, not a failure
	s := steps(true, true, false)
	if o := Verdict(p, s, 3); o.Status != "passed" {
		t.Errorf("ramp that broke above its first step = %+v, want passed", o)
	}
	// broke at step 1: the smallest step is not comfortable
	s = steps(false)
	s = append(s, report.LoadStep{Step: 2, Sessions: 20, Status: report.LoadStepNotRun})
	if o := Verdict(p, s, 1); o.Status != "failed" || !strings.Contains(o.Observed, "even the smallest (10 users)") {
		t.Errorf("= %+v", o)
	}
	// Must Sustain 40 never reached: the ramp stopped at step 2
	s = []report.LoadStep{steps(true)[0], steps(true, false)[1], {Step: 3, Sessions: 40, Status: report.LoadStepNotRun}}
	o := Verdict(&scenario.HTTPLoadProfile{Steps: []int{10, 20, 40}, MustSustain: 40}, s, 2)
	if o.Status != "failed" || !strings.Contains(o.Observed, "the 40-user step was never reached: the ramp stopped at step 2 (20 users)") {
		t.Errorf("= %+v", o)
	}
	// a step that recorded nothing
	s = []report.LoadStep{steps(true)[0], {Step: 2, Sessions: 20, Status: report.LoadStepSetupFailed}}
	if o := Verdict(p, s, 0); o.Status != "failed" || !strings.Contains(o.Observed, "no request was recorded at step 2 of 2 (20 users)") {
		t.Errorf("= %+v", o)
	}
	// restarts with every assertion held
	s = steps(true, true)
	three := 3
	s[1].RestartsDelta = &three
	if o := Verdict(p, s, 0); o.Status != report.StatusDegraded {
		t.Errorf("= %+v", o)
	}
	// the observed text never carries a threshold
	s = steps(false)
	if o := Verdict(&scenario.HTTPLoadProfile{Steps: []int{10}, TargetP95Ms: 777}, s, 1); strings.Contains(o.Observed, "777") {
		t.Errorf("observed carries the declared threshold: %s", o.Observed)
	}
}

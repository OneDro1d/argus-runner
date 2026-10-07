package amqpload

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

const t0 = int64(1_000_000)

func pub(ts int64, us int, extra string) Row {
	return Row{TimeStampMs: ts, ElapsedMs: us / 1000, Label: "X-amqp-publish", Code: "200", Success: true,
		Message: fmt.Sprintf("published 1000B confirm=each us=%d%s", us, extra)}
}

func del(ts int64, us int) Row {
	return Row{TimeStampMs: ts, ElapsedMs: us / 1000, Label: "X-amqp-deliver", Code: "200", Success: true,
		Message: fmt.Sprintf("delivered 1000B us=%d redelivered=false", us)}
}

func fail(label, code, msg string, ts int64) Row {
	return Row{TimeStampMs: ts, Label: label, Code: code, Message: msg, Success: false}
}

func in() StepInput {
	return StepInput{Step: 1, Sessions: 2, RampSeconds: 10, RatePerSession: 1, Confirm: "each", ScenarioID: "X",
		TargetP95Ms: 250, MaxErrorRate: 0.01, MinDeliveredRatio: 0.95}
}

// fixture: 2 sessions x 10 seconds of steady state, plus a ramp that carries absurd values which must
// never reach a statistic. Hand-computed: see TestAggregate_HandComputed.
func steadyRows() []Row {
	var rows []Row
	// the ramp (ts < t0+10000): setup + 4 rows with 9,999,999us
	rows = append(rows, Row{TimeStampMs: t0, Label: "X-amqp-setup", Code: "200", Success: true, Message: "session ready"})
	for i := 0; i < 2; i++ {
		rows = append(rows, pub(t0+int64(1000*i), 9_999_999, ""), del(t0+int64(1000*i), 9_999_999))
	}
	// the steady window: 10 timestamps t0+10500 .. t0+19500, two sessions each
	for k := 1; k <= 10; k++ {
		ts := t0 + 10000 + int64(k)*1000 - 500
		for s := 0; s < 2; s++ {
			rows = append(rows, pub(ts, k*1000, ""), del(ts, k*3000))
		}
	}
	return rows
}

func TestAggregate_HandComputed_WindowRatesAndQuantiles(t *testing.T) {
	st := AggregateStep(steadyRows(), in())
	// window = [t0+10000, maxTs = t0+19500] = 9.5 s; 20 publish and 20 deliver rows inside it.
	if st.WindowSeconds != 9.5 {
		t.Errorf("window = %v, want 9.5", st.WindowSeconds)
	}
	want := 20 / 9.5
	for name, got := range map[string]float64{"offered": st.OfferedPerS, "sent": st.SentPerS, "confirmed": st.ConfirmedPerS, "delivered": st.DeliveredPerS} {
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s_per_s = %v, want %v", name, got, want)
		}
	}
	if st.DeliveredRatio != 1 {
		t.Errorf("delivered_ratio = %v", st.DeliveredRatio)
	}
	// publish us sorted: 1000,1000,2000,2000,...,10000,10000 (N=20): p50=10th=5000 p75=15th=8000 p95=19th=10000 p99=20th=10000
	pc := st.PublishConfirmUs
	if pc == nil || *pc != (report.Quantiles{Min: 1000, P50: 5000, P75: 8000, P95: 10000, P99: 10000, Max: 10000}) {
		t.Errorf("publish_confirm_us = %+v", pc)
	}
	pd := st.PublishDeliverUs
	if pd == nil || *pd != (report.Quantiles{Min: 3000, P50: 15000, P75: 24000, P95: 30000, P99: 30000, Max: 30000}) {
		t.Errorf("publish_deliver_us = %+v (the 9,999,999us ramp rows must be excluded)", pd)
	}
	if st.Status != report.LoadStepMeasured || !st.Comfortable || st.GeneratorLimited || len(st.Blocked) != 0 || len(st.Errors) != 0 {
		t.Errorf("record = %+v", st)
	}
}

func TestAggregate_ConfirmOffHasNoConfirmQuantiles(t *testing.T) {
	i := in()
	i.Confirm = "off"
	st := AggregateStep(steadyRows(), i)
	if st.PublishConfirmUs != nil || st.ConfirmedPerS != 0 {
		t.Errorf("confirm=off must report no confirm figures: %+v / %v", st.PublishConfirmUs, st.ConfirmedPerS)
	}
	if st.PublishDeliverUs == nil {
		t.Error("publish->deliver is measured whatever the confirm mode")
	}
}

func TestAggregate_FallsBackToElapsedWhenNoUsToken(t *testing.T) {
	rows := []Row{{TimeStampMs: t0, Label: "X-amqp-setup", Code: "200", Success: true},
		{TimeStampMs: t0 + 11000, ElapsedMs: 7, Label: "X-amqp-deliver", Code: "200", Success: true, Message: "delivered 10B"},
		{TimeStampMs: t0 + 12000, ElapsedMs: 9, Label: "X-amqp-publish", Code: "200", Success: true, Message: "published 10B"}}
	st := AggregateStep(rows, in())
	if st.PublishDeliverUs == nil || st.PublishDeliverUs.P50 != 7000 {
		t.Errorf("elapsed*1000 fallback missing: %+v", st.PublishDeliverUs)
	}
}

func TestAggregate_ErrorsByClosedClassSet(t *testing.T) {
	rows := steadyRows()
	ts := t0 + 15000
	rows = append(rows,
		fail("X-amqp-publish", "503", "blocked by broker: memory alarm (since=1014000)", ts),
		fail("X-amqp-publish", "504", "not confirmed within 5000ms", ts),
		fail("X-amqp-publish", "502", "nacked by broker", ts),
		fail("X-amqp-publish", "502", "nacked by broker", ts),
		fail("X-amqp-deliver", "408", "no delivery within 5000ms", ts),
		fail("X-amqp-publish", "404", "NOT_FOUND", ts),
		fail("X-amqp-publish", "500", "boom", ts),
		fail("X-amqp-setup", "500", "connect refused", ts),
	)
	st := AggregateStep(rows, in())
	want := map[string]int{"blocked": 1, "confirm_timeout": 1, "nack": 2, "deliver_timeout": 1, "channel:404": 1, "other": 1, "setup": 1}
	for k, v := range want {
		if st.Errors[k] != v {
			t.Errorf("errors[%s] = %d, want %d (all: %v)", k, st.Errors[k], v, st.Errors)
		}
	}
	if len(st.Errors) != len(want) {
		t.Errorf("unexpected classes: %v", st.Errors)
	}
}

func TestAggregate_BlockedPeriodsFromSinceAndWasBlocked(t *testing.T) {
	rows := steadyRows()
	rows = append(rows,
		fail("X-amqp-publish", "503", "blocked by broker: memory alarm (since=1013000)", t0+13100),
		fail("X-amqp-publish", "503", "blocked by broker: memory alarm (since=1013000)", t0+13600),
		pub(t0+14500, 800, " (was blocked 1500ms)"),
	)
	st := AggregateStep(rows, in())
	if st.Status != report.LoadStepBlocked {
		t.Fatalf("status = %q", st.Status)
	}
	if len(st.Blocked) != 1 || st.Blocked[0] != (report.BlockedPeriod{SinceMs: 1013000, UntilMs: t0 + 14500, Reason: "memory alarm"}) {
		t.Fatalf("blocked = %+v", st.Blocked)
	}
	if st.BlockedSeconds != 1.5 {
		t.Errorf("blocked_seconds = %v", st.BlockedSeconds)
	}
	if st.Comfortable {
		t.Error("a blocked step is never comfortable")
	}
}

func TestAggregate_StillBlockedAtTheEndHasNoUntil(t *testing.T) {
	rows := []Row{{TimeStampMs: t0, Label: "X-amqp-setup", Code: "200", Success: true},
		fail("X-amqp-publish", "503", "blocked by broker: disk alarm (since=1005000)", t0+11000),
		fail("X-amqp-publish", "503", "blocked by broker: disk alarm (since=1005000)", t0+16000)}
	st := AggregateStep(rows, in())
	if len(st.Blocked) != 1 || st.Blocked[0].UntilMs != 0 || st.Blocked[0].Reason != "disk alarm" {
		t.Fatalf("blocked = %+v", st.Blocked)
	}
	if st.BlockedSeconds != 11 { // 1016000 - 1005000
		t.Errorf("blocked_seconds = %v", st.BlockedSeconds)
	}
}

func TestAggregate_GeneratorLimitedIsNeverComfortable(t *testing.T) {
	i := in()
	i.Sessions = 100 // expects 100 msg/s; the fixture offers 20 in 9.5 s
	st := AggregateStep(steadyRows(), i)
	if !st.GeneratorLimited || st.Comfortable {
		t.Errorf("offered %.2f/s against 100/s must flag the generator, got limited=%v comfortable=%v", st.OfferedPerS, st.GeneratorLimited, st.Comfortable)
	}
}

// item 4: a broker block stalls the publishers, so the offered rate collapses BECAUSE of the
// block. That shortfall is the broker's, not the generator's: a blocked step must not be flagged generator-limited.
func TestAggregate_BlockedStepIsNotGeneratorLimited(t *testing.T) {
	rows := []Row{{TimeStampMs: t0, Label: "X-amqp-setup", Code: "200", Success: true, Message: "session ready"}}
	// two seconds of normal offering (2 sessions x 1/s), then the broker blocks the publishers until the end
	for k := 0; k < 2; k++ {
		ts := t0 + 10000 + int64(k)*1000 + 500
		rows = append(rows, pub(ts, 1000, ""), pub(ts, 1000, ""), del(ts, 2000), del(ts, 2000))
	}
	rows = append(rows, fail("X-amqp-publish", "503", "blocked by broker: memory alarm (since=1012500)", t0+19500))
	st := AggregateStep(rows, in())
	if st.Status != report.LoadStepBlocked {
		t.Fatalf("fixture must be a blocked step, status = %q", st.Status)
	}
	if st.OfferedPerS >= 0.9*2 {
		t.Fatalf("fixture must offer below 90%% of the expected 2/s, offered %.2f/s", st.OfferedPerS)
	}
	if st.GeneratorLimited {
		t.Errorf("a step the broker blocked must not be marked generator_limited (offered %.2f/s)", st.OfferedPerS)
	}
}

func TestAggregate_SetupFailureStatus(t *testing.T) {
	rows := []Row{fail("X-amqp-setup", "500", "connect refused", t0)}
	if st := AggregateStep(rows, in()); st.Status != report.LoadStepSetupFailed || st.Comfortable {
		t.Errorf("record = %+v", st)
	}
}

func TestAggregate_ComfortRulesEachFailOnTheirOwn(t *testing.T) {
	base := steadyRows()
	slow := in()
	slow.TargetP95Ms = 20 // p95 deliver is 30ms
	if AggregateStep(base, slow).Comfortable {
		t.Error("p95 above the target must not be comfortable")
	}
	lossy := append(append([]Row{}, base...), fail("X-amqp-publish", "502", "nacked by broker", t0+15000))
	strict := in()
	strict.MaxErrorRate = 0.001 // 1 failure in 41 rows
	if AggregateStep(lossy, strict).Comfortable {
		t.Error("an error rate above the maximum must not be comfortable")
	}
	var noDeliver []Row
	for _, r := range base {
		if !strings.HasSuffix(r.Label, "-deliver") {
			noDeliver = append(noDeliver, r)
		}
	}
	if AggregateStep(noDeliver, in()).Comfortable {
		t.Error("delivered/sent below the minimum must not be comfortable")
	}
}

// The percentile method is the repo's own (nearest rank) -- no new one to defend (D-6).
func TestQuantiles_MatchTheRepoNearestRank(t *testing.T) {
	var ms []int
	var rows []Row
	for k := 1; k <= 37; k++ {
		v := (k * 7919) % 1000
		ms = append(ms, v)
		rows = append(rows, del(t0+11000+int64(k), v*1000))
	}
	rows = append(rows, Row{TimeStampMs: t0, Label: "X-amqp-setup", Code: "200", Success: true})
	got := AggregateStep(rows, in()).PublishDeliverUs
	ls := report.ComputeLoadStats(ms, 0)
	if got == nil || got.P50 != int64(ls.P50Ms)*1000 || got.P95 != int64(ls.P95Ms)*1000 || got.P99 != int64(ls.P99Ms)*1000 {
		t.Errorf("quantiles %+v differ from report.ComputeLoadStats %+v", got, ls)
	}
}

func TestReadJTL_ReadsByHeaderNameNotPosition(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jtl")
	body := "label,success,responseCode,timeStamp,elapsed,responseMessage,failureMessage,threadName\n" +
		"X-amqp-publish,true,200,1000500,12,\"published 10B confirm=each us=12345\",,T 1-1\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadJTL(p)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	r := rows[0]
	if r.TimeStampMs != 1000500 || r.ElapsedMs != 12 || r.Label != "X-amqp-publish" || r.Code != "200" || !r.Success || !strings.Contains(r.Message, "us=12345") {
		t.Errorf("row = %+v", r)
	}
}

func prof() *scenario.AMQPLoadProfile {
	return &scenario.AMQPLoadProfile{Steps: []int{10, 100, 1000}, MustSustain: 0}
}

func step(n, sessions int, status string, comfortable bool) report.LoadStep {
	return report.LoadStep{Step: n, Sessions: sessions, Status: status, Comfortable: comfortable}
}

func TestVerdict_CrossingTheLimitAtTheTopIsAMeasuredLimitNotAFailure(t *testing.T) {
	o := Verdict(prof(), []report.LoadStep{step(1, 10, "measured", true), step(2, 100, "measured", true), step(3, 1000, "measured", false)})
	if o.Status != "passed" {
		t.Fatalf("status = %q (%s)", o.Status, o.Observed)
	}
}

func TestVerdict_ABlockedStepFailsAndNamesTheBrokerReason(t *testing.T) {
	blocked := step(2, 100, "blocked", false)
	blocked.Blocked = []report.BlockedPeriod{{SinceMs: 1, Reason: "memory alarm"}}
	o := Verdict(prof(), []report.LoadStep{step(1, 10, "measured", true), blocked, step(3, 1000, "not_run", false)})
	if o.Status != "failed" {
		t.Fatalf("status = %q", o.Status)
	}
	for _, want := range []string{"blocked by broker", "memory alarm", "100"} {
		if !strings.Contains(o.Observed, want) {
			t.Errorf("observed %q does not name %q", o.Observed, want)
		}
	}
}

func TestVerdict_SetupFailureFails(t *testing.T) {
	o := Verdict(prof(), []report.LoadStep{step(1, 10, "setup_failed", false), step(2, 100, "not_run", false)})
	if o.Status != "failed" || !strings.Contains(o.Observed, "set up") {
		t.Fatalf("%+v", o)
	}
}

func TestVerdict_NoComfortableStepFails(t *testing.T) {
	o := Verdict(prof(), []report.LoadStep{step(1, 10, "measured", false), step(2, 100, "measured", false), step(3, 1000, "measured", false)})
	if o.Status != "failed" {
		t.Fatalf("status = %q", o.Status)
	}
}

func TestVerdict_MustSustainStepNotComfortableFails(t *testing.T) {
	p := prof()
	p.MustSustain = 100
	o := Verdict(p, []report.LoadStep{step(1, 10, "measured", true), step(2, 100, "measured", false), step(3, 1000, "measured", false)})
	if o.Status != "failed" {
		t.Fatalf("status = %q", o.Status)
	}
}

func TestVerdict_BrokerRestartsDegradeAnOtherwisePassingRamp(t *testing.T) {
	n := 2
	s := step(2, 100, "measured", true)
	s.RestartsDelta = &n
	o := Verdict(prof(), []report.LoadStep{step(1, 10, "measured", true), s})
	if o.Status != "degraded" {
		t.Fatalf("status = %q", o.Status)
	}
}

// VR-C8: the observed line is reality only. The declared thresholds live in the scenario.
func TestVerdict_ObservedNeverEchoesADeclaredThreshold(t *testing.T) {
	p := prof()
	p.TargetP95Ms, p.MaxErrorRate, p.MinDeliveredRatio, p.MustSustain = 250, 0.01, 0.95, 100
	for _, steps := range [][]report.LoadStep{
		{step(1, 10, "measured", false), step(2, 100, "measured", false)},
		{step(1, 10, "blocked", false)},
	} {
		o := Verdict(p, steps)
		for _, leak := range []string{"250", "0.01", "0.95", "target", "Target", "Must Sustain"} {
			if strings.Contains(o.Observed, leak) {
				t.Errorf("observed leaks %q: %s", leak, o.Observed)
			}
		}
	}
}

// The frozen shape PR-E consumes. A golden: any rename or re-type fails here first.
func TestLoadStep_JSONShapeIsFrozen(t *testing.T) {
	st := AggregateStep(steadyRows(), in())
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "step_record.golden.json")
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("golden missing: %v\n%s", err, b)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(b)) {
		t.Errorf("step record JSON changed.\n got:\n%s\nwant:\n%s", b, want)
	}
}

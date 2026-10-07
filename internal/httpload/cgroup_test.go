package httpload

import (
	"errors"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// #615 / the executor's own cgroup CPU throttling decides generator_limited.

const v2Stat = "usage_usec 123\nuser_usec 100\nsystem_usec 23\nnr_periods 400\nnr_throttled 300\nthrottled_usec 99\n"

func TestParseCPUStat_V2AndV1BodiesAndGarbage(t *testing.T) {
	st, ok := ParseCPUStat(v2Stat)
	if !ok || st.Periods != 400 || st.Throttled != 300 {
		t.Fatalf("v2 body: %+v ok=%v", st, ok)
	}
	if st, ok := ParseCPUStat("nr_periods 7\nnr_throttled 2\nthrottled_time 5\n"); !ok || st.Periods != 7 || st.Throttled != 2 {
		t.Errorf("v1 body: %+v ok=%v", st, ok)
	}
	for _, bad := range []string{"", "usage_usec 5\n", "nr_periods 7\n", "nr_periods x\nnr_throttled 2\n", "nr_periods -1\nnr_throttled 0\n"} {
		if _, ok := ParseCPUStat(bad); ok {
			t.Errorf("%q parsed as a reading", bad)
		}
	}
}

func TestReadCPUStatFrom_FirstPathWithBothCountersWins_UnreadableIsNotOK(t *testing.T) {
	fs := map[string]string{
		"/a": "usage_usec 5\n", // a v2 root with no cpu controller: no counters, try the next
		"/b": "nr_periods 10\nnr_throttled 4\n",
	}
	read := func(p string) ([]byte, error) {
		if s, ok := fs[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
	if st, ok := ReadCPUStatFrom([]string{"/missing", "/a", "/b"}, read); !ok || st.Periods != 10 || st.Throttled != 4 {
		t.Errorf("got %+v ok=%v", st, ok)
	}
	if st, ok := ReadCPUStatFrom([]string{"/missing", "/a"}, read); ok || st != (CPUStat{}) {
		t.Errorf("nothing readable must be not-ok and empty, got %+v ok=%v", st, ok)
	}
}

func TestThrottledShare_DeltaOfCounters_NeverInvented(t *testing.T) {
	if v, ok := ThrottledShare(CPUStat{100, 10}, CPUStat{300, 110}, true, true); !ok || v != 0.5 {
		t.Errorf("share = %v ok=%v, want 0.5 over the delta", v, ok)
	}
	cases := map[string]struct {
		b, a   CPUStat
		hb, ha bool
	}{
		"before unreadable":   {CPUStat{}, CPUStat{10, 5}, false, true},
		"after unreadable":    {CPUStat{10, 5}, CPUStat{}, true, false},
		"no period elapsed":   {CPUStat{10, 5}, CPUStat{10, 5}, true, true},
		"counter went back":   {CPUStat{100, 50}, CPUStat{20, 5}, true, true},
		"throttled went back": {CPUStat{100, 50}, CPUStat{200, 40}, true, true},
	}
	for name, c := range cases {
		if v, ok := ThrottledShare(c.b, c.a, c.hb, c.ha); ok {
			t.Errorf("%s: invented a share %v", name, v)
		}
	}
}

func shareOf(v float64) *float64 { return &v }

func TestAggregateStep_ThrottledPastTheBoundIsGeneratorLimited_AndNeverComfortable(t *testing.T) {
	in := StepInput{Step: 1, Users: 20, RampSeconds: 10, ScenarioID: "HL-1", TargetP95Ms: 1000, MaxErrorRate: 0.5}
	in.GeneratorThrottled = shareOf(0.9)
	st := AggregateStep(stepFixture(), in)
	if !st.GeneratorLimited || st.Comfortable {
		t.Fatalf("a step run at 90%% throttling: generator_limited=%v comfortable=%v", st.GeneratorLimited, st.Comfortable)
	}
	if st.GeneratorCPUThrottledShare == nil || *st.GeneratorCPUThrottledShare != 0.9 || st.GeneratorNotMeasured {
		t.Errorf("the measured share must be recorded: %+v notMeasured=%v", st.GeneratorCPUThrottledShare, st.GeneratorNotMeasured)
	}
	// the same step at the bound and below is the target's reading, comfortable as before
	for _, v := range []float64{0, 0.1, GeneratorThrottleBound} {
		in.GeneratorThrottled = shareOf(v)
		if s := AggregateStep(stepFixture(), in); s.GeneratorLimited || !s.Comfortable || s.GeneratorCPUThrottledShare == nil {
			t.Errorf("share %v: %+v", v, s)
		}
	}
}

func TestAggregateStep_UnreadableCgroupIsRecordedNotMeasured_AndChangesNothingElse(t *testing.T) {
	in := StepInput{Step: 1, Users: 20, RampSeconds: 10, ScenarioID: "HL-1", TargetP95Ms: 1000, MaxErrorRate: 0.5}
	st := AggregateStep(stepFixture(), in) // GeneratorThrottled nil
	if !st.GeneratorNotMeasured || st.GeneratorCPUThrottledShare != nil || st.GeneratorLimited {
		t.Errorf("unreadable cgroup: notMeasured=%v share=%v limited=%v", st.GeneratorNotMeasured, st.GeneratorCPUThrottledShare, st.GeneratorLimited)
	}
	if !st.Comfortable {
		t.Error("an unmeasured generator must not turn a comfortable step uncomfortable")
	}
	if e := AggregateStep(nil, in); !e.GeneratorNotMeasured || e.Status != report.LoadStepSetupFailed {
		t.Errorf("a step with no request still says the generator was not measured: %+v", e)
	}
}

func TestVerdict_GeneratorLimitedStepIsNotAPlainPass_AndNamesTheGenerator(t *testing.T) {
	p := &scenario.HTTPLoadProfile{}
	steps := []report.LoadStep{
		{Step: 1, Sessions: 5, Status: report.LoadStepMeasured, Comfortable: true},
		{Step: 2, Sessions: 20, Status: report.LoadStepMeasured, GeneratorLimited: true, GeneratorCPUThrottledShare: shareOf(0.9)},
		{Step: 3, Sessions: 40, Status: report.LoadStepNotRun},
	}
	o := Verdict(p, steps, 2)
	if o.Status != "degraded" || !strings.Contains(o.Observed, "load generator, not the target") || !strings.Contains(o.Observed, "90%") {
		t.Errorf("verdict = %+v", o)
	}
	// when the generator limited the FIRST step nothing was comfortable: failed, and Describe says why
	only := []report.LoadStep{{Step: 1, Sessions: 5, Status: report.LoadStepMeasured, GeneratorLimited: true, GeneratorCPUThrottledShare: shareOf(0.8)}}
	o = Verdict(p, only, 1)
	if o.Status != "failed" || !strings.Contains(o.Observed, "the load generator was the limit") || !strings.Contains(o.Observed, "80%") {
		t.Errorf("verdict = %+v", o)
	}
	// without any generator-limited step the verdict is the old one
	steps[1] = report.LoadStep{Step: 2, Sessions: 20, Status: report.LoadStepMeasured}
	if o := Verdict(p, steps, 2); o.Status != "passed" {
		t.Errorf("verdict = %+v, want passed", o)
	}
}

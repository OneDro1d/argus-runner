package argus

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/httpload"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// #615 / the executor's own cgroup CPU throttling, read before and after each step's JMeter run.

// cpuScript answers httpLoadCPUStat for a run of steps: reading 2i is step i's "before", 2i+1 its "after", and
// step i throttled delta[i] of 100 periods. A nil script entry is an unreadable cgroup.
func cpuScript(delta []int64) func() (httpload.CPUStat, bool) {
	var calls int
	var cur httpload.CPUStat
	return func() (httpload.CPUStat, bool) {
		i := calls
		calls++
		if i%2 == 1 { // an "after": the step's periods and throttled periods
			cur.Periods += 100
			cur.Throttled += delta[i/2]
		}
		return cur, true
	}
}

func TestHTTPLoad_ThrottledExecutorStep_IsGeneratorLimited_NeverComfortable_AndStopsTheRamp(t *testing.T) {
	e := newHTTPLoadEnv(t)
	httpLoadCPUStat = cpuScript([]int64{10, 60, 90}) // step 2 (4 users) runs with the executor throttled in 60% of periods
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-001", "2, 4, 8", httpLoadDefaultLoad)
	f := &loadFake{gen: func(n int, p map[string]string) string { return httpJTL("HL-001", p, "200", 5) }} // the target is fine
	res := e.run(t, httpLoadConfig("http://api-lab.invalid", allowAPILab), f)

	if len(f.calls) != 2 {
		t.Fatalf("the ramp went on past the generator-limited step: %d runs, want 2", len(f.calls))
	}
	s1, s2 := res.LoadSteps[0], res.LoadSteps[1]
	if !s1.Comfortable || s1.GeneratorLimited || s1.GeneratorCPUThrottledShare == nil || *s1.GeneratorCPUThrottledShare != 0.10 {
		t.Errorf("step 1 (10%% throttled) = %+v", s1)
	}
	if s2.Comfortable || !s2.GeneratorLimited || s2.GeneratorCPUThrottledShare == nil || *s2.GeneratorCPUThrottledShare != 0.60 {
		t.Errorf("step 2 (60%% throttled) must be generator_limited, not comfortable, with the share: %+v", s2)
	}
	if res.LoadSteps[2].Status != report.LoadStepNotRun || res.LoadStoppedAtStep != 2 {
		t.Errorf("step 3 = %s, stopped at %d; want not_run and 2", res.LoadSteps[2].Status, res.LoadStoppedAtStep)
	}
	if res.Status != report.StatusDegraded || res.Failure == nil ||
		!strings.Contains(res.Failure.Observed, "load generator, not the target") || !strings.Contains(res.Failure.Observed, "60%") {
		t.Errorf("verdict must say the generator was the limit: status=%s failure=%+v", res.Status, res.Failure)
	}
}

func TestHTTPLoad_UnreadableCgroup_IsRecordedNotMeasured_AndTheRunIsUnharmed(t *testing.T) {
	e := newHTTPLoadEnv(t) // its default httpLoadCPUStat reads nothing
	writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-001", "2, 4", httpLoadDefaultLoad)
	f := &loadFake{gen: func(n int, p map[string]string) string { return httpJTL("HL-001", p, "200", 5) }}
	res := e.run(t, httpLoadConfig("http://api-lab.invalid", allowAPILab), f)
	if res.Status != "passed" || len(f.calls) != 2 {
		t.Fatalf("an unreadable cgroup must not fail or stop the run: status=%s calls=%d", res.Status, len(f.calls))
	}
	for _, s := range res.LoadSteps {
		if !s.GeneratorNotMeasured || s.GeneratorCPUThrottledShare != nil || s.GeneratorLimited || !s.Comfortable {
			t.Errorf("step %d: notMeasured=%v share=%v limited=%v comfortable=%v", s.Step, s.GeneratorNotMeasured, s.GeneratorCPUThrottledShare, s.GeneratorLimited, s.Comfortable)
		}
	}
}

func TestPlainLoad_RecordsTheExecutorsThrottling_AndNotMeasuredWhenUnreadable(t *testing.T) {
	run := func(read func() (httpload.CPUStat, bool)) *report.LoadStats {
		old := httpLoadCPUStat
		httpLoadCPUStat = read
		defer func() { httpLoadCPUStat = old }()
		dir := t.TempDir()
		scDir := filepath.Join(dir, "scenarios")
		writeScenarioMD(t, scDir, "http-ingestion", "LOAD-001", loadScenarioMD("LOAD-001", loadBodyGenerous))
		fr := &multiSampleRunner{elapsedMs: []int{10, 20, 30}, codes: []int{202, 202, 202}}
		c := &config.Config{}
		c.Project.Name = "order-service"
		c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://order-api:8080"}
		rr, err := RunAll(c, scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", fr)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := rr.Report.Find("LOAD-001")
		if res.Load == nil {
			t.Fatal("no load record")
		}
		return res.Load
	}
	n := 0
	ls := run(func() (httpload.CPUStat, bool) {
		n++
		return httpload.CPUStat{Periods: int64(n-1) * 80, Throttled: int64(n-1) * 20}, true
	})
	if ls.GeneratorCPUThrottledShare == nil || *ls.GeneratorCPUThrottledShare != 0.25 || ls.GeneratorNotMeasured {
		t.Errorf("plain ## LOAD record: share=%v notMeasured=%v, want 0.25", ls.GeneratorCPUThrottledShare, ls.GeneratorNotMeasured)
	}
	if ls := run(func() (httpload.CPUStat, bool) { return httpload.CPUStat{}, false }); ls.GeneratorCPUThrottledShare != nil || !ls.GeneratorNotMeasured {
		t.Errorf("unreadable: share=%v notMeasured=%v", ls.GeneratorCPUThrottledShare, ls.GeneratorNotMeasured)
	}
}

package chain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// a driver call that never returns (an amqp publish into a broker holding a memory
// alarm) must not hold the chain, and with it every other run of the instance, past the chain's own
// declared time.

func TestRun_AStepThatNeverReturnsIsCappedFromOutsideAndTheNextRunStarts(t *testing.T) {
	old := stepGrace
	stepGrace = 100 * time.Millisecond
	t.Cleanup(func() { stepGrace = old })

	release := make(chan struct{})
	lateDone := make(chan struct{})
	hang := Step{Name: "publish-hangs", Run: func(cid string, vars map[string]string) report.StepResult {
		<-release // the driver call that never returns
		vars["late"] = "written after the run finished"
		close(lateDone)
		return report.StepResult{Status: "passed", Observed: "late pass", Captured: map[string]string{"late": "x"}}
	}}
	var laterFired, cleanupFired bool
	later := Step{Name: "next-step", Run: func(string, map[string]string) report.StepResult {
		laterFired = true
		return report.StepResult{Status: "passed"}
	}}
	cleanup := Step{Name: "cleanup", Always: true, Run: func(string, map[string]string) report.StepResult {
		cleanupFired = true
		return report.StepResult{Status: "passed"}
	}}

	start := time.Now()
	res := within(t, 3*time.Second, "chain.Run", func() report.ScenarioResult {
		return Run("tr-cap", []Step{hang, later, cleanup}, 200*time.Millisecond)
	})
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Fatalf("Run returned after %s, before the chain's remaining time plus the grace", took)
	}

	if res.Status != "failed" {
		t.Fatalf("Status = %q, want failed", res.Status)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("got %d steps, want 3: %+v", len(res.Steps), res.Steps)
	}
	if s := res.Steps[0]; s.Status != "failed" || !strings.Contains(s.Observed, "the step did not return within") ||
		!strings.Contains(s.Observed, "abandoned") {
		t.Fatalf("capped step = %+v, want a failure naming the cap and the abandonment", s)
	}
	if res.Failure == nil || !strings.HasPrefix(res.Failure.Observed, "step 'publish-hangs' failed: the step did not return within") {
		t.Fatalf("Failure = %+v, want the capped step named as the cause", res.Failure)
	}
	for _, s := range res.Steps[1:] {
		if s.Status != report.StepNotMeasured || !strings.Contains(s.Observed, `"publish-hangs" did not return and was abandoned`) {
			t.Fatalf("step %q = %+v, want not-measured naming the abandoned step", s.Name, s)
		}
	}
	if laterFired || cleanupFired {
		t.Fatalf("a step fired after the chain ended (next-step=%v cleanup=%v)", laterFired, cleanupFired)
	}
	if res.Residue == "" {
		t.Fatal("Residue is empty: the run may have left something behind and must say so")
	}

	// The run is finished. Snapshot it, let the abandoned call finish and write, and compare.
	before, _ := json.Marshal(res)
	close(release)
	select {
	case <-lateDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned call never finished")
	}
	time.Sleep(50 * time.Millisecond)
	after, _ := json.Marshal(res)
	if string(before) != string(after) {
		t.Fatalf("the abandoned call changed the finished run's results:\nbefore %s\nafter  %s", before, after)
	}

	// The executor goes on: a second run starts and completes.
	ran := false
	second := within(t, 2*time.Second, "second chain.Run", func() report.ScenarioResult {
		return Run("tr-next", []Step{{Name: "ok", Run: func(string, map[string]string) report.StepResult {
			ran = true
			return report.StepResult{Status: "passed"}
		}}}, 200*time.Millisecond)
	})
	if !ran || second.Status != "passed" {
		t.Fatalf("second run: ran=%v status=%q", ran, second.Status)
	}
}

func TestRun_AHungCleanupStepAfterTheDeadlineIsCappedToo(t *testing.T) {
	old := stepGrace
	stepGrace = 50 * time.Millisecond
	t.Cleanup(func() { stepGrace = old })
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	slow := Step{Name: "slow", Run: func(string, map[string]string) report.StepResult {
		time.Sleep(120 * time.Millisecond)
		return report.StepResult{Status: "passed"}
	}}
	hungCleanup := Step{Name: "cleanup", Always: true, Run: func(string, map[string]string) report.StepResult {
		<-block
		return report.StepResult{Status: "passed"}
	}}
	res := within(t, 3*time.Second, "chain.Run", func() report.ScenarioResult {
		return Run("tr-cleanup", []Step{slow, {Name: "never", Run: slow.Run}, hungCleanup}, 100*time.Millisecond)
	})
	last := res.Steps[len(res.Steps)-1]
	if last.Name != "cleanup" || last.Status != report.StepRanAfterFailureFailed || !strings.Contains(last.Observed, "did not return within") {
		t.Fatalf("hung cleanup step = %+v", last)
	}
}

func TestRun_TheBetweenStepsMessageIsUnchanged(t *testing.T) {
	slow := func(string, map[string]string) report.StepResult {
		time.Sleep(80 * time.Millisecond)
		return report.StepResult{Status: "passed"}
	}
	res := Run("tr-between", []Step{{Name: "a", Run: slow}, {Name: "b", Run: slow}}, 40*time.Millisecond)
	if res.Failure == nil || res.Failure.Observed != `chain exceeded its declared ## TIMEOUT of 40ms (stopped before step "b")` {
		t.Fatalf("Failure = %+v, want the existing between-steps message", res.Failure)
	}
}

func TestRun_NoBudgetMeansNoCap(t *testing.T) {
	res := Run("tr-nocap", []Step{{Name: "a", Run: func(string, map[string]string) report.StepResult {
		time.Sleep(50 * time.Millisecond)
		return report.StepResult{Status: "passed"}
	}}})
	if res.Status != "passed" {
		t.Fatalf("Status = %q", res.Status)
	}
}

func TestRun_APanicInAStepStillSurfacesOnTheCallersGoroutine(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("the panic was swallowed")
		}
	}()
	Run("tr-panic", []Step{{Name: "p", Run: func(string, map[string]string) report.StepResult { panic("boom") }}}, time.Second)
}

func within[T any](t *testing.T, d time.Duration, what string, fn func() T) T {
	t.Helper()
	res := make(chan T, 1)
	go func() { res <- fn() }()
	select {
	case v := <-res:
		return v
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
		panic("unreachable")
	}
}

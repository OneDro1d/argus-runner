package argus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// (msgbus tester, T8 finding A, runs 20261003T172536253 and 20261003T173323283): when the broker
// blocks, the publishers it holds keep the JVM alive past the step-aware backstop, so Run returns an error. The run
// loop used to file that as a rig failure BEFORE looking at the step it had just measured, and the row read
// `error` "jmeter run error: … backstop …" instead of `failed` "blocked by broker: <reason> … at step N".
// The broker refusing load is the SUT's answer; the backstop is a consequence of it.

// errLoadFake writes the JTL of call n like loadFake, then returns err(n) (nil = the run finished).
type errLoadFake struct {
	loadFake
	err func(n int) error
}

func (f *errLoadFake) Run(base, jtl string, props map[string]string, timeout time.Duration) error {
	if err := f.loadFake.Run(base, jtl, props, timeout); err != nil {
		return err
	}
	return f.err(len(f.calls) - 1)
}

var errBackstop = errors.New("jmeter process exceeded its backstop of 3m0s; the EXECUTOR's own process-level backstop against a hung jmeter killed it")

func TestAMQPLoad_BlockedStepWhoseJVMHitTheBackstop_FailsAsBlockedByBroker(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4, 8", "")
	f := &errLoadFake{
		loadFake: loadFake{gen: func(n int, p map[string]string) string {
			if n == 1 {
				return blockedJTL("AL-001")
			}
			return healthyJTL("AL-001", p)
		}},
		err: func(n int) error {
			if n == 1 {
				return errBackstop
			}
			return nil
		},
	}
	res := e.run(t, loadConfig(t, allowLab), f)

	if len(f.calls) != 2 {
		t.Fatalf("runner calls = %d, want 2: the ramp must stop at the blocked step", len(f.calls))
	}
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed: a broker block is the SUT's answer, not a rig error (failure=%+v)", res.Status, res.Failure)
	}
	if res.Failure == nil {
		t.Fatal("no failure on a failed row")
	}
	obs := res.Failure.Observed
	for _, want := range []string{"blocked by broker", "memory alarm", "step 2 of 3", "4 sessions"} {
		if !strings.Contains(obs, want) {
			t.Errorf("observed lacks %q: %s", want, obs)
		}
	}
	if !strings.Contains(obs, "backstop") {
		t.Errorf("the backstop kill must still be reported (as the second line), not hidden: %s", obs)
	}
	if i, j := strings.Index(obs, "blocked by broker"), strings.Index(obs, "backstop"); i < 0 || j < 0 || i > j {
		t.Errorf("the broker block must lead and the backstop follow: %s", obs)
	}
	if res.Failure.Expected == nil {
		t.Error("a failed row names what was expected (the scenario's EXPECT), as every other blocked verdict does")
	}
	got := []string{res.LoadSteps[0].Status, res.LoadSteps[1].Status, res.LoadSteps[2].Status}
	if got[0] != "measured" || got[1] != "blocked" || got[2] != "not_run" {
		t.Errorf("step statuses = %v", got)
	}
}

// VR-7 holds in the other direction: a rig failure with NO measured block is still the rig's error, never a SUT verdict.
func TestAMQPLoad_BackstopWithoutABlockedStep_IsStillARigError(t *testing.T) {
	for name, jtl := range map[string]func(p map[string]string) string{
		"healthy rows": func(p map[string]string) string { return healthyJTL("AL-001", p) },
		"no rows":      func(p map[string]string) string { return jtlHead },
	} {
		t.Run(name, func(t *testing.T) {
			e := newLoadEnv(t)
			writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4", "")
			f := &errLoadFake{
				loadFake: loadFake{gen: func(n int, p map[string]string) string { return jtl(p) }},
				err:      func(n int) error { return errBackstop },
			}
			res := e.run(t, loadConfig(t, allowLab), f)
			if len(f.calls) != 1 {
				t.Fatalf("runner calls = %d, want 1: a rig failure stops the ramp", len(f.calls))
			}
			if res.Status != report.StatusError {
				t.Fatalf("status = %q, want error: nothing says the broker refused (failure=%+v)", res.Status, res.Failure)
			}
			if res.Failure == nil || !strings.HasPrefix(res.Failure.Observed, "jmeter run error: ") || strings.Contains(res.Failure.Observed, "blocked by broker") {
				t.Errorf("failure = %+v", res.Failure)
			}
		})
	}
}

// The backstop text is the runner's, and a runner error can carry anything the process printed: it is scrubbed on
// the new path too.
func TestAMQPLoad_BlockedPlusBackstop_ScrubsTheRunnerText(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2", "")
	f := &errLoadFake{
		loadFake: loadFake{gen: func(n int, p map[string]string) string { return blockedJTL("AL-001") }},
		err: func(n int) error {
			return fmt.Errorf("jmeter process exceeded its backstop; last output: amqp://loaduser:%s@load-lab.invalid:5672/", loadSecret)
		},
	}
	res := e.run(t, loadConfig(t, allowLab), f)
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "blocked by broker") {
		t.Fatalf("failure = %+v", res.Failure)
	}
	if strings.Contains(res.Failure.Observed, loadSecret) {
		t.Error("the broker password reached Observed through the runner's error text")
	}
	if _, err := os.Stat(e.results); err != nil {
		t.Fatal(err)
	}
}

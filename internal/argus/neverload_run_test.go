package argus

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// a load check whose test target declares `load_test: never` is refused at the last door,
// before anything is dialled. The target is ALLOWED under load_allowed_targets here, so the older S9 door
// would let it through: only the new door can produce this refusal.

const neverTargets = "test_targets:\n  - name: live\n    label: the live bus\n    load_test: never\n    match: { scenario_prefixes: [AL-] }\n"

func TestAMQPLoad_NeverTargetIsRefusedBeforeFiring(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	res := e.run(t, loadConfig(t, allowLab+neverTargets), f)

	if len(f.calls) != 0 {
		t.Fatalf("the runner (the dial) was called %d time(s) for a check of a never target", len(f.calls))
	}
	if e.envCalls != 0 || len(e.sleeps) != 0 {
		t.Errorf("a refused check touched the environment (%d) or slept (%d)", e.envCalls, len(e.sleeps))
	}
	if res.Status != report.StatusError {
		t.Errorf("status = %q, want %q", res.Status, report.StatusError)
	}
	obs := ""
	if res.Failure != nil {
		obs = res.Failure.Observed
	}
	t.Logf("REFUSAL TEXT: %s", obs)
	for _, want := range []string{"refused before firing", `"live"`, "load_test: never", "Nothing was sent"} {
		if !strings.Contains(obs, want) {
			t.Errorf("refusal text lacks %q: %s", want, obs)
		}
	}
	for _, leak := range []string{"load-lab.invalid", "amqp://", loadSecret, "loaduser"} {
		if strings.Contains(obs, leak) {
			t.Errorf("refusal leaks %q", leak)
		}
	}
}

// Control: the same check with the target NOT declaring the key runs (so the refusal above is the key's doing).
func TestAMQPLoad_SameCheckWithoutTheKeyStillRuns(t *testing.T) {
	e := newLoadEnv(t)
	writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-001", "2, 4", "")
	f := &loadFake{gen: func(n int, p map[string]string) string { return healthyJTL("AL-001", p) }}
	_ = e.run(t, loadConfig(t, allowLab+strings.Replace(neverTargets, "    load_test: never\n", "", 1)), f)
	if len(f.calls) == 0 {
		t.Fatal("without load_test: never the check did not run")
	}
}

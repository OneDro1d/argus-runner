package argus

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpload"
)

// an HTTP Load step whose JTL is over amqpload.JTLRowsFloor is never judged on a prefix.
// The step ends the scenario `error` with the words, and the ramp does not go on.
func TestHTTPLoad_AStepOverTheRowCapIsErrorWithTheWordsNeverAPass(t *testing.T) {
	pinMemory(t, "", "") // no readable limit: the cap is its floor
	big := func(rows int) func(int, map[string]string) string {
		return func(_ int, p map[string]string) string {
			line := "1000000,5," + p["scenario.id"] + ",200,msg,T 1-1,true,\n"
			return jtlHead + strings.Repeat(line, rows)
		}
	}
	run := func(rows int) (status, observed string, calls int) {
		e := newHTTPLoadEnv(t)
		writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-009", "2, 4", httpLoadDefaultLoad)
		f := &loadFake{gen: big(rows)}
		res := e.run(t, httpLoadConfig("http://api-lab.invalid:8081", allowAPILab), f)
		if res.Failure != nil {
			observed = res.Failure.Observed
		}
		return res.Status, observed, len(f.calls)
	}

	// positive control: exactly at the cap, the step is read and judged (comfortable, so the ramp goes on).
	if status, observed, calls := run(amqpload.JTLRowsFloor); status != "passed" || calls != 2 {
		t.Fatalf("at the cap: status=%s calls=%d observed=%q, want passed over 2 steps", status, calls, observed)
	}
	status, observed, calls := run(amqpload.JTLRowsFloor + 1)
	if status != "error" {
		t.Fatalf("one row over the cap: status = %q (%s), want \"error\"", status, observed)
	}
	if calls != 1 {
		t.Errorf("the ramp must stop at the step that was not judged: %d JMeter runs, want 1", calls)
	}
	for _, want := range []string{"step 1:", "wrote 500001 samples", "cap of 500000", "no memory limit could be read", "NOT judged", "--executor-memory"} {
		if !strings.Contains(observed, want) {
			t.Errorf("observed must say %q: %q", want, observed)
		}
	}
}

// the per-step cap grows with the executor's memory limit the same way the plain-LOAD cap does.
// 600,000 rows are over the 500,000 floor (refused at the default 1536Mi) and under the 4Gi cap of 1,333,333.
func TestHTTPLoad_TheStepRowCapScalesWithTheMemoryLimit(t *testing.T) {
	big := func(_ int, p map[string]string) string {
		return jtlHead + strings.Repeat("1000000,5,"+p["scenario.id"]+",200,msg,T 1-1,true,\n", 600_000)
	}
	run := func() (status, observed string, calls int) {
		e := newHTTPLoadEnv(t)
		writeHTTPLoad(t, filepath.Join(e.dir, "scenarios"), "HL-009", "2, 4", httpLoadDefaultLoad)
		f := &loadFake{gen: big}
		res := e.run(t, httpLoadConfig("http://api-lab.invalid:8081", allowAPILab), f)
		if res.Failure != nil {
			observed = res.Failure.Observed
		}
		return res.Status, observed, len(f.calls)
	}
	pinMemory(t, gi4, "")
	if status, observed, calls := run(); status != "passed" || calls != 2 {
		t.Fatalf("600000 rows a step at 4Gi: status=%s calls=%d observed=%q, want judged (passed over 2 steps)", status, calls, observed)
	}
	pinMemory(t, mi1536, "") // negative control, same data
	status, observed, calls := run()
	if status != "error" || calls != 1 || !strings.Contains(observed, "cap of 500000") || !strings.Contains(observed, "wrote 600000 samples") {
		t.Fatalf("600000 rows a step at 1536Mi: status=%s calls=%d observed=%q, want error naming cap 500000 and 600000 samples", status, calls, observed)
	}
}

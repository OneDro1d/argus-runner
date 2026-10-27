package argus

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpload"
)

// an AMQP Load step whose JTL is over amqpload.JTLRowsFloor is never judged on a prefix.
func TestAMQPLoad_AStepOverTheRowCapIsErrorWithTheWordsNeverAPass(t *testing.T) {
	pinMemory(t, "", "") // no readable limit: the cap is its floor
	run := func(rows int) (status, observed string, calls int) {
		e := newLoadEnv(t)
		writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-009", "2, 4", "")
		f := &loadFake{gen: func(_ int, p map[string]string) string {
			// the setup row, then `rows-1` publish rows: exactly `rows` data rows in all
			return jtlHead + "1000000,5,AL-009-amqp-setup,200,session ready,T 1-1,true,\n" +
				strings.Repeat("1011000,2,AL-009-amqp-publish,200,published 1000B confirm=each us=2100,T 1-1,true,\n", rows-1)
		}}
		res := e.run(t, loadConfig(t, allowLab), f)
		if res.Failure != nil {
			observed = res.Failure.Observed
		}
		return res.Status, observed, len(f.calls)
	}

	// positive control: at the cap the step is read and judged (whatever its verdict, it is not the cap's error).
	if _, observed, calls := run(amqpload.JTLRowsFloor); strings.Contains(observed, "NOT judged") || calls < 1 {
		t.Fatalf("at the cap the step must be read: calls=%d observed=%q", calls, observed)
	}
	status, observed, calls := run(amqpload.JTLRowsFloor + 1)
	if status != "error" {
		t.Fatalf("one row over the cap: status = %q (%s), want \"error\"", status, observed)
	}
	if calls != 1 {
		t.Errorf("the ramp must stop at the step that was not judged: %d JMeter runs, want 1", calls)
	}
	for _, want := range []string{"step 1:", "wrote 500001 samples", "cap of 500000", "NOT judged", "--executor-memory"} {
		if !strings.Contains(observed, want) {
			t.Errorf("observed must say %q: %q", want, observed)
		}
	}
}

// the AMQP ramp's per-step cap grows with the memory limit too (600,000 rows: over the 500,000
// floor, under the 4Gi cap of 1,333,333).
func TestAMQPLoad_TheStepRowCapScalesWithTheMemoryLimit(t *testing.T) {
	run := func() (observed string) {
		e := newLoadEnv(t)
		writeAMQPLoad(t, filepath.Join(e.dir, "scenarios"), "AL-009", "2, 4", "")
		f := &loadFake{gen: func(_ int, p map[string]string) string {
			return jtlHead + "1000000,5,AL-009-amqp-setup,200,session ready,T 1-1,true,\n" +
				strings.Repeat("1011000,2,AL-009-amqp-publish,200,published 1000B confirm=each us=2100,T 1-1,true,\n", 599_999)
		}}
		res := e.run(t, loadConfig(t, allowLab), f)
		if res.Failure != nil {
			observed = res.Failure.Observed
		}
		return observed
	}
	pinMemory(t, gi4, "")
	if observed := run(); strings.Contains(observed, "NOT judged") {
		t.Fatalf("600000 rows a step at 4Gi must be read and judged: %q", observed)
	}
	pinMemory(t, mi1536, "") // negative control, same data
	if observed := run(); !strings.Contains(observed, "NOT judged") || !strings.Contains(observed, "wrote 600000 samples") || !strings.Contains(observed, "cap of 500000") {
		t.Fatalf("600000 rows a step at 1536Mi must be refused with the cap and the count: %q", observed)
	}
}

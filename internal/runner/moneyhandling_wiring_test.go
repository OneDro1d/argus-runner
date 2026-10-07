package runner

import (
	"os"
	"strings"
	"testing"
)

// T5.4 follow-up — the two Bootstrap/Run connections no behavioural test reaches
// (nothing in this package starts a federated Server against a control plane). Same structural answer
// as TestSUTConfigIsWiredIntoTheExecutor: each line can go missing, everything still compiles, and every
// instance would report NULL, which the control plane reads, by design, as "not money-handling".
func TestMoneyHandlingIsWiredIntoRegisterAndTheLoop(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ line, why string }{
		{"ConfigPath: cfg.Fed.Exec.ConfigPath", "Bootstrap never gives the Executor its config path, so every poll reports nil"},
		{"moneyHandlingFor(s.fed.Exec.ConfigPath)", "register-on-start never sends money_handling, so a first compose registration stores NULL"},
	} {
		if !strings.Contains(string(src), want.line) {
			t.Errorf("runner.go lacks %q: %s", want.line, want.why)
		}
	}
}

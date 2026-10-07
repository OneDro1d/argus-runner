package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// moneyhandling_wire_test.go — T5.4 follow-up: the executor's money_handling
// declaration rides BOTH the register and the poll (the same carrier rule pollRequestFor's own header
// states: an executor-only fact reaches the control plane exactly once on register-on-start — refused
// with 403 on k3d/aks re-register — so anything that must self-heal has to ride the poll too).
//
// THE SEMANTICS, three-valued, and this is the point of the whole change:
//
//	*bool == nil    NOT REPORTED — an executor older than this change, or a config that failed to
//	                load. Never false-by-accident: a control plane reading false here would turn the
//	                money guard OFF for an instance that simply has not told it anything yet.
//	*bool == false  the config loaded and does not declare money_handling (the YAML's own zero value
//	                is its default — "absent in config = false").
//	*bool == true   the config loaded and declares money_handling: true.

func writeMoneyConfig(t *testing.T, declare string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: p\n" + declare + "targets:\n  http:\n    base_url: http://sut.invalid\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMoneyHandlingFor_TrueDeclared(t *testing.T) {
	p := writeMoneyConfig(t, "money_handling: true\n")
	got := moneyHandlingFor(p)
	if got == nil || !*got {
		t.Fatalf("moneyHandlingFor(declared true) = %v, want *true", got)
	}
}

func TestMoneyHandlingFor_FalseDeclared(t *testing.T) {
	p := writeMoneyConfig(t, "money_handling: false\n")
	got := moneyHandlingFor(p)
	if got == nil || *got {
		t.Fatalf("moneyHandlingFor(declared false) = %v, want *false", got)
	}
}

func TestMoneyHandlingFor_AbsentInConfigIsFalse(t *testing.T) {
	// "absent in config = false (config default)" — a config that loads fine and simply never
	// mentions money_handling must report the YAML zero value, not nil: nil is reserved for "this
	// executor told us nothing at all", and a loadable config always tells us something.
	p := writeMoneyConfig(t, "")
	got := moneyHandlingFor(p)
	if got == nil || *got {
		t.Fatalf("moneyHandlingFor(absent) = %v, want *false, not nil — absence in a LOADABLE config is a declared false, never \"not reported\"", got)
	}
}

func TestMoneyHandlingFor_ConfigFailsToLoadIsNil(t *testing.T) {
	// An unresolved ${VAR} makes config.Load fail exactly like a missing/malformed file — the
	// production path this exercises is the same one declaredTargets already relies on.
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: p\nmoney_handling: true\ntargets:\n  http:\n    base_url: ${UNSET_MONEYGUARD_TEST_VAR}\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := moneyHandlingFor(p)
	if got != nil {
		t.Fatalf("moneyHandlingFor(config that fails to load) = %v, want nil — a failed load must never be reported as false-by-accident", got)
	}
}

func TestMoneyHandlingFor_EmptyPathIsNil(t *testing.T) {
	if got := moneyHandlingFor(""); got != nil {
		t.Fatalf("moneyHandlingFor(\"\") = %v, want nil", got)
	}
}

func TestRegisterRequestFor_CarriesMoneyHandling(t *testing.T) {
	yes := true
	req := registerRequestFor(FedConfig{InstanceID: "i1"}, "pub", "x25519", nil, &yes, nil)
	if req.MoneyHandling == nil || !*req.MoneyHandling {
		t.Fatalf("RegisterRequest.MoneyHandling = %v, want *true", req.MoneyHandling)
	}
}

func TestRegisterRequestFor_NilMoneyHandlingOmitted(t *testing.T) {
	req := registerRequestFor(FedConfig{InstanceID: "i1"}, "pub", "x25519", nil, nil, nil)
	if req.MoneyHandling != nil {
		t.Fatalf("RegisterRequest.MoneyHandling = %v, want nil (not reported)", req.MoneyHandling)
	}
}

func TestPollRequestFor_CarriesMoneyHandling(t *testing.T) {
	no := false
	req := pollRequestFor("0.3.29", nil, nil, nil, nil, nil, federation.SUTObservation{}, &no, nil)
	if req.MoneyHandling == nil || *req.MoneyHandling {
		t.Fatalf("PollRequest.MoneyHandling = %v, want *false", req.MoneyHandling)
	}
}

func TestPollRequestFor_NilMoneyHandlingOmitted(t *testing.T) {
	req := pollRequestFor("0.3.29", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil)
	if req.MoneyHandling != nil {
		t.Fatalf("PollRequest.MoneyHandling = %v, want nil (not reported)", req.MoneyHandling)
	}
}

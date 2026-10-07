package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// moneywrites_wire_test.go — "money writes" (T5.4 follow-up, 2026-09-26): mirrors
// moneyhandling_wire_test.go exactly, one level down — moneyWritesAllowFor's nil/"[]"/"[{…}]" rule,
// and that registerRequestFor/pollRequestFor actually carry it.

func TestMoneyWritesAllowFor_DeclaredBlockReported(t *testing.T) {
	p := writeMoneyConfig(t, "money_handling: true\n"+
		"money_writes:\n  allow:\n    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n")
	got := moneyWritesAllowFor(p)
	if got == nil {
		t.Fatal("moneyWritesAllowFor(declared block) = nil, want the encoded allowlist")
	}
	var al []map[string]any
	if err := json.Unmarshal(got, &al); err != nil {
		t.Fatalf("moneyWritesAllowFor did not produce valid JSON: %v (%s)", err, got)
	}
	if len(al) != 1 || al[0]["path"] != "/api/v1/trading/quote" {
		t.Fatalf("decoded allowlist = %+v, want one entry for /api/v1/trading/quote", al)
	}
}

func TestMoneyWritesAllowFor_NoBlockIsExplicitEmptyArray(t *testing.T) {
	// "absent in config = explicit empty" — mirrors moneyHandlingFor's "absent = *false" rule: a
	// LOADABLE config that declares no money_writes must report "[]", never nil, so a re-register
	// that later adds and then removes the block still overwrites the stored allowlist.
	p := writeMoneyConfig(t, "money_handling: true\n")
	got := moneyWritesAllowFor(p)
	if got == nil {
		t.Fatal("moneyWritesAllowFor(no block) = nil, want an explicit \"[]\"")
	}
	if string(got) != "[]" {
		t.Fatalf("moneyWritesAllowFor(no block) = %s, want \"[]\"", got)
	}
}

func TestMoneyWritesAllowFor_ConfigFailsToLoadIsNil(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: p\nmoney_handling: true\ntargets:\n  http:\n    base_url: ${UNSET_MONEYWRITES_TEST_VAR}\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moneyWritesAllowFor(p); got != nil {
		t.Fatalf("moneyWritesAllowFor(config that fails to load) = %s, want nil", got)
	}
}

func TestMoneyWritesAllowFor_EmptyPathIsNil(t *testing.T) {
	if got := moneyWritesAllowFor(""); got != nil {
		t.Fatalf("moneyWritesAllowFor(\"\") = %s, want nil", got)
	}
}

func TestRegisterRequestFor_CarriesMoneyWritesAllow(t *testing.T) {
	allow := json.RawMessage(`[{"method":"POST","path":"/x","spends":false}]`)
	req := registerRequestFor(FedConfig{InstanceID: "i1"}, "pub", "x25519", nil, nil, allow)
	if string(req.MoneyWritesAllow) != string(allow) {
		t.Fatalf("RegisterRequest.MoneyWritesAllow = %s, want %s", req.MoneyWritesAllow, allow)
	}
}

func TestPollRequestFor_CarriesMoneyWritesAllow(t *testing.T) {
	allow := json.RawMessage(`[]`)
	req := pollRequestFor("0.3.29", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, allow)
	if string(req.MoneyWritesAllow) != "[]" {
		t.Fatalf("PollRequest.MoneyWritesAllow = %s, want []", req.MoneyWritesAllow)
	}
}

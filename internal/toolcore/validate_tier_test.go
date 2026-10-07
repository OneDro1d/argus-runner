package toolcore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TS-E2 and TS-E4 in executable form — the OPERATOR-FACING half of VR-E8.
//
// The refusal lands in `errors` (so valid:false, so a non-zero exit) rather than in `warnings`, and
// that placement IS the requirement: onboard.sh already dies on a failed validate-config, so this is
// what makes the refusal real without inventing a second gate the shell would have to learn about.

func tierEnv(t *testing.T, body string) Env {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scen := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatal(err)
	}
	return Env{Instance: "local", ConfigPath: p, ScenariosDir: scen}
}

const twoTierCfg = `project:
  name: e8
observability:
  grafana:
    public_url:
      compose: http://localhost:3000
      k3d:     http://localhost:3000
`

// TS-E2: declare compose and k3d only, then onboard to managed — assert the onboard REFUSES and
// names the missing tier.
func TestValidateConfig_MissingTargetTierIsInvalid(t *testing.T) {
	e := tierEnv(t, twoTierCfg)
	e.Tier = "managed"

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !failed {
		t.Fatal("validate-config must FAIL for a tier the config does not declare (its exit code is onboarding's gate)")
	}
	m := payload.(map[string]any)
	if m["valid"] != false {
		t.Errorf("valid = %v, want false", m["valid"])
	}
	if m["tier"] != "managed" {
		t.Errorf("tier = %v, want managed — the reader must see WHICH tier was checked", m["tier"])
	}
	if !strings.Contains(errorText(m), "managed") || !strings.Contains(errorText(m), "public_url") {
		t.Errorf("the error must name the tier AND the field; got %q", errorText(m))
	}
}

// TS-E4: a config declaring NO public_url at all — the state all five bundled examples were in
// before this build — refuses identically. There is no fall-back and no distinction between
// "omitted my tier" and "declared nothing".
func TestValidateConfig_NoPublicURLAtAllIsInvalid(t *testing.T) {
	e := tierEnv(t, "project:\n  name: e8\n")
	e.Tier = "compose"

	_, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !failed {
		t.Fatal("a config declaring no public_url at all must refuse")
	}
}

// The declared tier passes, and the payload says which tier that judgement was made for. Without
// `tier` in the output a reader cannot tell a config that declares every tier from one that happens
// to declare the only tier that was checked.
func TestValidateConfig_DeclaredTierPasses(t *testing.T) {
	e := tierEnv(t, twoTierCfg)
	e.Tier = "compose"

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if failed {
		t.Fatalf("a declared tier must pass; errors = %v", errorText(payload.(map[string]any)))
	}
	m := payload.(map[string]any)
	if m["tier"] != "compose" {
		t.Errorf("tier = %v, want compose", m["tier"])
	}
	if got, _ := m["tiers_declared"].([]string); len(got) != 2 {
		t.Errorf("tiers_declared = %v, want both declared tiers", m["tiers_declared"])
	}
}

// NO TIER SUPPLIED is not an error and is NOT silently treated as compose. A direct/dogfood run has
// no tier and legitimately validates the rest of the file — but the payload must say the per-tier
// value was NOT examined, so nobody reads a green valid:true as proof it is present. This is the
// same rule as Env.Tier's zero value, enforced at the point where a human reads the result.
func TestValidateConfig_NoTierReportsNotCheckedRatherThanGuessing(t *testing.T) {
	e := tierEnv(t, "project:\n  name: e8\n") // declares NOTHING — would fail if a tier were assumed
	e.Tier = ""

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if failed {
		t.Fatal("with no tier supplied there is nothing to check against — this must not fail the config")
	}
	if got := payload.(map[string]any)["tier"]; got != "not-checked" {
		t.Errorf("tier = %v, want \"not-checked\" — an unexamined check must never read as a passed one", got)
	}
}

// errorText renders the errors slice for assertions. It is deliberately %v over the typed value
// rather than a JSON round-trip: the test asserts what the OPERATOR is told, and the operator sees
// this text either way.
func errorText(m map[string]any) string {
	return fmt.Sprintf("%v", m["errors"])
}

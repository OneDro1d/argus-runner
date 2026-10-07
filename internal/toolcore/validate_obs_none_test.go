package toolcore

import (
	"strings"
	"testing"
)

// (a): under `--obs none` the operator wired no Grafana of Argus's own, so
// observability.grafana.public_url is not required on ANY tier. Onboarding hands the mode to the
// validator as Env.ObsMode (ARGUS_OBS_MODE), the way the tier travels as Env.Tier.

// The refusal an operator who chose no observability used to get: a Grafana URL they had to invent.
func TestValidateConfig_ObsNoneDoesNotRequirePublicURL(t *testing.T) {
	for _, tier := range []string{"compose", "k3d", "managed"} {
		e := tierEnv(t, "project:\n  name: e13\n") // declares NO public_url
		e.Tier = tier
		e.ObsMode = "none"

		payload, failed, err := ValidateConfig(e)
		if err != nil {
			t.Fatalf("%s: ValidateConfig: %v", tier, err)
		}
		if failed {
			t.Errorf("%s: --obs none must not require observability.grafana.public_url; errors = %s", tier, errorText(payload.(map[string]any)))
		}
		if got := payload.(map[string]any)["tier"]; got != "skipped-obs-none" {
			t.Errorf("%s: tier = %v, want \"skipped-obs-none\" — a skipped check must never read as a passed one", tier, got)
		}
	}
}

// A hand-run `argus validate-config` sets no mode and must behave exactly as before.
func TestValidateConfig_NoObsModeStillRequiresPublicURL(t *testing.T) {
	e := tierEnv(t, "project:\n  name: e13\n")
	e.Tier = "compose"

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !failed || !strings.Contains(errorText(payload.(map[string]any)), "public_url") {
		t.Fatalf("with no mode set the tier's public_url is still required; failed=%v errors=%s", failed, errorText(payload.(map[string]any)))
	}
}

// Only `none` relaxes it: bundled, adopt, export and shared all wire a Grafana and keep the rule.
func TestValidateConfig_OtherObsModesStillRequirePublicURL(t *testing.T) {
	for _, mode := range []string{"bundled", "adopt", "export", "shared"} {
		e := tierEnv(t, "project:\n  name: e13\n")
		e.Tier = "compose"
		e.ObsMode = mode

		_, failed, err := ValidateConfig(e)
		if err != nil {
			t.Fatalf("%s: ValidateConfig: %v", mode, err)
		}
		if !failed {
			t.Errorf("--obs %s must still require the tier's public_url", mode)
		}
	}
}

// Declaring a public_url under none is allowed and untouched: the check is skipped, the declared
// tiers are still reported, and the link the executor builds still uses the declared value.
func TestValidateConfig_ObsNoneKeepsADeclaredPublicURL(t *testing.T) {
	e := tierEnv(t, twoTierCfg)
	e.Tier = "compose"
	e.ObsMode = "none"

	payload, failed, err := ValidateConfig(e)
	if err != nil || failed {
		t.Fatalf("ValidateConfig: failed=%v err=%v", failed, err)
	}
	if got, _ := payload.(map[string]any)["tiers_declared"].([]string); len(got) != 2 {
		t.Errorf("tiers_declared = %v, want both declared tiers", payload.(map[string]any)["tiers_declared"])
	}
}

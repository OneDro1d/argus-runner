package toolcore

import (
	"strings"
	"testing"
)

// item 8b (msgbus tester 2026-09-28): "runner__validate_config says valid:false for a correct
// chain-only kit. On tier managed it demands observability.grafana.public_url even when no
// observability is deployed... Suggest: no Grafana requirement when obs is none/absent."

// TestValidateConfig_ManagedTierNoObsBackend_NoPublicURLRequired: a config that declares NEITHER
// observability.loki NOR observability.betterstack, and NO public_url for any tier, must pass on
// tier managed — there is no dashboard this kit could ever produce a deep link for.
func TestValidateConfig_ManagedTierNoObsBackend_NoPublicURLRequired(t *testing.T) {
	e := tierEnv(t, "project:\n  name: e8\n") // declares NOTHING obs-related at all
	e.Tier = "managed"

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if failed {
		t.Fatalf("managed tier with no obs backend declared at all must not require a public_url: errors=%v",
			payload.(map[string]any)["errors"])
	}
	m := payload.(map[string]any)
	if m["tier"] != "managed" {
		t.Errorf("tier = %v, want managed", m["tier"])
	}
}

// TestValidateConfig_ManagedTierWithLoki_StillRequiresPublicURL: the exemption is for "no
// observability plane AT ALL", never a blanket managed-tier exception. A config that DOES declare
// observability.loki but no public_url for managed must still refuse — it has a dashboard, and the
// deep link would break.
func TestValidateConfig_ManagedTierWithLoki_StillRequiresPublicURL(t *testing.T) {
	e := tierEnv(t, "project:\n  name: e8\nobservability:\n  loki:\n    url: http://loki:3100\n")
	e.Tier = "managed"

	_, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !failed {
		t.Fatal("a config that declares a real Loki backend must still require public_url on managed")
	}
}

// TestValidateConfig_ManagedTierPartialPublicURL_StillRefused (TS-E2 regression guard): a config
// that declares public_url for OTHER tiers but not managed must still refuse — the exemption is
// for a file that answers NOTHING about dashboards, not for one that answers everything except
// the tier being checked.
func TestValidateConfig_ManagedTierPartialPublicURL_StillRefused(t *testing.T) {
	e := tierEnv(t, twoTierCfg) // declares public_url for compose+k3d only
	e.Tier = "managed"

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !failed {
		t.Fatal("a config declaring public_url for OTHER tiers must still refuse managed by name")
	}
	m := payload.(map[string]any)
	if !strings.Contains(errorText(m), "managed") || !strings.Contains(errorText(m), "public_url") {
		t.Errorf("the error must still name the tier and the field: %q", errorText(m))
	}
}

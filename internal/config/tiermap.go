package config

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/internal/buildinfo"
)

// TierMap is a value that legitimately DIFFERS PER DEPLOYMENT TIER (M3-FX section E, VR-E1).
//
// `argus-config.yaml`'s own header states the rule that lets one file serve three tiers:
//
//	"THE ONE RULE: every URL is written from the RUNNER's network vantage — the SUT's compose
//	 SERVICE-NAME + in-container port — never localhost:<published-port>."
//
// `http://social-gateway:8080/mcp` therefore works UNCHANGED on compose, k3d and managed, because the
// service name resolves identically from the runner in all three. Almost everything in the file obeys
// that rule, which is why almost nothing needs this type.
//
// `observability.grafana.public_url` is the exception, and was verified by search to be the ONLY
// host-facing URL in the file. It answers a different question — not "where does the RUNNER reach
// this?" but "what does a HUMAN open in a browser?" — and that has a genuinely different answer per
// environment. One value cannot be right for three, which is how the managed tier came to emit
// `localhost:3000` deep links that 404 (INT-008).
//
// The type is general because the RULE is general (the owner's E-ii): the next value that differs per
// tier follows this pattern rather than inventing a second one. Today exactly one field uses it.
type TierMap map[string]string

// CanonicalTiers are the only accepted keys — the same three the control plane's CHECK constraint
// allows (001_init.up.sql). An operator may type `--tier aks`; federation.WireTier normalizes that to
// `managed` BEFORE anything reaches here, so a config can never declare a tier an instance could not
// register as.
var CanonicalTiers = []string{"compose", "k3d", "managed"}

func isCanonicalTier(t string) bool {
	for _, c := range CanonicalTiers {
		if c == t {
			return true
		}
	}
	return false
}

// UnmarshalYAML accepts ONLY the per-tier mapping form.
//
// Rejecting the old scalar AT PARSE TIME is the whole point of having a type here (VR-E3). Decoding
// into a plain map[string]string would make `public_url: http://localhost:3000` a type error with a
// yaml-library message, or worse — with a permissive decoder — an empty map, which would then fail
// later as "no value for your tier" and send the reader hunting for a typo instead of telling them the
// SHAPE CHANGED. The migration is mechanical; only the message makes it obvious.
func (t *TierMap) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		return fmt.Errorf("line %d: this field is now PER-TIER, not a single value. Replace\n"+
			"      public_url: %s\n"+
			"    with a value for each tier you onboard to:\n"+
			"      public_url:\n"+
			"        compose: http://localhost:3000\n"+
			"        k3d:     http://localhost:3000\n"+
			"        managed: "+buildinfo.DefaultGrafana()+"\n"+
			"    (on a k8s tier `localhost` is NOT the operator's machine, so a localhost link cannot open the cluster's Grafana)",
			value.Line, value.Value)
	case yaml.MappingNode:
		m := map[string]string{}
		if err := value.Decode(&m); err != nil {
			return fmt.Errorf("line %d: expected a per-tier map (%s): %w", value.Line, strings.Join(CanonicalTiers, " | "), err)
		}
		for k := range m {
			if !isCanonicalTier(k) {
				return fmt.Errorf("line %d: %q is not a tier name — use one of %s",
					value.Line, k, strings.Join(CanonicalTiers, ", "))
			}
		}
		*t = m
		return nil
	default:
		return fmt.Errorf("line %d: expected a per-tier map of %s", value.Line, strings.Join(CanonicalTiers, " | "))
	}
}

// Resolve returns the value declared for tier. The second result is false when the tier was not
// declared — which callers must treat as a REFUSAL, not as an empty string to paper over.
func (t TierMap) Resolve(tier string) (string, bool) {
	if t == nil {
		return "", false
	}
	v, ok := t[tier]
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

// Has reports whether tier is declared with a non-empty value.
func (t TierMap) Has(tier string) bool { _, ok := t.Resolve(tier); return ok }

// Tiers lists the declared tiers, sorted, for error messages that tell the operator what the file
// DOES say rather than only what it is missing.
func (t TierMap) Tiers() []string {
	out := make([]string, 0, len(t))
	for k := range t {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// missingTierError is the one wording used wherever a per-tier value is absent. VR-E8 requires the
// tier AND the field to be named; the owner also ruled that "declared a map but omitted my tier" and
// "declared nothing at all" produce the SAME refusal, so there is one constructor and no branch.
func missingTierError(field, tier string, declared []string) error {
	if tier == "" {
		return fmt.Errorf("cannot resolve %s: the deployment tier is unknown — it was not supplied, "+
			"and guessing one would resolve against a tier nobody chose", field)
	}
	has := "the file declares none"
	if len(declared) > 0 {
		has = "the file declares: " + strings.Join(declared, ", ")
	}
	return fmt.Errorf("argus-config.yaml does not declare %s for the %q tier (%s). "+
		"This value is REQUIRED for the tier you are onboarding to — onboarding stops here rather than "+
		"producing a dashboard link that does not open. On a k8s tier `localhost` is not your machine, "+
		"so the managed entry must be the cluster's Grafana host", field, tier, has)
}

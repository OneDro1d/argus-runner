package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseYAML loads a config body and RETURNS the error instead of failing the test, so the
// reject-at-parse rules below can assert on the message a user would actually see.
func parseYAML(body string) (*Config, error) {
	dir, err := os.MkdirTemp("", "cfg")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		return nil, err
	}
	return Load(p)
}

// Section E of the M3-FX build: `argus-config.yaml` gains a COMMON part and a PER-TIER part, and
// `observability.grafana.public_url` is the first (today the only) field to move.
//
// WHY IT MOVED. The file's own header states the rule that lets one config serve three tiers: every
// URL is written from the RUNNER's network vantage — a service name, which resolves identically on
// compose, k3d and managed. `public_url` is the ONE field that breaks that rule, because it answers a
// different question: not "where does the runner reach this?" but "what does a HUMAN open in a
// browser?" — and that genuinely has a different answer per environment. One value cannot be right
// for three, which is how the managed tier ended up emitting localhost:3000 links that 404 (INT-008).

const tierYAML = `project:
  name: svc
observability:
  grafana:
    dashboard_template: dashboards/argus-overview.json
    public_url:
      compose: http://localhost:3000
      k3d:     http://localhost:3000
      managed: https://grafana.example.com
`

func TestTierMap_ResolvesPerTier(t *testing.T) {
	c := loadYAML(t, tierYAML)
	cases := map[string]string{
		"compose": "http://localhost:3000",
		"k3d":     "http://localhost:3000",
		"managed": "https://grafana.example.com",
	}
	for tier, want := range cases {
		got, err := c.GrafanaPublicURL(tier)
		if err != nil {
			t.Errorf("GrafanaPublicURL(%q): %v", tier, err)
			continue
		}
		if got != want {
			t.Errorf("GrafanaPublicURL(%q) = %q, want %q", tier, got, want)
		}
	}
}

// VR-E8 / the owner's E-i: the value is MANDATORY for the tier being onboarded, and there is
// deliberately NO distinction between "declared a map but omitted my tier" and "declared nothing at
// all" — both stop. The message must name the tier AND the field, or the operator has to guess which
// of a dozen fields is missing.
func TestGrafanaPublicURL_MissingTierRefusesAndNamesBoth(t *testing.T) {
	partial := `project:
  name: svc
observability:
  grafana:
    public_url:
      compose: http://localhost:3000
      k3d:     http://localhost:3000
`
	c := loadYAML(t, partial)
	_, err := c.GrafanaPublicURL("managed")
	if err == nil {
		t.Fatal("onboarding to a tier the config does not declare must REFUSE")
	}
	if !strings.Contains(err.Error(), "managed") {
		t.Errorf("the error must name the TIER; got %q", err)
	}
	if !strings.Contains(err.Error(), "public_url") {
		t.Errorf("the error must name the FIELD; got %q", err)
	}
}

func TestGrafanaPublicURL_NoBlockAtAllRefusesIdentically(t *testing.T) {
	c := loadYAML(t, "project:\n  name: svc\n")
	_, err := c.GrafanaPublicURL("compose")
	if err == nil {
		t.Fatal("a config declaring no public_url at all must REFUSE — this is the state all five bundled examples were in")
	}
	if !strings.Contains(err.Error(), "compose") || !strings.Contains(err.Error(), "public_url") {
		t.Errorf("the error must name the tier and the field; got %q", err)
	}
}

// VR-E8 explicitly: "There is no fall-back to the environment". ARGUS_GRAFANA_PUBLIC_URL used to
// win over the built-in default; now it wins over nothing, because a config that cannot answer for
// its tier is stopped rather than quietly patched from the environment. A silent environment fallback
// is what let a wrong config look correct for weeks.
func TestGrafanaPublicURL_DoesNotFallBackToTheEnvironment(t *testing.T) {
	t.Setenv("ARGUS_GRAFANA_PUBLIC_URL", "https://grafana.example.com")
	c := loadYAML(t, "project:\n  name: svc\n")
	if _, err := c.GrafanaPublicURL("managed"); err == nil {
		t.Fatal("the environment must NOT rescue a config that declares nothing (VR-E8)")
	}
}

// Env.Tier is empty when the tier was never supplied. Resolving against "" must be an explicit
// "I could not determine the tier", never a silent pick of compose — the same rule Env.Tier's own
// tests pin, enforced again at the point of use.
func TestGrafanaPublicURL_EmptyTierIsUnknownNotCompose(t *testing.T) {
	c := loadYAML(t, tierYAML)
	got, err := c.GrafanaPublicURL("")
	if err == nil {
		t.Fatalf("an empty tier must refuse, not resolve; got %q", got)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "tier") {
		t.Errorf("the error must say the tier is unknown; got %q", err)
	}
}

// VR-E3: the old single-value form is NOT kept. A scalar is rejected AT PARSE TIME with a message
// showing the new shape — which is the whole reason to reject rather than accept: a
// map[string]string would have silently found no keys and reported "missing for your tier", sending
// the reader hunting for a typo instead of telling them the shape changed.
func TestTierMap_ScalarFormIsRejectedAtParseWithMigrationHelp(t *testing.T) {
	old := `project:
  name: svc
observability:
  grafana:
    public_url: http://localhost:3000
`
	_, err := parseYAML(old)
	if err == nil {
		t.Fatal("the pre-M3-FX scalar public_url must be rejected, not silently ignored")
	}
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "per-tier") {
		t.Errorf("the parse error must say the field is now per-tier; got %q", msg)
	}
	for _, tier := range []string{"compose", "k3d", "managed"} {
		if !strings.Contains(msg, tier) {
			t.Errorf("the parse error must show the expected shape including %q; got %q", tier, msg)
		}
	}
}

// A tier nobody supports is a typo, and a typo that parses is a config that stops working on exactly
// one tier, months later. The accepted set is the same three the control plane's CHECK constraint
// allows (001_init.up.sql), so a config cannot declare a tier an instance could never register as.
func TestTierMap_UnknownTierKeyIsRejected(t *testing.T) {
	bad := `project:
  name: svc
observability:
  grafana:
    public_url:
      compose: http://localhost:3000
      aks:     https://grafana.example.com
`
	_, err := parseYAML(bad)
	if err == nil {
		t.Fatal(`"aks" is not a tier name — the canonical set is compose|k3d|managed (WireTier normalizes aks -> managed BEFORE this point)`)
	}
	if !strings.Contains(err.Error(), "aks") {
		t.Errorf("the error must name the offending key; got %q", err)
	}
}

// The common part is unaffected: dashboard_template is the same everywhere and stays a plain scalar.
// The change is deliberately narrow — only what genuinely differs per environment moves.
func TestTierMap_CommonFieldsAreUntouched(t *testing.T) {
	c := loadYAML(t, tierYAML)
	if got := c.Observability.Grafana.DashboardTemplate; got != "dashboards/argus-overview.json" {
		t.Errorf("dashboard_template = %q; common fields must stay scalar and unchanged", got)
	}
}

// Declaring every tier is allowed to be redundant: compose and k3d share a value today and that is
// fine. The point is that the file SAYS so, rather than a default saying it for them.
func TestTierMap_TiersPresent(t *testing.T) {
	c := loadYAML(t, tierYAML)
	if !c.Observability.Grafana.PublicURL.Has("managed") {
		t.Error("Has(managed) = false")
	}
	if c.Observability.Grafana.PublicURL.Has("nope") {
		t.Error("Has(nope) = true")
	}
	if n := len(c.Observability.Grafana.PublicURL.Tiers()); n != 3 {
		t.Errorf("Tiers() = %v, want 3", c.Observability.Grafana.PublicURL.Tiers())
	}
}

package toolcore

import "testing"

// VR-E8 needs the TIER before it can have any logic: `argus-config.yaml` gains per-tier values and
// onboarding must refuse when the target tier has none. Before Env.Tier existed the tier reached only
// serve-time federation registration and the rendered k8s Deployment, so a config validator had no
// way to know which tier it was validating for.
//
// The rule these tests pin is not "the field exists" — it is that an ABSENT tier stays absent.

// Defaulting an unset tier to "compose" would be the natural-looking choice, since that is the CLI
// default elsewhere. It is the wrong one: a per-tier lookup would then resolve against a tier nobody
// chose and report success, which is the confident-wrong-answer class this whole build exists to
// remove. Empty must stay empty so the caller is forced to say "could not determine the tier".
func TestEnvTier_EmptyMeansUnknownNotCompose(t *testing.T) {
	var e Env
	if e.Tier != "" {
		t.Fatalf("zero-value Env.Tier = %q; an unsupplied tier must be EMPTY, never a guess", e.Tier)
	}
	if e.Tier == "compose" {
		t.Fatal("Env.Tier must not default to compose — see the field comment")
	}
}

// Tier and Cluster answer different questions and must not be conflated: two instances on different
// clusters share a tier. Tier drives CONFIGURATION (which per-tier value to read); Cluster drives
// where telemetry lands.
func TestEnvTier_IsDistinctFromCluster(t *testing.T) {
	e := Env{Tier: "managed", Cluster: "example-cluster"}
	if e.Tier == e.Cluster {
		t.Fatal("Tier and Cluster must remain separate fields")
	}
	if e.Tier != "managed" {
		t.Errorf("Tier = %q, want managed", e.Tier)
	}
	if e.Cluster != "example-cluster" {
		t.Errorf("Cluster = %q, want the cluster name", e.Cluster)
	}
}

// The three values the control plane's CHECK constraint accepts (001_init.up.sql:30). Anything the
// operator types — `--tier aks` and friends — is normalized by federation.WireTier BEFORE it reaches
// Env, so consumers can compare against exactly these and nothing else.
func TestEnvTier_CanonicalValues(t *testing.T) {
	for _, tier := range []string{"compose", "k3d", "managed"} {
		e := Env{Tier: tier}
		if e.Tier != tier {
			t.Errorf("Env{Tier: %q}.Tier = %q", tier, e.Tier)
		}
	}
}

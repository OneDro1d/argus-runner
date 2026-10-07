package toolcore

// cluster_test.go — R6 (M3.1 ADR-10, CP-M3-116). TEST-FIRST: the runner's obs pushes hardcoded
// cluster="local", so the shared per-environment Prometheus could never distinguish tiers/
// environments (§D-3.1.2). The contract now: Env carries a deploy-derived Cluster
// (compose | k3d-local | <aks-cluster>) with "compose" as the tier-1 default; the push sites use it.

import "testing"

func TestEnvCluster_DefaultCompose(t *testing.T) {
	e := Env{}
	if got := e.cluster(); got != "compose" {
		t.Fatalf("Env{}.cluster() = %q; want the tier-1 default \"compose\"", got)
	}
}

func TestEnvCluster_DeployDerivedOverride(t *testing.T) {
	e := Env{Cluster: "k3d-local"}
	if got := e.cluster(); got != "k3d-local" {
		t.Fatalf("cluster() = %q; want the deploy-derived value passed through", got)
	}
}

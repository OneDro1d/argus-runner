package k8srender

// INT-032: the rendered executor Deployment can NEVER be updated on a constrained cluster.
//
// The renderer sets no `strategy:`, so Kubernetes applies its default RollingUpdate — maxSurge 25%,
// maxUnavailable 25%. On the single-replica executor that onboarding creates (replicas: 1 by GENESIS,
// so the first pod writes identity.key without a race), those percentages resolve to:
//
//	maxSurge       25% of 1 = 0.25 -> rounds UP   -> 1   (a second pod MAY be created)
//	maxUnavailable 25% of 1 = 0.25 -> rounds DOWN -> 0   (the old pod may NOT be removed)
//
// So the update needs EXTRA capacity before it can free any. Measured live on example-cluster
// (2026-08-08, social-aks-v1): the Deployment carried the new digest, the new pod sat Pending with
// "0/7 nodes are available: 3 Insufficient cpu" and the autoscaler in backoff, and the OLD pod kept
// serving and kept reporting version 0.3.0+m3-iii35. The onboard reported SUCCESS throughout.
//
// Patching the strategy to maxSurge 0 / maxUnavailable 1 converged it immediately.
//
// This is not a capacity problem dressed as a bug. A cluster at capacity is the NORMAL case, and
// UC069's self-update — the feature whose entire purpose is replacing a running executor — cannot work
// while an update requires spare headroom it may never get.
//
// maxSurge 0 / maxUnavailable 1 is correct at BOTH sizes: at 1 replica the old pod goes first and the
// new one reuses its resources; at the min-3 axiom it rolls one at a time and still never asks for
// extra capacity. The cost is a brief gap at replicas: 1 — which is the right trade against "can never
// be updated", and is exactly what the executor's own outbox and the CP's re-poll are built to absorb.

import (
	"strings"
	"testing"
)

func renderedExecutorDeployment(t *testing.T, replicas int) string {
	t.Helper()
	out, err := RenderExecutor(Instance{
		ID: "probe-inst", Tier: "aks", SUTNamespace: "sut-ns",
		Image: "ghcr.io/example/suite@sha256:deadbeef", Replicas: replicas,
		CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
		ArgusConfig: "project: sut\n",
	})
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	i := strings.Index(out, "\n  name: executor\n")
	if i < 0 {
		t.Fatalf("no executor Deployment in the render:\n%s", first(out, 400))
	}
	return out
}

func first(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func TestExecutorDeployment_DeclaresAStrategyThatNeedsNoSpareCapacity(t *testing.T) {
	out := renderedExecutorDeployment(t, 1)

	if !strings.Contains(out, "maxUnavailable: 1") {
		t.Error("the executor Deployment does not set maxUnavailable: 1. With no `strategy:` Kubernetes " +
			"defaults to 25%, which on replicas: 1 rounds DOWN to 0 — so the old pod can never be removed " +
			"and the update deadlocks on any cluster without spare CPU (INT-032)")
	}
	if !strings.Contains(out, "maxSurge: 0") {
		t.Error("the executor Deployment does not set maxSurge: 0. A surge asks the cluster for capacity " +
			"BEFORE freeing any, which is precisely what a full cluster cannot give (INT-032)")
	}
}

func TestExecutorDeployment_StrategyHoldsAtTheMin3Size(t *testing.T) {
	// The the operator axiom puts k8s executors at 3. The same strategy must still make progress there, and must
	// still never ask for extra capacity.
	out := renderedExecutorDeployment(t, 3)
	if !strings.Contains(out, "maxSurge: 0") || !strings.Contains(out, "maxUnavailable: 1") {
		t.Error("the strategy must be identical at 3 replicas — rolling one at a time, no surge (INT-032)")
	}
}

func TestExecutorDeployment_StrategyIsOnTheEXECUTOR_notTheObsPlane(t *testing.T) {
	// A guard against pasting the block into the wrong Deployment: the executor is the one that must be
	// replaceable under pressure, because UC069's self-update depends on it.
	out := renderedExecutorDeployment(t, 1)
	execIdx := strings.Index(out, "\n  name: executor\n")
	stratIdx := strings.Index(out, "maxUnavailable: 1")
	if stratIdx < 0 || execIdx < 0 {
		t.Fatal("precondition: both the executor Deployment and the strategy must be present")
	}
	if stratIdx < execIdx {
		t.Error("the rollout strategy appears BEFORE the executor Deployment — it has been attached to the " +
			"wrong object")
	}
}

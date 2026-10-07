package runner

// VR10-U1 (V28-020) — THE EXECUTOR REPORTS THE CLUSTER IT WAS ONBOARDED AGAINST, ON BOTH CARRIERS.
//
// The control plane cannot derive which kubectl context reaches an instance's cluster, and without it
// the update block names none — so update.sh talks to whatever context the operator's kubectl happens
// to point at. Measured 2026-09-05 on a managed instance: that is `k3d-argus`, the script refuses
// by name, and the page never says to switch.
//
// So the value follows kit_dir hop for hop: onboarding writes it into the executor's environment, the
// executor reports it on REGISTER (once, at onboard) and on every POLL (continuously, so an instance
// registered before this existed repairs itself without a re-onboard), and the store keeps whichever
// is non-empty. Both carriers are pinned here, because the register-only half is the one that cannot
// self-heal and the poll-only half is the one that never fires on a first onboard.

import (
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func TestExecutorEnv_KubeContextRidesRegisterAndPoll(t *testing.T) {
	t.Setenv("ARGUS_KUBE_CONTEXT_HOST", "example-overlay")
	t.Setenv("ARGUS_KUBECONFIG_HOST", `C:\Users\api\.kube\example-overlay.yaml`)

	reg := registerRequestFor(FedConfig{InstanceID: "v29-memstore-aks", Tier: "managed"}, "pk", "", nil, nil, nil)
	if reg.KubeContext != "example-overlay" || reg.Kubeconfig != `C:\Users\api\.kube\example-overlay.yaml` {
		t.Errorf("register carries kube_context=%q kubeconfig=%q — an onboard is the ONLY moment the "+
			"control plane learns this for a fresh instance", reg.KubeContext, reg.Kubeconfig)
	}

	poll := pollRequestFor("0.3.29", nil, nil, nil, nil, nil, federation.SUTObservation{}, nil, nil)
	if poll.KubeContext != "example-overlay" || poll.Kubeconfig != `C:\Users\api\.kube\example-overlay.yaml` {
		t.Errorf("the poll carries kube_context=%q kubeconfig=%q — without it an instance registered "+
			"before 0.3.29 could never repair itself short of a re-onboard", poll.KubeContext, poll.Kubeconfig)
	}
}

// ⛔ ABSENT MEANS ABSENT. Compose has no cluster and a pre-0.3.29 onboard recorded none; the executor
// must report NOTHING rather than "", because `omitempty` plus the store's COALESCE(NULLIF(...)) rule
// is what stops an older executor erasing a context the control plane already knows.
func TestExecutorEnv_UnsetKubeContextIsNotReportedAsBlank(t *testing.T) {
	os.Unsetenv("ARGUS_KUBE_CONTEXT_HOST")
	os.Unsetenv("ARGUS_KUBECONFIG_HOST")
	reg := registerRequestFor(FedConfig{InstanceID: "orders-compose", Tier: "compose"}, "pk", "", nil, nil, nil)
	if reg.KubeContext != "" || reg.Kubeconfig != "" {
		t.Errorf("compose invented a cluster: %q / %q", reg.KubeContext, reg.Kubeconfig)
	}
}

// And the executor's OWN render site reads the same two variables, so the block it logs names the
// same cluster the control plane's block does.
func TestExecutorEnv_TheLoggedBlockNamesTheRecordedCluster(t *testing.T) {
	e := &Executor{Tier: "k3d", Instance: "social-k3d", KitDir: `C:\kits\social-k3d`, KubeContext: "k3d-argus"}
	block, ph := e.UpdateBlock("ghcr.io/x@sha256:aa")
	if ph {
		t.Error("a recorded context was reported as a placeholder")
	}
	// 1-SHIM: the context is assigned ONCE and read by both verbs of the new block
	if want := "KCTX='k3d-argus'"; !strings.Contains(block, want) || strings.Count(block, `--kube-context "$KCTX"`) != 2 {
		t.Errorf("the executor's block does not assign %q and pass it to both verbs:\n%s", want, block)
	}
}

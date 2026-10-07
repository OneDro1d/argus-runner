package k8srender

import (
	"strings"
	"testing"
)

// The defect this pins (2026-07-23, found live on orderservice-k3d): the instance ResourceQuota was a
// FIXED 4 CPU regardless of replica count or cluster size. At the the operator min-3-replica setting the
// executors alone claim 3 CPU (limit 1 each); loki (500m) + pushgateway (200m) + ONE promtail (200m)
// bring it to 3900m, and the quota then refuses the SECOND promtail with
//
//	exceeded quota: instance-quota, requested: limits.cpu=200m, used: 3900m, limited: 4
//
// promtail is a DaemonSet — one pod per node, each tailing only ITS node's pods. A promtail that
// cannot be scheduled means every SUT pod on that node is silently uncollected: the run still passes,
// the dashboard just has no logs for half the cluster. Nothing reports an error.

func renderFor(t *testing.T, replicas int) string {
	t.Helper()
	out, err := RenderExecutor(Instance{
		ID:           "quota-probe",
		SUTNamespace: "sut-ns",
		Image:        "example/image:test",
		CPURL:        "https://cp.example",
		WorkspaceID:  "ws",
		SUTName:      "sut",
		Replicas:     replicas,
		ArgusConfig:  "project: sut\n",
	})
	if err != nil {
		t.Fatalf("Render(replicas=%d): %v", replicas, err)
	}
	return out
}

func quotaLine(t *testing.T, manifest, key string) string {
	t.Helper()
	for _, l := range strings.Split(manifest, "\n") {
		s := strings.TrimSpace(l)
		if strings.HasPrefix(s, key+":") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(s, key+":")), `"`)
		}
	}
	t.Fatalf("quota key %q not found in manifest", key)
	return ""
}

// A render at the GENESIS replica count (1) must still be sized for the RUN. The executor scales its
// own Deployment to the active replica count (3 by default) whenever a run starts, whatever the
// rendered count was — and onboarding always renders at 1. Sizing the quota for 1 therefore refused
// the third executor replica on every run (found live on a two-node k3s, 2026-09-18: eleven
// `exceeded quota: instance-quota, requested: limits.cpu=1, used: limits.cpu=3100m, limited: 4`).
func TestQuota_genesisRenderIsSizedForTheRunScaleUp(t *testing.T) {
	genesis := renderFor(t, 1)
	run := renderFor(t, activeReplicas)
	for _, key := range []string{"limits.cpu", "limits.memory", "pods"} {
		if got, want := quotaLine(t, genesis, key), quotaLine(t, run, key); got != want {
			t.Errorf("replicas=1 renders %s = %q, but the executor scales itself to %d on a run, which needs %q",
				key, got, activeReplicas, want)
		}
	}
}

// The real bug: at 3 replicas the quota must leave room for the executors AND a promtail per node.
func TestQuota_growsWithReplicas(t *testing.T) {
	m := renderFor(t, 3)
	if got, want := quotaLine(t, m, "limits.cpu"), "6"; got != want {
		t.Errorf("replicas=3: limits.cpu = %q, want %q", got, want)
	}
	if got, want := quotaLine(t, m, "pods"), "14"; got != want {
		t.Errorf("replicas=3: pods = %q, want %q", got, want)
	}
}

// The property that actually matters, stated as arithmetic rather than as a magic number: whatever
// the replica count, the quota must fit every pod the renderer itself emits plus a promtail on each
// of a realistic number of nodes, with headroom for the one-shot pods (de-register, auth-preflight)
// that onboarding and teardown create inside this namespace.
func TestQuota_fitsEverythingTheRendererEmits(t *testing.T) {
	const (
		executorCPU    = 1000 // m, per replica
		lokiCPU        = 500
		pushgatewayCPU = 200
		promtailCPU    = 200
		nodesAllowed   = 4 // a promtail per node on a realistic local cluster
		oneShotCPU     = 500
	)
	for _, replicas := range []int{1, 2, 3, 5} {
		m := renderFor(t, replicas)
		need := replicas*executorCPU + lokiCPU + pushgatewayCPU + nodesAllowed*promtailCPU + oneShotCPU
		got := quotaLine(t, m, "limits.cpu")
		var haveCPU int
		switch got {
		case "3":
			haveCPU = 3000
		default:
			// values are whole CPUs as a plain integer string
			for _, c := range got {
				if c < '0' || c > '9' {
					t.Fatalf("replicas=%d: unexpected cpu quota %q", replicas, got)
				}
			}
			haveCPU = 0
			for _, c := range got {
				haveCPU = haveCPU*10 + int(c-'0')
			}
			haveCPU *= 1000
		}
		if haveCPU < need {
			t.Errorf("replicas=%d: quota %dm cannot fit the %dm the renderer emits", replicas, haveCPU, need)
		}
	}
}

// An explicitly supplied limit is the operator's call and must survive untouched.
func TestQuota_explicitLimitsWin(t *testing.T) {
	out, err := RenderExecutor(Instance{
		ID: "quota-probe", SUTNamespace: "sut-ns", Image: "example/image:test",
		CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
		ArgusConfig: "project: sut\n",
		Replicas:    3, CPULimit: "16", MemoryLimit: "32Gi", PodLimit: "99",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for key, want := range map[string]string{"limits.cpu": "16", "limits.memory": "32Gi", "pods": "99"} {
		if got := quotaLine(t, out, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

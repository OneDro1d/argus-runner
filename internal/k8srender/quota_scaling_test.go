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

// The memory quota is sized from the executor's own limit (--executor-memory), not a flat 1536Mi per
// replica. Found live on shop-dev-aks, 2026-10-19: with executors at 3Gi the quota stayed 9Gi and the
// THIRD replica was refused (`exceeded quota: instance-quota, requested: limits.memory=3Gi, used: 6912Mi,
// limited: 9Gi`). The default renders exactly what it did before (9216Mi = 3 x 1536 + 4608).
func TestQuota_memoryFollowsTheExecutorMemorySetting(t *testing.T) {
	for _, c := range []struct{ setting, want string }{
		{"", "9216Mi"},
		{"1536Mi", "9216Mi"},
		{"3Gi", "13824Mi"},      // 3 x 3072 + 4608
		{"4G", "16053Mi"},       // 4e9 B = 3814.7Mi, rounded UP to 3815: 3 x 3815 + 4608
		{"402653184", "5760Mi"}, // the floor, 384Mi in bytes: 3 x 384 + 4608
	} {
		out, err := RenderExecutor(Instance{
			ID: "quota-probe", SUTNamespace: "sut-ns", Image: "example/image:test",
			CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
			ArgusConfig: "project: sut\n", Replicas: 1, ExecutorMemory: c.setting,
		})
		if err != nil {
			t.Fatalf("Render(--executor-memory %q): %v", c.setting, err)
		}
		if got := quotaLine(t, out, "limits.memory"); got != c.want {
			t.Errorf("--executor-memory %q: quota limits.memory = %q, want %q", c.setting, got, c.want)
		}
	}
}

// The CPU quota is sized from the executor's own CPU limit (--executor-cpu), not a flat 1 CPU per replica: with
// executors at 2 CPU each, a quota of 6 refuses the third executor (3 x 2 = 6 leaves nothing for loki, the
// pushgateway and a promtail per node). The default renders exactly what it did before ("6" = 3 x 1 + 3 CPU).
func TestQuota_cpuFollowsTheExecutorCPUSetting(t *testing.T) {
	for _, c := range []struct {
		setting  string
		replicas int
		want     string
	}{
		{"", 1, "6"},
		{"", 3, "6"},
		{"", 5, "8"},
		{"1", 3, "6"},
		{"1000m", 3, "6"},
		{"2", 1, "9"}, // 3 x 2 + 3
		{"2", 3, "9"},
		{"2", 5, "13"},
		{"4", 3, "15"},
		{"1500m", 3, "7500m"}, // 3 x 1.5 + 3 = 7.5, not a whole CPU: millicores
		{"1.5", 3, "7500m"},
		{"0.5", 3, "4500m"},
		{"150m", 3, "3450m"}, // the floor: 3 x 0.15 + 3
		{"2500m", 3, "10500m"},
		{"1.5", 4, "9"}, // 4 x 1.5 + 3 = 9, whole again
	} {
		out, err := RenderExecutor(Instance{
			ID: "quota-probe", SUTNamespace: "sut-ns", Image: "example/image:test",
			CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
			ArgusConfig: "project: sut\n", Replicas: c.replicas, ExecutorCPU: c.setting,
		})
		if err != nil {
			t.Fatalf("Render(--executor-cpu %q, replicas=%d): %v", c.setting, c.replicas, err)
		}
		if got := quotaLine(t, out, "limits.cpu"); got != c.want {
			t.Errorf("--executor-cpu %q replicas=%d: quota limits.cpu = %q, want %q", c.setting, c.replicas, got, c.want)
		}
	}
}

// The quota always holds every executor at its limit: whatever the setting, quota - quotaReplicas x limit is the
// 3 CPU of headroom, checked against what the renderer actually emitted for the limit.
func TestQuota_cpuAlwaysHoldsTheExecutorsPlusHeadroom(t *testing.T) {
	for _, setting := range []string{"", "1", "2", "1.5", "0.5", "1500m", "3333m", "0.333"} {
		in := Instance{
			ID: "quota-probe", SUTNamespace: "sut-ns", Image: "example/image:test",
			CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
			ArgusConfig: "project: sut\n", Replicas: 1, ExecutorCPU: setting,
		}
		out, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("%q: %v", setting, err)
		}
		_, lim := execResources(t, out)
		limMilli, err := parseCPUQuantity(lim["cpu"].(string))
		if err != nil {
			t.Fatal(err)
		}
		quotaMilli, err := parseCPUQuantity(quotaLine(t, out, "limits.cpu"))
		if err != nil {
			t.Fatalf("%q: the quota %q is not a CPU quantity: %v", setting, quotaLine(t, out, "limits.cpu"), err)
		}
		if want := int64(activeReplicas)*limMilli + 3000; quotaMilli != want {
			t.Errorf("--executor-cpu %q: quota %dm, want %d x %dm + 3000m = %dm", setting, quotaMilli, activeReplicas, limMilli, want)
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

package envcapture

import "testing"

func baseCapture() Capture {
	return Capture{
		Captured: true,
		Pods: []Pod{
			{Name: "checkout-aaa", OwnerKind: "Deployment", OwnerName: "checkout", Node: "node-a", Containers: []Container{
				{Name: "checkout", Image: "img:1", ImageID: "img@sha256:aaa", RequestsCPU: "250m", RequestsMemory: "256Mi", LimitsCPU: "500m", LimitsMemory: "512Mi"},
			}},
		},
		Workloads: []Workload{{Kind: "Deployment", Name: "checkout", ReplicasDesired: 3, ReplicasReady: 3}},
		Nodes:     []Node{{Name: "node-a", AllocatableCPU: "8", AllocatableMemory: "32Gi"}},
	}
}

// TestFingerprint_StableAcrossPodAndNodeNameChange: a rolling restart gives a pod (and a
// cluster-autoscaler replacement gives a node) a brand-new NAME for the identical shape. The
// fingerprint must not move — see fingerprint.go's doc comment on why pod/node identity is
// deliberately excluded from the hashed content.
func TestFingerprint_StableAcrossPodAndNodeNameChange(t *testing.T) {
	a := baseCapture()
	b := baseCapture()
	b.Pods[0].Name = "checkout-zzz-different-hash"
	b.Nodes[0].Name = "node-replacement-xyz"

	fa, fb := Fingerprint(a), Fingerprint(b)
	if fa != fb {
		t.Errorf("fingerprint changed on a pod/node NAME change alone: %s != %s", fa, fb)
	}
}

// TestFingerprint_DiffersOnResourceChange: a bumped limit IS a different environment — the whole
// point of the feature (P3 #23) is to catch exactly this.
func TestFingerprint_DiffersOnResourceChange(t *testing.T) {
	a := baseCapture()
	b := baseCapture()
	b.Pods[0].Containers[0].LimitsMemory = "1Gi" // was 512Mi

	fa, fb := Fingerprint(a), Fingerprint(b)
	if fa == fb {
		t.Error("fingerprint identical despite a changed container memory limit")
	}
}

// TestFingerprint_DiffersOnReplicaCount: a scaled-down Deployment is a different environment even
// with every live pod's shape unchanged.
func TestFingerprint_DiffersOnReplicaCount(t *testing.T) {
	a := baseCapture()
	b := baseCapture()
	b.Workloads[0].ReplicasReady = 1 // was 3 — e.g. two pods crash-looping

	fa, fb := Fingerprint(a), Fingerprint(b)
	if fa == fb {
		t.Error("fingerprint identical despite a different ReplicasReady")
	}
}

// TestFingerprint_OrderIndependent: the API server gives no ordering guarantee across calls;
// Fingerprint must sort before hashing, or two reads of the SAME environment would spuriously
// "differ".
func TestFingerprint_OrderIndependent(t *testing.T) {
	a := baseCapture()
	a.Pods = append(a.Pods, Pod{
		Name: "worker-aaa", OwnerKind: "Deployment", OwnerName: "worker", Containers: []Container{
			{Name: "worker", Image: "img:2", RequestsCPU: "100m"},
		},
	})
	b := Capture{Captured: true, Pods: []Pod{a.Pods[1], a.Pods[0]}, Workloads: a.Workloads, Nodes: a.Nodes}

	fa, fb := Fingerprint(a), Fingerprint(b)
	if fa != fb {
		t.Errorf("fingerprint depends on slice order: %s != %s", fa, fb)
	}
}

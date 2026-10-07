package envcapture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// normContainer is one pod-container's SHAPE for fingerprinting: which workload it belongs to,
// its image + resolved digest, and its resource envelope. Deliberately NOT the pod name — a
// rolling restart gives a pod a brand-new name for the IDENTICAL shape (same owner, same image,
// same requests/limits), and a fingerprint that changed on every restart would flag "environment
// differs" for a redeploy that changed nothing an operator would call different. What DOES change
// the fingerprint: a different image/digest, different requests/limits, a different container
// count per pod, or (via normWorkload) a different replica count.
type normContainer struct {
	OwnerKind      string `json:"owner_kind"`
	OwnerName      string `json:"owner_name"`
	Container      string `json:"container"`
	Image          string `json:"image"`
	ImageID        string `json:"image_id"`
	RequestsCPU    string `json:"requests_cpu"`
	RequestsMemory string `json:"requests_memory"`
	LimitsCPU      string `json:"limits_cpu"`
	LimitsMemory   string `json:"limits_memory"`
}

// normWorkload carries desired/ready replicas — this is where a scaled-down or degraded workload
// shows up, since normContainer alone (one entry per live pod) would just have fewer entries and
// nothing would explain WHY.
type normWorkload struct {
	Kind            string `json:"kind"`
	Name            string `json:"name"`
	ReplicasDesired int    `json:"replicas_desired"`
	ReplicasReady   int    `json:"replicas_ready"`
}

// normNode carries ONLY allocatable capacity — never the node's name (a node can be replaced by
// the cluster autoscaler with an identically-sized one between two runs; that is not a resource
// change an operator needs flagged).
type normNode struct {
	AllocatableCPU    string `json:"allocatable_cpu"`
	AllocatableMemory string `json:"allocatable_memory"`
}

// Fingerprint computes a stable hash of Capture's NORMALIZED shape (see the three types above):
// sorted so map/slice ordering from the API server can never change the hash, and stripped of the
// identifiers (pod name, node name) that are expected to change across an unremarkable redeploy.
//
// It is only ever computed over a Captured=true reading — CaptureNamespace is the one caller, and
// it always fills Fingerprint before returning. A Captured=false Capture keeps Fingerprint empty
// (the zero value), which is deliberately never equal to another empty string being read as a
// match — see CompareCaptures, which refuses on Captured alone and never reaches a fingerprint
// comparison for either side.
func Fingerprint(c Capture) string {
	var containers []normContainer
	for _, p := range c.Pods {
		for _, ct := range p.Containers {
			containers = append(containers, normContainer{
				OwnerKind: p.OwnerKind, OwnerName: p.OwnerName, Container: ct.Name,
				Image: ct.Image, ImageID: ct.ImageID,
				RequestsCPU: ct.RequestsCPU, RequestsMemory: ct.RequestsMemory,
				LimitsCPU: ct.LimitsCPU, LimitsMemory: ct.LimitsMemory,
			})
		}
	}
	sort.Slice(containers, func(i, j int) bool { return lessContainer(containers[i], containers[j]) })

	var workloads []normWorkload
	for _, w := range c.Workloads {
		workloads = append(workloads, normWorkload{w.Kind, w.Name, w.ReplicasDesired, w.ReplicasReady})
	}
	sort.Slice(workloads, func(i, j int) bool {
		if workloads[i].Kind != workloads[j].Kind {
			return workloads[i].Kind < workloads[j].Kind
		}
		return workloads[i].Name < workloads[j].Name
	})

	var nodes []normNode
	for _, n := range c.Nodes {
		nodes = append(nodes, normNode{n.AllocatableCPU, n.AllocatableMemory})
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].AllocatableCPU != nodes[j].AllocatableCPU {
			return nodes[i].AllocatableCPU < nodes[j].AllocatableCPU
		}
		return nodes[i].AllocatableMemory < nodes[j].AllocatableMemory
	})

	// json.Marshal over a struct with fixed field order + already-sorted slices is deterministic —
	// no map is ever marshaled here.
	payload, _ := json.Marshal(struct {
		Containers []normContainer `json:"containers"`
		Workloads  []normWorkload  `json:"workloads"`
		Nodes      []normNode      `json:"nodes"`
	}{containers, workloads, nodes})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func lessContainer(a, b normContainer) bool {
	if a.OwnerKind != b.OwnerKind {
		return a.OwnerKind < b.OwnerKind
	}
	if a.OwnerName != b.OwnerName {
		return a.OwnerName < b.OwnerName
	}
	if a.Container != b.Container {
		return a.Container < b.Container
	}
	if a.Image != b.Image {
		return a.Image < b.Image
	}
	if a.ImageID != b.ImageID {
		return a.ImageID < b.ImageID
	}
	if a.RequestsCPU != b.RequestsCPU {
		return a.RequestsCPU < b.RequestsCPU
	}
	if a.RequestsMemory != b.RequestsMemory {
		return a.RequestsMemory < b.RequestsMemory
	}
	if a.LimitsCPU != b.LimitsCPU {
		return a.LimitsCPU < b.LimitsCPU
	}
	return a.LimitsMemory < b.LimitsMemory
}

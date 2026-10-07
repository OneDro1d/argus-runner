package envcapture

import "time"

// Container is one container's image identity and resource envelope, read from a Pod's spec
// (requests/limits) and its runtime status (the resolved image digest — ImageID, NOT the tag in
// Image, which can point at a moving tag while the pod actually runs an older pull).
type Container struct {
	Name           string `json:"name"`
	Image          string `json:"image"`
	ImageID        string `json:"image_id,omitempty"` // the resolved digest (status.containerStatuses[].imageID)
	RequestsCPU    string `json:"requests_cpu,omitempty"`
	RequestsMemory string `json:"requests_memory,omitempty"`
	LimitsCPU      string `json:"limits_cpu,omitempty"`
	LimitsMemory   string `json:"limits_memory,omitempty"`
	// RestartCount is status.containerStatuses[].restartCount at capture time. json:"-" ON PURPOSE: it is
	// read only to compute an AMQP Load step's restarts_delta from two captures; serialising it would
	// change the environment block every existing load report carries (and its fingerprint).
	RestartCount int `json:"-"`
}

// Pod is one running pod of the SUT namespace, with its owning workload resolved (Pod -> ReplicaSet
// -> Deployment, or Pod -> StatefulSet directly) so a reader sees "this is one of the checkout
// Deployment's 3 pods", not a bare hash-suffixed name.
type Pod struct {
	Name       string      `json:"name"`
	OwnerKind  string      `json:"owner_kind,omitempty"`
	OwnerName  string      `json:"owner_name,omitempty"`
	Node       string      `json:"node,omitempty"`
	Containers []Container `json:"containers,omitempty"`
}

// Workload is one Deployment or StatefulSet in the SUT namespace: what it ASKED for (desired) vs
// what Kubernetes actually has ready right now — a run that ran against 1-of-3 ready pods is a
// different environment from one that ran against 3-of-3, even with an identical scenario set.
type Workload struct {
	Kind            string `json:"kind"` // "Deployment" | "StatefulSet"
	Name            string `json:"name"`
	ReplicasDesired int    `json:"replicas_desired"`
	ReplicasReady   int    `json:"replicas_ready"`
}

// Node is one node a SUT pod actually landed on. Allocatable, not capacity: allocatable is what the
// kubelet will actually hand out (capacity minus system/kube reservations), the number that decides
// whether the SUT's own requests fit at all.
//
// Node reads are CLUSTER-SCOPED — a SUT owner's own namespace-scoped Role (SUTAccessRoleManifest)
// cannot grant them, so this section is best-effort: present when the executor's ServiceAccount
// separately carries a cluster-wide node-read right (a cluster-operator decision, not a per-SUT
// one), silently omitted (not refused) otherwise. Its ABSENCE never flips Captured to false — pods
// and workloads are the load-bearing evidence.
type Node struct {
	Name              string `json:"name"`
	AllocatableCPU    string `json:"allocatable_cpu,omitempty"`
	AllocatableMemory string `json:"allocatable_memory,omitempty"`
}

// Capture is one load run's environment reading. Captured=false means exactly one thing: nothing
// below it may be trusted, and Reason names — by exact word, e.g. "forbidden: pods in namespace X"
// — why, so a reader is never left guessing whether "no environment" meant "nothing to capture" or
// "we didn't look."
//
// Fingerprint is a stable hash of the NORMALIZED capture (fingerprint.go): it deliberately excludes
// raw pod/node NAMES (a rolling restart gives a pod a new name for the exact same resource shape —
// see fingerprint.go's doc comment) so two runs against the unchanged environment compare equal
// even if Kubernetes recycled a pod between them.
type Capture struct {
	Captured   bool       `json:"captured"`
	Reason     string     `json:"reason,omitempty"`
	Namespace  string     `json:"namespace,omitempty"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
	Pods       []Pod      `json:"pods,omitempty"`
	Workloads  []Workload `json:"workloads,omitempty"`
	Nodes      []Node     `json:"nodes,omitempty"`
	// Warnings names a section that could not be read WITHOUT failing the whole capture (today:
	// workloads, nodes) — e.g. "Deployments: forbidden". Pods succeeding is the one hard requirement
	// for Captured=true; everything else is best-effort and says so here rather than vanishing.
	Warnings    []string `json:"warnings,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
}

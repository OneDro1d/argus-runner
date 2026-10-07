// Package envcapture reads the SUT namespace's Kubernetes shape at load-run time (P3 #23, tester
// msgbus 2026-09-27): "we can't say 'it breaks under load'; we must say 'this environment, with
// these resources, breaks under this load'" (the operator). It is READ-ONLY and OPT-IN: the executor
// presents its OWN ServiceAccount token (already mounted in-cluster for UC069 self-delete —
// internal/runner/autoscale.go) against a namespace it does not own, and Kubernetes RBAC alone
// decides whether that succeeds. Argus never grants itself a right in another namespace — the SUT
// owner grants a Role/RoleBinding in THEIR OWN namespace (see SUTAccessRoleManifest,
// internal/k8srender), the same pattern msgbus's own `deploy/k8s/55-argus-test-access.yaml` uses.
//
// A capture that could not read anything says so BY NAME (Capture.Reason) — it is never silently
// empty, and a missing capture must never be read as "the same environment" (see CompareCaptures).
package envcapture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// apiClient is the seam CaptureNamespace calls through — *Client in production, an httptest.Server
// pointed at by a test-constructed *Client in tests. Kept as a concrete type (not an interface)
// because there is exactly one production implementation and the tests need nothing more than a
// different baseURL; see client.go.
type apiClient = *Client

// ForbiddenReason is the Capture.Reason of a namespace whose pods the executor may not read. Exported so a test
// of the namespace choice can follow the advice in this very text instead of a copy of it.
//
// The `outside_cluster: true` it points at is only useful where the choice of namespace honours it: a run that
// reads this namespace because a check of the run (load checks of a load run, every check of a compare run)
// belongs to an in-cluster target or to none, which is exactly where a 403 can come from.
func ForbiddenReason(namespace string) string {
	return fmt.Sprintf("forbidden: pods in namespace %s. Either the owner of that namespace grants the read (render the Role with `argus render-k8s --emit-sut-access-role`), "+
		"or, if the system under test does not run in a Kubernetes cluster, declare its test target outside the cluster (`outside_cluster: true` under test_targets in argus-config.yaml)", namespace)
}

// CaptureNamespace reads the pods, workloads (Deployments/StatefulSets) and — best-effort — nodes
// of one namespace via cl, and returns a Capture that is EITHER fully populated with
// Captured=true and a Fingerprint, OR Captured=false with a named Reason. It never returns a
// half-populated Captured=true reading: pods succeeding is the one hard requirement (see
// Capture.Warnings for the soft, best-effort sections).
//
// cl == nil means "no in-cluster credentials were available at all" (compose/local dev, or a
// SUT namespace declared with no executor running in a cluster) — the caller (argus.RunAll, via
// captureEnvironmentIfNeeded) is expected to have already turned that into its own Capture.Reason
// and never call in with a nil client; CaptureNamespace still refuses safely if it does.
func CaptureNamespace(ctx context.Context, cl apiClient, namespace string) Capture {
	now := time.Now().UTC()
	cap := Capture{Namespace: namespace, CapturedAt: &now}
	if namespace == "" {
		cap.Reason = "no SUT namespace declared"
		return cap
	}
	if cl == nil {
		cap.Reason = "no in-cluster Kubernetes credentials available"
		return cap
	}

	podBody, status, err := cl.get(ctx, "/api/v1/namespaces/"+namespace+"/pods")
	if err != nil {
		cap.Reason = "unreachable: " + err.Error()
		return cap
	}
	if status == http.StatusForbidden {
		cap.Reason = ForbiddenReason(namespace)
		return cap
	}
	if status != http.StatusOK {
		cap.Reason = fmt.Sprintf("k8s API GET pods returned status %d", status)
		return cap
	}
	var pods k8sPodList
	if err := json.Unmarshal(podBody, &pods); err != nil {
		cap.Reason = "pods response: " + err.Error()
		return cap
	}

	// ReplicaSets resolve a pod's real owner: Pod -> ReplicaSet -> Deployment. Best-effort — a SUT
	// whose Role omits replicasets still gets its pods, just with OwnerKind="ReplicaSet" (the
	// intermediate object) instead of "Deployment".
	rsOwner := map[string]k8sOwnerRef{}
	if rsBody, rsStatus, rsErr := cl.get(ctx, "/apis/apps/v1/namespaces/"+namespace+"/replicasets"); rsErr == nil && rsStatus == http.StatusOK {
		var rsList k8sReplicaSetList
		if json.Unmarshal(rsBody, &rsList) == nil {
			for _, rs := range rsList.Items {
				if len(rs.Metadata.OwnerReferences) > 0 {
					rsOwner[rs.Metadata.Name] = rs.Metadata.OwnerReferences[0]
				}
			}
		}
	}

	nodeNames := map[string]bool{}
	for _, p := range pods.Items {
		pod := Pod{Name: p.Metadata.Name, Node: p.Spec.NodeName}
		switch {
		case len(p.Metadata.OwnerReferences) == 0:
			pod.OwnerKind, pod.OwnerName = "Pod", p.Metadata.Name
		case p.Metadata.OwnerReferences[0].Kind == "ReplicaSet":
			own := p.Metadata.OwnerReferences[0]
			if dep, ok := rsOwner[own.Name]; ok {
				pod.OwnerKind, pod.OwnerName = dep.Kind, dep.Name
			} else {
				pod.OwnerKind, pod.OwnerName = "ReplicaSet", own.Name
			}
		default:
			pod.OwnerKind, pod.OwnerName = p.Metadata.OwnerReferences[0].Kind, p.Metadata.OwnerReferences[0].Name
		}
		imageIDs := map[string]string{}
		restarts := map[string]int{}
		for _, cs := range p.Status.ContainerStatuses {
			imageIDs[cs.Name] = cs.ImageID
			restarts[cs.Name] = cs.RestartCount
		}
		for _, c := range p.Spec.Containers {
			pod.Containers = append(pod.Containers, Container{
				Name: c.Name, Image: c.Image, ImageID: imageIDs[c.Name], RestartCount: restarts[c.Name],
				RequestsCPU: c.Resources.Requests.CPU, RequestsMemory: c.Resources.Requests.Memory,
				LimitsCPU: c.Resources.Limits.CPU, LimitsMemory: c.Resources.Limits.Memory,
			})
		}
		cap.Pods = append(cap.Pods, pod)
		if p.Spec.NodeName != "" {
			nodeNames[p.Spec.NodeName] = true
		}
	}
	cap.Captured = true // pods read: this is now a real (if partial) capture, never silently empty

	for _, wk := range []struct{ kind, path string }{
		{"Deployment", "/apis/apps/v1/namespaces/" + namespace + "/deployments"},
		{"StatefulSet", "/apis/apps/v1/namespaces/" + namespace + "/statefulsets"},
	} {
		body, wStatus, wErr := cl.get(ctx, wk.path)
		if wErr != nil {
			cap.Warnings = append(cap.Warnings, fmt.Sprintf("%ss: unreachable: %s", wk.kind, wErr.Error()))
			continue
		}
		if wStatus != http.StatusOK {
			cap.Warnings = append(cap.Warnings, fmt.Sprintf("%ss: k8s API returned status %d", wk.kind, wStatus))
			continue
		}
		var wl k8sWorkloadList
		if err := json.Unmarshal(body, &wl); err != nil {
			cap.Warnings = append(cap.Warnings, fmt.Sprintf("%ss: %s", wk.kind, err.Error()))
			continue
		}
		for _, w := range wl.Items {
			desired := 0
			if w.Spec.Replicas != nil {
				desired = *w.Spec.Replicas
			}
			cap.Workloads = append(cap.Workloads, Workload{
				Kind: wk.kind, Name: w.Metadata.Name,
				ReplicasDesired: desired, ReplicasReady: w.Status.ReadyReplicas,
			})
		}
	}

	// Nodes are CLUSTER-SCOPED (see Node's doc comment): best-effort, never demotes Captured.
	if len(nodeNames) > 0 {
		body, nStatus, nErr := cl.get(ctx, "/api/v1/nodes")
		switch {
		case nErr != nil:
			cap.Warnings = append(cap.Warnings, "nodes: unreachable: "+nErr.Error())
		case nStatus != http.StatusOK:
			cap.Warnings = append(cap.Warnings, fmt.Sprintf("nodes: k8s API returned status %d (namespace-scoped RBAC cannot grant node reads)", nStatus))
		default:
			var nl k8sNodeList
			if err := json.Unmarshal(body, &nl); err == nil {
				for _, n := range nl.Items {
					if !nodeNames[n.Metadata.Name] {
						continue
					}
					cap.Nodes = append(cap.Nodes, Node{
						Name:           n.Metadata.Name,
						AllocatableCPU: n.Status.Allocatable.CPU, AllocatableMemory: n.Status.Allocatable.Memory,
					})
				}
			}
		}
	}

	cap.Fingerprint = Fingerprint(cap)
	return cap
}

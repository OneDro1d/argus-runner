package envcapture

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCluster serves the small subset of the Kubernetes API CaptureNamespace reads, over a real
// httptest.Server — the brief's "fake API server (httptest) returning pods/nodes/deployments JSON
// and a 403 case", exercising the REAL Client/get() HTTP path, not a hand-rolled interface stub.
func fakeCluster(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, token: "fake-token", hc: srv.Client()}
}

func jsonHandler(t *testing.T, byPath map[string]any, forbidden map[string]bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if forbidden[r.URL.Path] {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Forbidden"}`))
			return
		}
		body, ok := byPath[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Fatalf("encode fixture for %s: %v", r.URL.Path, err)
		}
	}
}

func onePod() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name":            "checkout-7d8f9-abcde",
			"ownerReferences": []map[string]any{{"kind": "ReplicaSet", "name": "checkout-7d8f9"}},
		},
		"spec": map[string]any{
			"nodeName": "node-a",
			"containers": []map[string]any{
				{
					"name":  "checkout",
					"image": "registry.example/checkout:1.2.3",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "250m", "memory": "256Mi"},
						"limits":   map[string]any{"cpu": "500m", "memory": "512Mi"},
					},
				},
			},
		},
		"status": map[string]any{
			"containerStatuses": []map[string]any{
				{"name": "checkout", "imageID": "registry.example/checkout@sha256:deadbeef"},
			},
		},
	}
}

func fixtures() map[string]any {
	return map[string]any{
		"/api/v1/namespaces/checkout/pods": map[string]any{"items": []any{onePod()}},
		"/apis/apps/v1/namespaces/checkout/replicasets": map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{
					"name":            "checkout-7d8f9",
					"ownerReferences": []map[string]any{{"kind": "Deployment", "name": "checkout"}},
				},
			},
		}},
		"/apis/apps/v1/namespaces/checkout/deployments": map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "checkout"},
				"spec":     map[string]any{"replicas": 3},
				"status":   map[string]any{"readyReplicas": 3},
			},
		}},
		"/apis/apps/v1/namespaces/checkout/statefulsets": map[string]any{"items": []any{}},
		"/api/v1/nodes": map[string]any{"items": []any{
			map[string]any{
				"metadata": map[string]any{"name": "node-a"},
				"status":   map[string]any{"allocatable": map[string]any{"cpu": "8", "memory": "32Gi"}},
			},
		}},
	}
}

func TestCaptureNamespace_Success(t *testing.T) {
	cl := fakeCluster(t, jsonHandler(t, fixtures(), nil))
	cap := CaptureNamespace(context.Background(), cl, "checkout")

	if !cap.Captured {
		t.Fatalf("Captured = false, reason %q — want true", cap.Reason)
	}
	if len(cap.Pods) != 1 {
		t.Fatalf("Pods = %d, want 1", len(cap.Pods))
	}
	p := cap.Pods[0]
	if p.OwnerKind != "Deployment" || p.OwnerName != "checkout" {
		t.Errorf("pod owner = %s/%s, want Deployment/checkout (via ReplicaSet resolution)", p.OwnerKind, p.OwnerName)
	}
	if len(p.Containers) != 1 || p.Containers[0].ImageID != "registry.example/checkout@sha256:deadbeef" {
		t.Errorf("container/imageID = %+v", p.Containers)
	}
	if p.Containers[0].RequestsCPU != "250m" || p.Containers[0].LimitsMemory != "512Mi" {
		t.Errorf("resources not read: %+v", p.Containers[0])
	}
	if len(cap.Workloads) != 1 || cap.Workloads[0].ReplicasDesired != 3 || cap.Workloads[0].ReplicasReady != 3 {
		t.Errorf("workloads = %+v", cap.Workloads)
	}
	if len(cap.Nodes) != 1 || cap.Nodes[0].AllocatableCPU != "8" {
		t.Errorf("nodes = %+v", cap.Nodes)
	}
	if cap.Fingerprint == "" {
		t.Error("Fingerprint is empty on a successful capture")
	}
	if len(cap.Warnings) != 0 {
		t.Errorf("unexpected warnings on a fully-permitted capture: %v", cap.Warnings)
	}
}

func TestCaptureNamespace_PodsForbidden(t *testing.T) {
	cl := fakeCluster(t, jsonHandler(t, fixtures(), map[string]bool{"/api/v1/namespaces/checkout/pods": true}))
	cap := CaptureNamespace(context.Background(), cl, "checkout")

	if cap.Captured {
		t.Fatal("Captured = true on a 403 pods response")
	}
	// The refusal keeps its exact opening words (readers match "forbidden") and then says what to do, in words:
	// grant the read, or declare the target outside the cluster.
	const want = "forbidden: pods in namespace checkout"
	if !strings.HasPrefix(cap.Reason, want) {
		t.Errorf("Reason = %q, want it to open with %q", cap.Reason, want)
	}
	for _, w := range []string{"--emit-sut-access-role", "outside_cluster: true", "test_targets"} {
		if !strings.Contains(cap.Reason, w) {
			t.Errorf("Reason = %q, lacks %q", cap.Reason, w)
		}
	}
	if cap.Fingerprint != "" {
		t.Errorf("Fingerprint set on a failed capture: %q", cap.Fingerprint)
	}
}

func TestCaptureNamespace_WorkloadsForbidden_StillCaptures(t *testing.T) {
	// Pods succeed, Deployments/StatefulSets 403 (a Role that grants only pods, get/list) — the
	// brief's "nodes if permitted" leniency extends to workloads too: a partial grant still gets a
	// real (if smaller) reading, named by Warnings, never demoted to Captured=false.
	forbidden := map[string]bool{
		"/apis/apps/v1/namespaces/checkout/deployments":  true,
		"/apis/apps/v1/namespaces/checkout/statefulsets": true,
	}
	cl := fakeCluster(t, jsonHandler(t, fixtures(), forbidden))
	cap := CaptureNamespace(context.Background(), cl, "checkout")

	if !cap.Captured {
		t.Fatalf("Captured = false, reason %q — pods alone should suffice", cap.Reason)
	}
	if len(cap.Workloads) != 0 {
		t.Errorf("Workloads = %+v, want none (forbidden)", cap.Workloads)
	}
	if len(cap.Warnings) == 0 {
		t.Error("no warning recorded for the forbidden Deployments/StatefulSets reads")
	}
}

func TestCaptureNamespace_NoNamespace(t *testing.T) {
	cap := CaptureNamespace(context.Background(), nil, "")
	if cap.Captured {
		t.Fatal("Captured = true with no namespace declared")
	}
	if cap.Reason != "no SUT namespace declared" {
		t.Errorf("Reason = %q", cap.Reason)
	}
}

func TestCaptureNamespace_NoClient(t *testing.T) {
	cap := CaptureNamespace(context.Background(), nil, "checkout")
	if cap.Captured {
		t.Fatal("Captured = true with a nil client")
	}
	if cap.Reason != "no in-cluster Kubernetes credentials available" {
		t.Errorf("Reason = %q", cap.Reason)
	}
}

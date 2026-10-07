package main

import (
	"os"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/k8srender"
)

// T2.2 — MVP2-SPRINT.md: `argus render-k8s` had no way for an operator to say WHICH storage class
// or access mode the shared results volume should use. On any tier that is neither aks nor a local
// one (k3d/kind/minikube) — e.g. a managed k3s/EKS/GKE cluster — normalize() always fell through to
// `local-path` + ReadWriteOnce, which #215 caps at 1 executor replica, silently losing the min-3
// availability axiom. These tests drive the REAL render-k8s through dispatch (runRenderK8s), the
// same harness render_next_test.go already uses, so a regression at the cmdRenderK8s call site (not
// just in k8srender's own unit tests) is caught here too.

// TestRenderK8s_StorageClassLandsInPVCAndEnv (T2.2 item 1): --storage-class + --results-access-mode
// must reach BOTH the rendered PVC and the executor's ARGUS_RESULTS_ACCESS_MODE env, on a tier
// (managed) that would otherwise get local-path/ReadWriteOnce.
func TestRenderK8s_StorageClassLandsInPVCAndEnv(t *testing.T) {
	rc, out, _ := runRenderK8s(t, "--tier", "managed",
		"--storage-class", "my-rwx-class", "--results-access-mode", "ReadWriteOnce")
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	if out.StorageClass != "my-rwx-class" {
		t.Errorf("JSON storage_class = %q, want %q", out.StorageClass, "my-rwx-class")
	}
	if out.ResultsAccessMode != "ReadWriteOnce" {
		t.Errorf("JSON results_access_mode = %q, want %q", out.ResultsAccessMode, "ReadWriteOnce")
	}
	manifest, err := os.ReadFile(out.Executor)
	if err != nil {
		t.Fatalf("read rendered executor manifest: %v", err)
	}
	m := string(manifest)
	if !strings.Contains(m, "storageClassName: my-rwx-class") {
		t.Errorf("rendered manifest does not contain `storageClassName: my-rwx-class`:\n%s", m)
	}
	if !strings.Contains(m, "ARGUS_RESULTS_ACCESS_MODE") || !strings.Contains(m, "ReadWriteOnce") {
		t.Errorf("rendered manifest does not carry ReadWriteOnce on ARGUS_RESULTS_ACCESS_MODE:\n%s", m)
	}
}

// TestRenderK8s_StorageClassAloneDefaultsToReadWriteMany (T2.2 item 1, bullet 1): naming a class
// with NO --results-access-mode must default the mode to ReadWriteMany — a class that cannot do
// RWX then leaves the PVC visibly Pending, instead of silently pinning replicas to one node.
func TestRenderK8s_StorageClassAloneDefaultsToReadWriteMany(t *testing.T) {
	rc, out, _ := runRenderK8s(t, "--tier", "managed", "--storage-class", "my-rwx-class")
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	if out.ResultsAccessMode != "ReadWriteMany" {
		t.Errorf("results_access_mode = %q, want ReadWriteMany (a class was named with no explicit mode)", out.ResultsAccessMode)
	}
	manifest, err := os.ReadFile(out.Executor)
	if err != nil {
		t.Fatalf("read rendered executor manifest: %v", err)
	}
	if !strings.Contains(string(manifest), "ReadWriteMany") {
		t.Errorf("rendered manifest does not carry ReadWriteMany")
	}
}

// TestRenderK8s_BadAccessModeIsRefused (T2.2 item 1, bullet 2): any value other than ReadWriteMany /
// ReadWriteOnce must be refused with a non-zero exit BEFORE anything is written — never a manifest
// with a value the k8s API will itself reject minutes later.
func TestRenderK8s_BadAccessModeIsRefused(t *testing.T) {
	rc, out, dir := runRenderK8s(t, "--tier", "managed", "--results-access-mode", "RWX")
	if rc == exitOK {
		t.Fatalf("render-k8s accepted --results-access-mode RWX; want a refusal. output: %+v", out)
	}
	if out.Error == "" {
		t.Errorf("no error message in output: %+v", out)
	}
	if _, err := os.Stat(dir + "/executor.yaml"); err == nil {
		t.Errorf("executor.yaml was written despite the refused --results-access-mode — nothing should be written before the refusal")
	}
}

// TestRenderK8s_NoOverrideFlagsMatchesTierDefault (T2.2 item 1, bullet 3, half): with neither flag
// given, the rendered class/mode must still be exactly what StorageDefaultsFor(tier) says — the
// same agreement k8srender's own TestStorageDefaultsFor_AgreesWithRenderedManifest pins internally,
// asserted here at the CLI layer that a regression at the cmdRenderK8s call site would otherwise miss.
func TestRenderK8s_NoOverrideFlagsMatchesTierDefault(t *testing.T) {
	for _, tier := range []string{"aks", "k3d", "managed"} {
		tier := tier
		t.Run("tier="+tier, func(t *testing.T) {
			rc, out, _ := runRenderK8s(t, "--tier", tier)
			if rc != exitOK {
				t.Fatalf("render-k8s exited %d: %+v", rc, out)
			}
			wantClass, wantMode := k8srender.StorageDefaultsFor(tier)
			if out.StorageClass != wantClass {
				t.Errorf("tier %q: storage_class = %q, want %q", tier, out.StorageClass, wantClass)
			}
			if out.ResultsAccessMode != wantMode {
				t.Errorf("tier %q: results_access_mode = %q, want %q", tier, out.ResultsAccessMode, wantMode)
			}
		})
	}
}

// TestRenderK8s_ManagedTierWarnsWithoutStorageClass (T2.2 item 2): the managed-tier trap — a
// results volume that silently caps the executor at 1 replica — must be a loud warning at render
// time, on any tier that is neither aks nor local (k3d/kind/minikube), and ONLY then.
func TestRenderK8s_ManagedTierWarnsWithoutStorageClass(t *testing.T) {
	rc, out, _ := runRenderK8s(t, "--tier", "managed")
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	if !strings.Contains(out.Warning, "1 replica") && !strings.Contains(out.Warning, "--storage-class") {
		t.Errorf("tier managed with no --storage-class: warning = %q, want it to name the 1-replica cap and --storage-class as the fix", out.Warning)
	}
}

// TestRenderK8s_NoWarningOnAksK3dWithoutStorageClass (T2.2 item 2, "No warning on aks/k3d/kind"):
// those tiers already default to an RWX class, so the managed-tier trap does not apply to them.
func TestRenderK8s_NoWarningOnAksK3dWithoutStorageClass(t *testing.T) {
	for _, tier := range []string{"aks", "k3d"} {
		tier := tier
		t.Run("tier="+tier, func(t *testing.T) {
			// A kube context is supplied so the UNRELATED "no kube context recorded" warning does not
			// make this test pass for the wrong reason.
			rc, out, _ := runRenderK8s(t, "--tier", tier, "--kube-context", "test-ctx")
			if rc != exitOK {
				t.Fatalf("render-k8s exited %d: %+v", rc, out)
			}
			if out.Warning != "" {
				t.Errorf("tier %q with no --storage-class: warning = %q, want none", tier, out.Warning)
			}
		})
	}
}

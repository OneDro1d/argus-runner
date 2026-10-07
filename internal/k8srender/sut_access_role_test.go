package k8srender

import (
	"strings"
	"testing"
)

// TestSUTAccessRoleManifest_TargetsSUTNamespace_NotExecutorNamespace: this manifest is applied by
// the SUT OWNER in THEIR namespace — it must never name the executor's own namespace as the Role's
// home, which would be a manifest for Argus to apply against itself instead of the intended grant.
func TestSUTAccessRoleManifest_TargetsSUTNamespace_NotExecutorNamespace(t *testing.T) {
	in := sampleInstance()
	out := SUTAccessRoleManifest(in)
	ds := docs(t, out)
	if len(ds) != 2 {
		t.Fatalf("got %d docs, want 2 (Role, RoleBinding)", len(ds))
	}
	role, binding := ds[0], ds[1]

	if role["kind"] != "Role" {
		t.Fatalf("doc 0 kind = %v, want Role", role["kind"])
	}
	roleMeta, _ := role["metadata"].(map[string]any)
	if roleMeta["namespace"] != in.SUTNamespace {
		t.Errorf("Role namespace = %v, want the SUT namespace %q (not the executor's own %q)", roleMeta["namespace"], in.SUTNamespace, in.Namespace())
	}

	if binding["kind"] != "RoleBinding" {
		t.Fatalf("doc 1 kind = %v, want RoleBinding", binding["kind"])
	}
	bindMeta, _ := binding["metadata"].(map[string]any)
	if bindMeta["namespace"] != in.SUTNamespace {
		t.Errorf("RoleBinding namespace = %v, want the SUT namespace %q", bindMeta["namespace"], in.SUTNamespace)
	}
	subjects, _ := binding["subjects"].([]any)
	if len(subjects) != 1 {
		t.Fatalf("subjects = %v, want exactly 1", subjects)
	}
	subj, _ := subjects[0].(map[string]any)
	if subj["name"] != "argus-executor" {
		t.Errorf("subject name = %v, want argus-executor", subj["name"])
	}
	// The SUBJECT's namespace is the EXECUTOR's own namespace (where its ServiceAccount lives) —
	// the one place the executor's namespace belongs in this manifest.
	if subj["namespace"] != in.Namespace() {
		t.Errorf("subject namespace = %v, want the executor's own namespace %q", subj["namespace"], in.Namespace())
	}
}

// The artifact measurement of a certifying run reads pods (status.containerStatuses[].imageID) and
// nothing else: the grant that serves it must contain get+list on pods, be read-only, and never touch
// secrets or configmaps.
func TestSUTAccessRoleManifest_GrantsExactlyWhatArtifactMeasurementNeeds_ReadOnly(t *testing.T) {
	out := SUTAccessRoleManifest(sampleInstance())
	rules, _ := docs(t, out)[0]["rules"].([]any)
	podsRead := false
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		for _, v := range rule["verbs"].([]any) {
			switch v {
			case "get", "list":
			default:
				t.Errorf("verb %v granted: the SUT access Role must stay read-only", v)
			}
		}
		for _, res := range rule["resources"].([]any) {
			if res == "secrets" || res == "configmaps" || res == "pods/exec" || res == "pods/log" {
				t.Errorf("resource %v granted", res)
			}
			if res == "pods" {
				verbs := map[any]bool{}
				for _, v := range rule["verbs"].([]any) {
					verbs[v] = true
				}
				podsRead = verbs["get"] && verbs["list"]
			}
		}
	}
	if !podsRead {
		t.Error("the Role does not grant get+list on pods: a certifying run could not measure the running digests")
	}
	if !strings.Contains(out, "MEASURE") {
		t.Error("the manifest header does not say the grant now also serves certification measurement")
	}
}

// TestSUTAccessRoleManifest_NoNodesRule: node reads are cluster-scoped and cannot be granted by a
// namespace-scoped Role — the manifest must not claim a right it cannot actually confer.
func TestSUTAccessRoleManifest_NoNodesRule(t *testing.T) {
	in := sampleInstance()
	out := SUTAccessRoleManifest(in)
	ds := docs(t, out)
	rules, _ := ds[0]["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		resources, _ := rule["resources"].([]any)
		for _, res := range resources {
			if res == "nodes" {
				t.Fatal("manifest grants a namespaced Role the (cluster-scoped) nodes resource — this can never actually work")
			}
		}
	}
}

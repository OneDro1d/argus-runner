package k8srender

import (
	"strings"
	"testing"
)

// UC069 (W9) needs the rendered executor to carry exactly the self-delete apparatus: a dedicated
// ServiceAccount with a mounted token, the downward-API pod identity, and a namespace-scoped Role
// granting ONLY get+delete on pods — nothing cluster-scoped, no other verbs.
func TestRenderExecutor_carriesSelfDeleteRBAC(t *testing.T) {
	out, err := RenderExecutor(Instance{
		ID: "sd-probe", SUTNamespace: "sut-ns", Image: "example/img:test",
		CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
		Tier: "k3d", Replicas: 1, ArgusConfig: "project:\n  name: sut\n",
	})
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	for _, want := range []string{
		"serviceAccountName: argus-executor",
		"automountServiceAccountToken: true",
		"kind: ServiceAccount",
		"name: argus-executor",
		"kind: Role",
		"name: argus-executor-selfdelete",
		"kind: RoleBinding",
		"resources: [pods]",
		"verbs: [get, delete]",
		"resources: [deployments/scale]", // UC196 right-sizing
		"verbs: [get, patch]",
		"fieldPath: metadata.name",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered executor is missing %q", want)
		}
	}
	// The self-delete right must NOT be cluster-scoped and must not grant broader verbs.
	if strings.Contains(out, "kind: ClusterRole") {
		t.Errorf("self-delete must be a namespaced Role, never a ClusterRole")
	}
	for _, forbidden := range []string{"verbs: [\"*\"]", "resources: [\"*\"]", "list, watch, create, update, patch, delete"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("self-delete RBAC is too broad: found %q", forbidden)
		}
	}
	// The Role must sit in the INSTANCE namespace, not the SUT namespace.
	ns := Instance{ID: "sd-probe"}.Namespace()
	if !strings.Contains(out, "name: argus-executor-selfdelete, namespace: "+ns) {
		t.Errorf("Role should be in the instance namespace %q", ns)
	}
	// No ImagePullSecret was set (k3d, default): the Role must grant nothing on secrets.
	if strings.Contains(out, "resources: [secrets]") {
		t.Errorf("no ImagePullSecret was set; the Role must not reference any Secret")
	}
}

// V19-007 (withdrawing V19-006's design): the executor must NEVER hold RBAC to read ANY Secret,
// including its own image-pull Secret, even `get`-only and even resourceNames-pinned to one name.
// V19-006 granted exactly that (a narrow `get` on the one named pull secret) so the update guard
// could read the secret's `auths` map keys over the API to decide registry coverage. The orchestrator
// ruled that trade wrong: the executor runs test workloads, and the ghcr-pull token is shared across
// every tester — some of those Secrets also carry a full credential copy in their
// last-applied-configuration annotation, so "keys only" did not bound the exposure the way it read.
//
// V19-007 moves the coverage decision to RENDER time (an annotation on the executor Deployment,
// TestRenderExecutor_recordsPullSecretRegistryAnnotation below) so the guard never needs to read a
// Secret at all. This test is the regression guard for that withdrawal: the rendered Role's secrets
// grant must be BYTE-IDENTICAL to dev 1c4b90c's (none), with or without an ImagePullSecret set.
func TestRenderExecutor_grantsNothingOnSecretsEvenWithImagePullSecret(t *testing.T) {
	for _, tc := range []struct {
		name            string
		imagePullSecret string
	}{
		{"no ImagePullSecret", ""},
		{"with ImagePullSecret", "ghcr-pull"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Instance{
				ID: "sd-probe-pull", SUTNamespace: "sut-ns", Image: "example/img:test",
				CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
				Tier: "k3d", Replicas: 1, ArgusConfig: "project:\n  name: sut\n",
				ImagePullSecret: tc.imagePullSecret,
			}
			out, err := RenderExecutor(in)
			if err != nil {
				t.Fatalf("RenderExecutor: %v", err)
			}
			for _, forbidden := range []string{
				"resources: [secrets]",
				"resources: [\"secrets\"]",
			} {
				if strings.Contains(out, forbidden) {
					t.Errorf("rendered executor Role must grant NOTHING on secrets (V19-007); found %q in:\n%s", forbidden, out)
				}
			}
			// Belt-and-braces: the Role block itself must not mention the word "secrets" as a resource
			// at all — only the pull-secret annotation (a Deployment metadata string, not an RBAC rule)
			// may still say "secret".
			roleStart := strings.Index(out, "kind: Role")
			roleEnd := strings.Index(out, "kind: RoleBinding")
			if roleStart >= 0 && roleEnd > roleStart {
				roleBlock := out[roleStart:roleEnd]
				if strings.Contains(roleBlock, "secrets") {
					t.Errorf("Role block references secrets: %s", roleBlock)
				}
			}
		})
	}
}

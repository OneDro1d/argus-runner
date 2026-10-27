package k8srender

import (
	"regexp"
	"strings"
	"testing"
)

func renderForProbe(t *testing.T) string {
	t.Helper()
	out, err := RenderExecutor(Instance{
		ID: "pc-probe", SUTNamespace: "sut-ns", Image: "example/img:test",
		CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
		Tier: "k3d", Replicas: 1, ArgusConfig: "project:\n  name: sut\n",
	})
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	return out
}

// the precondition probe's rights are in the executor's OWN namespace Role and nowhere else:
// pods create/list (get+delete were already there) and networkpolicies create/get/list/delete. Nothing
// cluster-scoped, nothing on secrets, and no right named for the system-under-test namespace.
func TestRenderExecutor_RoleCarriesTheProbesRightsInItsOwnNamespaceOnly(t *testing.T) {
	out := renderForProbe(t)
	for _, want := range []string{
		"apiGroups: [networking.k8s.io]",
		"resources: [networkpolicies]",
		"verbs: [create, get, list, delete]",
		"verbs: [create, list]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered executor Role is missing %q", want)
		}
	}
	if strings.Contains(out, "kind: ClusterRole") {
		t.Error("the probe's rights must be a namespaced Role")
	}
	if strings.Contains(out, "resources: [secrets]") {
		t.Error("the Role must grant nothing on secrets")
	}
	// the Role and its binding are in the instance namespace, never the SUT namespace
	if strings.Contains(out, "name: argus-executor-selfdelete, namespace: sut-ns") {
		t.Error("the Role must not sit in the SUT namespace")
	}
	// no wildcard verbs on the new rule
	if strings.Contains(out, `networkpolicies]
    verbs: ["*"]`) {
		t.Error("networkpolicies verbs are a wildcard")
	}
}

// The allow-same-namespace policy excludes the probe's isolated listener, so the namespace's default-deny alone
// isolates it and the ingress probe is meaningful on a rendered instance. Every other pod is still admitted.
func TestRenderExecutor_AllowSameNamespaceExcludesOnlyTheProbeListener(t *testing.T) {
	out := renderForProbe(t)
	re := regexp.MustCompile(`(?s)name: allow-same-namespace.*?policyTypes: \[Ingress\]\s+ingress:\s+- from:\s+- podSelector: \{\}`)
	m := re.FindString(out)
	if m == "" {
		t.Fatalf("allow-same-namespace no longer admits same-namespace pods:\n%s", out)
	}
	if !strings.Contains(m, "key: argus.onedroid.ai/probe-isolated, operator: DoesNotExist") {
		t.Errorf("allow-same-namespace does not exclude the probe listener:\n%s", m)
	}
}

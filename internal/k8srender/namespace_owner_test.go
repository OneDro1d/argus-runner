package k8srender

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// #213 — the owner of an instance namespace (argus.onedroid.ai/namespace-owner, AC-D9) is set ONLY
// by `kubectl annotate`, never inside a manifest anyone `kubectl apply`s. A key that is in an applied
// manifest enters last-applied-configuration, and the next apply of a Namespace document without it
// DELETES it (kubectl's three-way merge) — which is how onboarding stripped the operator's mark and
// teardown then deleted the operator's namespace and RoleBindings.
//
// So the rendered Namespace carries no owner annotation (the render is applied by onboarding and
// `create`d by a human; neither may bring the key into last-applied), and teardown's defence in
// depth — keep an unmarked namespace that holds a RoleBinding Argus did not create — needs the
// exact list of RoleBindings Argus DOES create, which lives in onboarding/lib/namespace-owner.sh.
// These two tests pin both sides.

func renderedNamespaceDoc(t *testing.T, manifest string) map[string]any {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if d["kind"] == "Namespace" {
			return d
		}
	}
	t.Fatalf("no Namespace document in the rendered executor")
	return nil
}

func TestRenderExecutor_NamespaceCarriesNoOwnerAnnotation(t *testing.T) {
	for _, tier := range []string{"k3d", "aks"} {
		in := sampleInstance()
		in.Tier = tier
		out, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("RenderExecutor(%s): %v", tier, err)
		}
		md, _ := renderedNamespaceDoc(t, out)["metadata"].(map[string]any)
		if ann, ok := md["annotations"].(map[string]any); ok {
			if v, has := ann["argus.onedroid.ai/namespace-owner"]; has {
				t.Errorf("tier %s: the rendered Namespace carries namespace-owner=%v. An applied manifest must never "+
					"carry the owner: it enters last-applied-configuration and the next owner-less apply deletes it (#213)", tier, v)
			}
		}
	}
}

// argusRoleBindingsInLib reads NS_ARGUS_ROLEBINDINGS="…" out of the shell library teardown uses.
func argusRoleBindingsInLib(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../onboarding/lib/namespace-owner.sh")
	if err != nil {
		t.Fatalf("read namespace-owner.sh: %v", err)
	}
	m := regexp.MustCompile(`(?m)^NS_ARGUS_ROLEBINDINGS="([^"]*)"`).FindSubmatch(b)
	if m == nil {
		t.Fatalf(`onboarding/lib/namespace-owner.sh has no NS_ARGUS_ROLEBINDINGS="…" line — teardown cannot tell ` +
			`an Argus RoleBinding from an operator's (#213)`)
	}
	names := strings.Fields(string(m[1]))
	sort.Strings(names)
	return names
}

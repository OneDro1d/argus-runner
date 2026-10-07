package k8srender

// VR10-U1 (V28-020) — THE KUBE CONTEXT REACHES THE EXECUTOR THE WAY THE KIT FOLDER DOES.
//
// The update block has to name the cluster, and BOTH render sites have to name the same one. The
// control plane learns it from the register/poll payload; the executor can only report what its own
// environment was told, and on a k8s tier that environment is this rendered pod spec. So the two new
// values follow kit_dir hop for hop — onboarding exports them, argus_render passes them in, this
// block writes them into the pod, and the executor reads them back out.
//
// ⛔ OMITTED WHEN EMPTY, INDIVIDUALLY. A present-but-empty ARGUS_KUBE_CONTEXT_HOST makes the
// executor report "" as an ANSWER, and the control plane cannot tell that from "this instance was
// onboarded before the context was recorded" — which is precisely the distinction the whole legacy
// placeholder rule rests on. Absent means absent (the absence-is-not-health trap).

import (
	"strings"
	"testing"
)

func TestFoldersEnvBlock_CarriesTheKubeContextAndKubeconfig(t *testing.T) {
	got := foldersEnvBlock(`C:\tmp\product`, `C:\tmp\test`, "DESKTOP-EXAMPLE", `C:\Users\api\argus-kits\social-k3d`,
		"k3d-argus", `C:\Users\api\.kube\config`)
	for _, want := range []string{
		"ARGUS_KUBE_CONTEXT_HOST", `"k3d-argus"`,
		"ARGUS_KUBECONFIG_HOST", `C:\\Users\\api\\.kube\\config`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Compose has no cluster, and an instance onboarded before 0.3.29 has no recorded one. Both must
// leave the pod env without the key at all.
func TestFoldersEnvBlock_OmitsTheKubeKeysWhenUnknown(t *testing.T) {
	got := foldersEnvBlock("", "", "", `C:\kits\k-0`, "", "")
	for _, unwanted := range []string{"ARGUS_KUBE_CONTEXT_HOST", "ARGUS_KUBECONFIG_HOST"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s is present although nothing was recorded — a blank value is an ANSWER the "+
				"control plane cannot tell from silence:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "ARGUS_KIT_DIR_HOST") {
		t.Errorf("a known value must still be reported:\n%s", got)
	}
	// A context WITHOUT a kubeconfig is the ordinary case (the operator's default kubeconfig holds
	// the context). The two are dropped independently, like every other key in this block.
	only := foldersEnvBlock("", "", "", "", "example-overlay", "")
	if !strings.Contains(only, "ARGUS_KUBE_CONTEXT_HOST") || strings.Contains(only, "ARGUS_KUBECONFIG_HOST") {
		t.Errorf("the two kube keys are not dropped independently:\n%s", only)
	}
}

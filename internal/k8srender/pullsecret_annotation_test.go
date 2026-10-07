package k8srender

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/registryhost"
)

// V19-007: with V19-006's Secret-read RBAC withdrawn (see selfdelete_rbac_test.go), the update guard
// (internal/runner/updateguard.go) can no longer ask a Secret which registry it covers. Render time
// already knows the answer — the pull secret it wires in (ImagePullSecret) was made for the registry
// of the image THIS render pins the executor to — so render records it as an annotation on the
// executor Deployment, which the guard reads back from the Deployment it already has RBAC for.
func executorDeployment(t *testing.T, manifest string) map[string]any {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if d["kind"] == "Deployment" {
			meta, _ := d["metadata"].(map[string]any)
			if meta != nil && meta["name"] == "executor" {
				return d
			}
		}
	}
	t.Fatal("no executor Deployment found in rendered manifest")
	return nil
}

func TestRenderExecutor_recordsPullSecretRegistryAnnotation(t *testing.T) {
	in := sampleInstance()
	in.Image = "ghcr.io/onedro1d/argus-runner:m3-dev"
	in.ImagePullSecret = "ghcr-pull"
	out, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	dep := executorDeployment(t, out)
	meta, _ := dep["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		t.Fatal("executor Deployment carries no annotations; want the pull-secret-registries record")
	}
	got, _ := ann[registryhost.PullSecretRegistriesAnnotation].(string)
	want := registryhost.HostOfImage(in.Image)
	if got != want {
		t.Errorf("%s annotation = %q, want %q (the registry of the rendered image)", registryhost.PullSecretRegistriesAnnotation, got, want)
	}
}

// An the private registry-hosted image renders an the private registry annotation — proves the annotation tracks the IMAGE's registry,
// not a hardcoded GHCR assumption.
func TestRenderExecutor_recordsPullSecretRegistryAnnotation_acr(t *testing.T) {
	in := sampleInstance()
	in.Image = "registry.example.com/argus/runner:m3-dev"
	in.ImagePullSecret = "acr-pull"
	out, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	dep := executorDeployment(t, out)
	meta, _ := dep["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	got, _ := ann[registryhost.PullSecretRegistriesAnnotation].(string)
	if got != "registry.example.com" {
		t.Errorf("%s annotation = %q, want %q", registryhost.PullSecretRegistriesAnnotation, got, "registry.example.com")
	}
}

// Local tiers with NO ImagePullSecret (k3d importing the image) get NO annotation at all — nothing
// for the guard to misread as coverage that was never established.
func TestRenderExecutor_noPullSecretRegistryAnnotationWithoutImagePullSecret(t *testing.T) {
	in := sampleInstance()
	in.ImagePullSecret = ""
	out, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	dep := executorDeployment(t, out)
	meta, _ := dep["metadata"].(map[string]any)
	if ann, ok := meta["annotations"].(map[string]any); ok {
		if _, has := ann[registryhost.PullSecretRegistriesAnnotation]; has {
			t.Errorf("no ImagePullSecret was set; the Deployment must carry no %s annotation", registryhost.PullSecretRegistriesAnnotation)
		}
	}
}

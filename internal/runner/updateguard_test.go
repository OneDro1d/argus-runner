package runner

import (
	"strings"
	"testing"
)

// VR5-U2 / VR5-T2 (V19-005) — the update button must not destroy the executor it was asked to update.
//
// Measured: pressing "Update this executor" on a k3d instance took it down. The patch always succeeds;
// the rollout is what fails, and at maxSurge:0 / replicas:1 the OLD pod is terminated FIRST. So an
// image the cluster cannot obtain leaves no executor and nothing able to revert.
//
// VR5-U3 (V19-006) extended this: a pod that references SOME pull secret was treated as "can pull
// ANYTHING". Released executors come from GHCR; an executor onboarded earlier may carry a secret that
// covers the private registry only. guardImageObtainable now has to know which REGISTRY a referenced secret covers.
//
// V19-007 withdraws V19-006's own answer (the guard reading the Secret's `auths` keys over the API,
// which required granting the executor `get` on its own pull secret) and replaces it with a
// render-time annotation on the executor's Deployment (registryhost.PullSecretRegistriesAnnotation) —
// the fourth parameter below is now that annotation's parsed content, never a Secret read's result.

const (
	running      = "ghcr.io/onedro1d/argus-runner@sha256:aaa"
	newer        = "ghcr.io/onedro1d/argus-runner@sha256:bbb"
	runningOnACR = "registry.example.com/argus/runner@sha256:ccc"
	newerOnGHCR  = "ghcr.io/onedro1d/argus-runner@sha256:ddd"
	ghcrHost     = "ghcr.io"
	acrHost      = "registry.example.com"
)

// THE ORIGINAL DEFECT (V19-005). k3d has no pull secret (local tiers import instead), so a new image
// is unobtainable.
func TestGuardImageObtainable_RefusesANewImageWithNoPullSecret(t *testing.T) {
	err := guardImageObtainable(running, newer, nil, nil)
	if err == nil {
		t.Fatal("the update was allowed to proceed on a cluster that cannot obtain the image.\n" +
			"  At maxSurge:0/replicas:1 the running pod is terminated BEFORE the new one is created,\n" +
			"  so this is the state that leaves a k3d instance with no executor at all — and no\n" +
			"  component left to roll it back. That is V19-005's first surface.")
	}
	// The operator's next action must be in the message: nothing inside the cluster can fix this.
	if !strings.Contains(err.Error(), "import") {
		t.Errorf("the refusal does not name the remedy (a node-store import): %q", err.Error())
	}
}

// The managed tier CAN pull when its secret actually covers the requested image's registry, so the
// same request must be allowed — the guard must not become a blanket ban that breaks the tier where
// updating works.
func TestGuardImageObtainable_AllowsWhenThePodCanPull(t *testing.T) {
	covered := map[string]bool{ghcrHost: true}
	if err := guardImageObtainable(running, newer, []string{"ghcr-pull"}, covered); err != nil {
		t.Fatalf("an executor whose pull secret covers the requested image's registry must be allowed to re-image: %v", err)
	}
}

// Re-requesting the image already running is a no-op rollout and cannot strand anything. Refusing it
// would break the idempotent re-request the control plane may legitimately make. True even with no
// covering secret at all — a no-op never needs to pull anything new.
func TestGuardImageObtainable_AllowsTheImageAlreadyRunning(t *testing.T) {
	if err := guardImageObtainable(running, running, nil, nil); err != nil {
		t.Fatalf("re-requesting the running image must be a no-op, not a refusal: %v", err)
	}
}

// An empty request is a control-plane bug; acting on it would patch the container to "".
func TestGuardImageObtainable_RefusesAnEmptyRequest(t *testing.T) {
	if err := guardImageObtainable(running, "", nil, nil); err == nil {
		t.Error("an empty image reference was accepted — the patch would blank the container's image")
	}
}

// THE NEW DEFECT (V19-006). An executor onboarded against the private registry, whose pod spec references only an
// the private registry-covering pull secret, is asked to move to the now-released GHCR image. The old guard saw
// hasPullSecret=true and allowed it; the cluster still cannot pull from GHCR with an the private registry credential.
// The running pod is terminated FIRST (maxSurge:0) — this is the exact outage the guard exists to
// prevent, just on a registry move instead of "no secret at all".
func TestGuardImageObtainable_RefusesCrossRegistryWhenOnlyTheOldRegistryIsCovered(t *testing.T) {
	covered := map[string]bool{acrHost: true} // the secret covers the private registry, NOT ghcr.io
	err := guardImageObtainable(runningOnACR, newerOnGHCR, []string{"acr-pull"}, covered)
	if err == nil {
		t.Fatal("the update was allowed to proceed onto a registry no referenced pull secret covers.\n" +
			"  Referencing SOME secret is not the same as being able to pull THIS image: an\n" +
			"  the private registry-only credential cannot authenticate to ghcr.io, and the running pod is terminated\n" +
			"  before the new one is found unobtainable (maxSurge:0). That is V19-006.")
	}
	msg := err.Error()
	for _, want := range []string{acrHost, ghcrHost} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name both registries (%q missing): %q", want, msg)
		}
	}
	// This is a DIFFERENT failure than "no secret at all" — it must not claim there are no
	// imagePullSecrets, and it must not reuse the "no secret" remedy wording (import).
	if strings.Contains(msg, "carries no") {
		t.Errorf("cross-registry refusal must not claim the pod has no imagePullSecrets: %q", msg)
	}
	// The remedy: create the matching secret and add it to the Deployment.
	if !strings.Contains(msg, "kubectl") || !strings.Contains(msg, "create secret") {
		t.Errorf("the refusal does not name the remedy (create a covering pull secret): %q", msg)
	}
}

// The other half of V19-006: when a covering secret for the NEW registry exists (alongside, or
// instead of, the old one), the cross-registry move must be allowed.
func TestGuardImageObtainable_AllowsCrossRegistryWhenTheNewRegistryIsCovered(t *testing.T) {
	covered := map[string]bool{acrHost: true, ghcrHost: true}
	if err := guardImageObtainable(runningOnACR, newerOnGHCR, []string{"acr-pull", "ghcr-pull"}, covered); err != nil {
		t.Fatalf("a pull secret covering the requested image's registry must allow the move: %v", err)
	}
}

// V19-007: a Deployment that references a pull secret but carries NO pull-secret-registries
// annotation at all (rendered before V19-007, or rendered with an ImagePullSecret that predates this
// guard) must refuse a CROSS-registry move exactly like "not listed" — an absent record is not proof
// of coverage, and the guard must never fall back to "a secret was referenced, so allow anything".
func TestGuardImageObtainable_CrossRegistryWithNoAnnotationRecordedRefuses(t *testing.T) {
	err := guardImageObtainable(runningOnACR, newerOnGHCR, []string{"acr-pull"}, map[string]bool{})
	if err == nil {
		t.Fatal("a cross-registry move with no pull-secret-registries annotation at all was allowed")
	}
	if !strings.Contains(err.Error(), ghcrHost) {
		t.Errorf("refusal should name the registry that needs covering: %q", err.Error())
	}
}

// V19-007: an annotation that records a DIFFERENT registry — neither the one currently running nor
// the one requested — must refuse the same way as no annotation at all. This is the guard's OWN
// Deployment read finding real content that simply does not vouch for this host; it must not be
// confused with "could not check" (an error) or with the current registry always being fine.
func TestGuardImageObtainable_AnnotationForAThirdRegistryRefuses(t *testing.T) {
	covered := map[string]bool{"quay.io": true} // recorded, but neither the private registry (current) nor GHCR (requested)
	err := guardImageObtainable(runningOnACR, newerOnGHCR, []string{"acr-pull"}, covered)
	if err == nil {
		t.Fatal("an annotation covering an unrelated registry was treated as covering the requested one")
	}
	msg := err.Error()
	for _, want := range []string{acrHost, ghcrHost} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name both registries (%q missing): %q", want, msg)
		}
	}
}

func TestRegistryHostOfImage(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/onedro1d/argus-runner@sha256:aaa":      "ghcr.io",
		"registry.example.com/argus/x@sha256:bbb":       "registry.example.com",
		"busybox@sha256:ccc":                            "docker.io",
		"busybox:latest":                                "docker.io",
		"example/onedroid-argus-execution-plane:m3-dev": "docker.io",
		"localhost:5000/foo:tag":                        "localhost:5000",
		"localhost/foo:tag":                             "localhost",
	}
	for ref, want := range cases {
		if got := registryHostOfImage(ref); got != want {
			t.Errorf("registryHostOfImage(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestNormalizeRegistryHost(t *testing.T) {
	cases := map[string]string{
		"ghcr.io":                     "ghcr.io",
		"GHCR.IO":                     "ghcr.io",
		"https://ghcr.io":             "ghcr.io",
		"ghcr.io:443":                 "ghcr.io",
		"https://ghcr.io:443/":        "ghcr.io",
		"docker.io":                   "docker.io",
		"index.docker.io":             "docker.io",
		"registry-1.docker.io":        "docker.io",
		"https://index.docker.io/v1/": "docker.io",
		"registry.example.com":        "registry.example.com",
	}
	for in, want := range cases {
		if got := normalizeRegistryHost(in); got != want {
			t.Errorf("normalizeRegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

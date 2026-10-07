package k8srender

import (
	"strings"
	"testing"
)

// StorageDefaultsFor exists so `argus preflight` can tell an operator which storage class their
// cluster needs BEFORE anything is created. Its whole value depends on it agreeing with what the
// renderer actually writes — a preflight that reports a different class from the one the manifest
// asks for is worse than no preflight, because it produces a confident green and then a Pending
// claim.
//
// ⛔ So this does not test the accessor against a table of expected values. A table would be a
// SECOND home for the mapping, and it would agree with the accessor while both drifted away from
// the renderer. It tests the accessor against the RENDERED MANIFEST.
func TestStorageDefaultsFor_AgreesWithRenderedManifest(t *testing.T) {
	// Every tier the product recognises, plus the two that matter most: the empty string and a
	// value nobody defined. Those are the ones that fall through, and the fallthrough is silent.
	for _, tier := range []string{"aks", "k3d", "kind", "minikube", "", "eks", "gke", "k8s-dev", "managed", "typo"} {
		tier := tier
		t.Run("tier="+tier, func(t *testing.T) {
			wantClass, wantMode := StorageDefaultsFor(tier)

			in := Instance{
				ID:           "pf-probe",
				SUTNamespace: "pf-sut",
				CPURL:        "https://cp.example.invalid",
				ArgusConfig:  "project:\n  name: pf-probe\n",
				Tier:         tier,
				Image:        "example.invalid/argus/execution-plane@sha256:" + strings.Repeat("a", 64),
			}
			manifest, err := RenderExecutor(in)
			if err != nil {
				t.Fatalf("RenderExecutor(tier=%q): %v", tier, err)
			}

			if !strings.Contains(manifest, "storageClassName: "+wantClass) {
				t.Errorf("StorageDefaultsFor(%q) says class %q, but the rendered manifest does not "+
					"contain `storageClassName: %s`. The preflight would promise one class and the "+
					"deploy would ask for another.", tier, wantClass, wantClass)
			}
			if !strings.Contains(manifest, wantMode) {
				t.Errorf("StorageDefaultsFor(%q) says access mode %q, which does not appear in the "+
					"rendered manifest", tier, wantMode)
			}
		})
	}
}

// TestStorageDefaultsFor_EmptyTierIsNotK3d pins the asymmetry that the accessor's doc comment
// warns about, because it is exactly the kind of thing a later refactor "tidies up".
//
// normalize() reads the tier for STORAGE before it defaults the tier field itself to k3d. So an
// empty tier gets local-path/ReadWriteOnce for storage while every other part of the instance
// behaves as k3d. Quietly promoting the storage branch to k3d would bind the claim to `argus-rwx`
// — a class that exists only after onboard.sh installs it — leaving the PVC Pending on every
// cluster without it.
func TestStorageDefaultsFor_EmptyTierIsNotK3d(t *testing.T) {
	emptyClass, emptyMode := StorageDefaultsFor("")
	k3dClass, _ := StorageDefaultsFor("k3d")

	if emptyClass == k3dClass {
		t.Fatalf("an empty tier now resolves to %q, the same as k3d. That is a behaviour change, "+
			"not a tidy-up: it binds the claim to a class that only exists after onboard.sh "+
			"installs it.", emptyClass)
	}
	if emptyClass != "local-path" || emptyMode != "ReadWriteOnce" {
		t.Errorf("empty tier = (%q, %q), want (local-path, ReadWriteOnce) — the documented fallback",
			emptyClass, emptyMode)
	}
}

package k8srender

// obs_persistence_test.go — / a per-instance Loki and Pushgateway, and
// the shared Loki, must keep their data across a pod recreate. Seen live on `documenso` (2026-10-05):
// the pods were recreated and the morning's runs were gone from both. Every assertion reads the
// PARSED manifest, never a substring of it, so a volume that merely appears in a comment cannot pass.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// obsDoc returns the document of the given kind and name, or fails.
func obsDoc(t *testing.T, manifest, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs(t, manifest) {
		if k, _ := d["kind"].(string); k == kind && nameOf(d) == name {
			return d
		}
	}
	t.Fatalf("no %s %q in the rendered manifest", kind, name)
	return nil
}

func podSpecOf(d map[string]any) map[string]any {
	spec, _ := d["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	ps, _ := tmpl["spec"].(map[string]any)
	return ps
}

// volumeOf returns the pod's volume of that name, or nil.
func volumeOf(d map[string]any, name string) map[string]any {
	vols, _ := podSpecOf(d)["volumes"].([]any)
	for _, v := range vols {
		if vm, _ := v.(map[string]any); vm["name"] == name {
			return vm
		}
	}
	return nil
}

func containerOf(d map[string]any) map[string]any {
	cs, _ := podSpecOf(d)["containers"].([]any)
	c, _ := cs[0].(map[string]any)
	return c
}

// assertClaim checks the volume `vol` of Deployment d is the PVC `claim`, that the PVC exists in the
// manifest with the wanted class, size and the ReadWriteOnce access mode.
func assertClaim(t *testing.T, manifest string, d map[string]any, vol, claim, ns, wantClass, wantSize string) {
	t.Helper()
	v := volumeOf(d, vol)
	if v == nil {
		t.Fatalf("%s has no volume %q", nameOf(d), vol)
	}
	if _, bad := v["emptyDir"]; bad {
		t.Fatalf("%s's %s volume is still an emptyDir — its data dies with the pod: %+v", nameOf(d), vol, v)
	}
	pvcRef, _ := v["persistentVolumeClaim"].(map[string]any)
	if pvcRef["claimName"] != claim {
		t.Fatalf("%s's %s volume = %+v, want persistentVolumeClaim %s", nameOf(d), vol, v, claim)
	}
	pvc := obsDoc(t, manifest, "PersistentVolumeClaim", claim)
	md, _ := pvc["metadata"].(map[string]any)
	if md["namespace"] != ns {
		t.Errorf("PVC %s namespace = %v, want %s", claim, md["namespace"], ns)
	}
	spec, _ := pvc["spec"].(map[string]any)
	modes, _ := spec["accessModes"].([]any)
	if len(modes) != 1 || modes[0] != "ReadWriteOnce" {
		t.Errorf("PVC %s accessModes = %v, want [ReadWriteOnce] (a file share is what Loki's docs warn against; one replica)", claim, modes)
	}
	// wantClass "" means the cluster's DEFAULT StorageClass: the claim must carry NO storageClassName
	// at all (an explicit empty string would mean "no class", i.e. never dynamically provisioned).
	gotClass, hasClass := spec["storageClassName"]
	if wantClass == "" {
		if hasClass {
			t.Errorf("PVC %s carries storageClassName %#v, want the field ABSENT so the cluster default class is used", claim, gotClass)
		}
	} else if gotClass != wantClass {
		t.Errorf("PVC %s storageClassName = %v, want %s", claim, gotClass, wantClass)
	}
	res, _ := spec["resources"].(map[string]any)
	req, _ := res["requests"].(map[string]any)
	if req["storage"] != wantSize {
		t.Errorf("PVC %s requests.storage = %v, want %s", claim, req["storage"], wantSize)
	}
}

// assertRecreate checks the Deployment cannot ask for two pods on one ReadWriteOnce volume: the type is
// Recreate and no rollingUpdate block is left to be merged in from the live object.
func assertRecreate(t *testing.T, d map[string]any) {
	t.Helper()
	spec, _ := d["spec"].(map[string]any)
	st, _ := spec["strategy"].(map[string]any)
	if st["type"] != "Recreate" {
		t.Errorf("%s strategy = %+v, want type Recreate (a surge pod cannot attach a ReadWriteOnce volume the old pod holds)", nameOf(d), st)
	}
	if v, present := st["rollingUpdate"]; present && v != nil {
		t.Errorf("%s strategy.rollingUpdate = %+v, want absent or null", nameOf(d), v)
	}
	if _, present := st["rollingUpdate"]; !present {
		t.Errorf("%s strategy has no `rollingUpdate: null`: on an apply over a live Deployment the old rollingUpdate block would stay and the API server refuses Recreate with it", nameOf(d))
	}
}

// assertRunsAs checks the pod writes the volume as the image's own user, not root.
func assertRunsAs(t *testing.T, d map[string]any, uid int) {
	t.Helper()
	sc, _ := podSpecOf(d)["securityContext"].(map[string]any)
	for _, k := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
		if sc[k] != uid {
			t.Errorf("%s pod securityContext.%s = %v, want %d (the image's user; fsGroup makes the fresh volume writable by it)", nameOf(d), k, sc[k], uid)
		}
	}
	if sc["runAsNonRoot"] != true {
		t.Errorf("%s pod securityContext.runAsNonRoot = %v, want true", nameOf(d), sc["runAsNonRoot"])
	}
}

func assertPushgatewayPersists(t *testing.T, manifest, ns, wantClass string) {
	t.Helper()
	d := obsDoc(t, manifest, "Deployment", "pushgateway")
	c := containerOf(d)
	var args []string
	rawArgs, _ := c["args"].([]any)
	for _, a := range rawArgs {
		s, _ := a.(string)
		args = append(args, s)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--persistence.file=/pushgateway/data") || !strings.Contains(joined, "--persistence.interval=1m") {
		t.Errorf("pushgateway args = %q, want --persistence.file=/pushgateway/data and --persistence.interval=1m", joined)
	}
	assertClaim(t, manifest, d, "data", "pushgateway-data", ns, wantClass, obsPushgatewayVolumeSize)
	mounted := false
	rawMounts, _ := c["volumeMounts"].([]any)
	for _, m := range rawMounts {
		mm, _ := m.(map[string]any)
		if mm["name"] == "data" && mm["mountPath"] == "/pushgateway" {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("pushgateway does not mount its data volume at /pushgateway: %+v", c["volumeMounts"])
	}
	assertRecreate(t, d)
	assertRunsAs(t, d, 65534)
}

// (amended): aks -> managed-csi; EVERY other tier -> no storageClassName, so the cluster's
// own default StorageClass binds the claim ("" = absent). A named class like local-path does not exist on
// kind/minikube ("standard"), EKS or GKE, and a claim naming it sits Pending forever.
var obsClassByTier = []struct{ tier, class string }{
	{"", ""},
	{"k3d", ""},
	{"kind", ""},
	{"minikube", ""},
	{"aks", "managed-csi"},
	{"AKS", "managed-csi"},
	{"eks", ""},
	{"gke", ""},
	{"k8s-dev", ""},
	{"managed", ""},
}

func TestRenderObs_bundled_lokiAndPushgatewayDataAreRWOClaims(t *testing.T) {
	for _, c := range obsClassByTier {
		t.Run(c.tier, func(t *testing.T) {
			in := sampleInstance()
			in.Tier = c.tier
			got, err := RenderObs(in, cfgJSON(t))
			if err != nil {
				t.Fatal(err)
			}
			ns := in.Namespace()
			loki := obsDoc(t, got, "Deployment", "loki")
			assertClaim(t, got, loki, "data", "loki-data", ns, c.class, obsLokiVolumeSize)
			assertRecreate(t, loki)
			assertRunsAs(t, loki, 10001)
			assertPushgatewayPersists(t, got, ns, c.class)
		})
	}
}

func TestRenderObs_shared_pushgatewayPersistsAndNoLoki(t *testing.T) {
	for _, c := range obsClassByTier {
		t.Run(c.tier, func(t *testing.T) {
			in := sharedInstance()
			in.Tier = c.tier
			got, err := RenderObs(in, cfgJSON(t))
			if err != nil {
				t.Fatal(err)
			}
			assertPushgatewayPersists(t, got, in.Namespace(), c.class)
			for _, d := range docs(t, got) {
				if nameOf(d) == "loki-data" || (d["kind"] == "Deployment" && nameOf(d) == "loki") {
					t.Errorf("--obs=shared renders a per-instance Loki object: %s %s", d["kind"], nameOf(d))
				}
			}
		})
	}
}

func TestRenderObs_obsStorageClassOverrideReachesBothClaims(t *testing.T) {
	in := sampleInstance()
	in.ObsStorageClass = "fast-ssd"
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	assertClaim(t, got, obsDoc(t, got, "Deployment", "loki"), "data", "loki-data", in.Namespace(), "fast-ssd", obsLokiVolumeSize)
	assertPushgatewayPersists(t, got, in.Namespace(), "fast-ssd")

	in = sharedInstance()
	in.ObsStorageClass = "fast-ssd"
	got, err = RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	assertPushgatewayPersists(t, got, in.Namespace(), "fast-ssd")
}

// The results volume's class (RWX, azurefile-csi / argus-rwx) must NOT leak into the obs volumes: an
// operator's --storage-class names the RESULTS volume and must not move Loki onto a file share.
func TestRenderObs_resultsStorageClassDoesNotMoveTheObsVolumes(t *testing.T) {
	in := sampleInstance()
	in.StorageClass = "my-nfs"
	in.AccessMode = "ReadWriteMany"
	got, err := RenderObs(in, cfgJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	assertClaim(t, got, obsDoc(t, got, "Deployment", "loki"), "data", "loki-data", in.Namespace(), "", obsLokiVolumeSize)
}

func TestRenderSharedLoki_dataIsAnRWOClaim(t *testing.T) {
	for _, c := range obsClassByTier {
		t.Run(c.tier, func(t *testing.T) {
			got, err := RenderSharedLoki(SharedLoki{Tier: c.tier})
			if err != nil {
				t.Fatal(err)
			}
			loki := obsDoc(t, got, "Deployment", "loki")
			assertClaim(t, got, loki, "data", "loki-data", SharedObsNamespace, c.class, obsLokiVolumeSize)
			assertRecreate(t, loki)
			assertRunsAs(t, loki, 10001)
		})
	}
	got, err := RenderSharedLoki(SharedLoki{Tier: "aks", StorageClass: "fast-ssd"})
	if err != nil {
		t.Fatal(err)
	}
	assertClaim(t, got, obsDoc(t, got, "Deployment", "loki"), "data", "loki-data", SharedObsNamespace, "fast-ssd", obsLokiVolumeSize)
}

// The instance quota bounds CPU, memory and pods only. If it ever starts bounding storage or claim
// counts, the two claims added here (5Gi + 1Gi, two PVCs) must be counted in it.
func TestRenderExecutor_quotaDoesNotBoundStorageSoTheObsClaimsFit(t *testing.T) {
	got, err := RenderExecutor(sampleInstance())
	if err != nil {
		t.Fatal(err)
	}
	q := obsDoc(t, got, "ResourceQuota", "instance-quota")
	spec, _ := q["spec"].(map[string]any)
	hard, _ := spec["hard"].(map[string]any)
	for k := range hard {
		switch k {
		case "limits.cpu", "limits.memory", "pods":
		default:
			t.Errorf("instance-quota now bounds %q — size it to hold loki-data (%s) and pushgateway-data (%s) as well", k, obsLokiVolumeSize, obsPushgatewayVolumeSize)
		}
	}
}

// ── compose: one source of truth for the Loki config ─────────────────────────────────────────────

func composeFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", name))
	if err != nil {
		t.Fatalf("read deploy/compose/%s: %v", name, err)
	}
	return b
}

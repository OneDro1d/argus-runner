package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// / — the observability volumes (Loki's data, the Pushgateway's file)
// get their own class flag, --obs-storage-class, separate from --storage-class (the RWX results volume).
// These drive the REAL render-k8s / render-obs-shared through dispatch, so a flag registered but never
// threaded into the Instance is caught here and not only in k8srender's own tests.

func readFileT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRenderK8s_ObsStorageClassDefaultsPerTierAndIsReported(t *testing.T) {
	rc, out, _ := runRenderK8s(t) // the helper's tier is aks
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	if out.ObsStorageClass != "managed-csi" {
		t.Errorf("JSON obs_storage_class = %q, want managed-csi on aks", out.ObsStorageClass)
	}
	m := readFileT(t, out.Obs)
	if strings.Count(m, "storageClassName: managed-csi") != 2 {
		t.Errorf("obs.yaml should carry managed-csi on both the loki-data and pushgateway-data claims:\n%s", m)
	}
	// Every other tier: NO class named — the cluster's default StorageClass binds the claims — and the
	// report says so with an empty string, never an invented name.
	for _, tier := range []string{"k3d", "managed"} {
		rc, out, _ = runRenderK8s(t, "--tier", tier)
		if rc != exitOK || out.ObsStorageClass != "" {
			t.Errorf("%s: rc=%d obs_storage_class=%q, want empty (cluster default)", tier, rc, out.ObsStorageClass)
		}
		if m := readFileT(t, out.Obs); strings.Contains(m, "storageClassName: local-path") || strings.Contains(m, "storageClassName: managed-csi") {
			t.Errorf("%s: obs.yaml names a class although none was asked for:\n%s", tier, m)
		}
	}
}

func TestRenderK8s_ObsStorageClassFlagReachesBothClaimsAndNotTheResultsVolume(t *testing.T) {
	rc, out, _ := runRenderK8s(t, "--obs-storage-class", "fast-ssd", "--storage-class", "my-rwx", "--results-access-mode", "ReadWriteMany")
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	if out.ObsStorageClass != "fast-ssd" || out.StorageClass != "my-rwx" {
		t.Errorf("obs_storage_class=%q storage_class=%q, want fast-ssd / my-rwx", out.ObsStorageClass, out.StorageClass)
	}
	obs := readFileT(t, out.Obs)
	if strings.Count(obs, "storageClassName: fast-ssd") != 2 || strings.Contains(obs, "my-rwx") {
		t.Errorf("obs.yaml: want fast-ssd on both obs claims and never the results class:\n%s", obs)
	}
	if exec := readFileT(t, out.Executor); !strings.Contains(exec, "storageClassName: my-rwx") || strings.Contains(exec, "fast-ssd") {
		t.Errorf("executor.yaml: the results claim must keep --storage-class and never take --obs-storage-class")
	}
}

func TestRenderObsShared_ObsStorageClassFlag(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--tier", "aks"}, "storageClassName: managed-csi"},
		{[]string{"--tier", "k3d", "--obs-storage-class", "standard"}, "storageClassName: standard"},
		{[]string{"--tier", "aks", "--obs-storage-class", "fast-ssd"}, "storageClassName: fast-ssd"},
	} {
		dir := t.TempDir()
		rc := dispatch(append([]string{"render-obs-shared", "--out", dir}, c.args...))
		if rc != exitOK {
			t.Fatalf("%v: exit %d", c.args, rc)
		}
		if y := readFileT(t, filepath.Join(dir, "obs-shared.yaml")); !strings.Contains(y, c.want) {
			t.Errorf("%v: obs-shared.yaml lacks %q", c.args, c.want)
		}
	}
}

func TestUpOnboardArgs_ObsStorageClassForwardedOnlyWhenGiven(t *testing.T) {
	given := strings.Join(upOnboardArgs(upArgs{Tier: "aks", ObsStorageClass: "fast-ssd"}, "/p", "/s", "i"), " ")
	if !strings.Contains(given, "--obs-storage-class fast-ssd") {
		t.Errorf("onboard.sh args lack --obs-storage-class fast-ssd: %q", given)
	}
	if absent := strings.Join(upOnboardArgs(upArgs{Tier: "aks"}, "/p", "/s", "i"), " "); strings.Contains(absent, "--obs-storage-class") {
		t.Errorf("--obs-storage-class forwarded though not given: %q", absent)
	}
}

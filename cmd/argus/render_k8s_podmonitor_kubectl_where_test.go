package main

// AC-D57 (#391): `render-k8s --podmonitor auto` probes the PodMonitor CRD with
// kubectl. Where kubectl does not exist (the executor image ships none) the note must say THAT, not
// dress it up as a cluster problem; and a cluster/API failure must keep its own, different wording.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubKubectlOnPath puts a `kubectl` shell stub alone on PATH: it prints stderrText and exits 1.
func stubKubectlOnPath(t *testing.T, stderrText string) {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\necho '" + stderrText + "' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestRenderK8s_PodmonitorAuto_KubectlNotOnPath_NamesThatReason(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty directory: no kubectl anywhere, exactly the executor image
	rc, out, outDir, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--kube-context", "k3d-argus")
	if rc != exitOK {
		t.Fatalf("exit %d: %+v", rc, out)
	}
	if !strings.Contains(stderr, "kubectl is not available where render-k8s runs") {
		t.Errorf("the note must say kubectl is not available where render-k8s runs, got stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "the cluster was not checked") {
		t.Errorf("the note must say the cluster was not checked, got stderr: %q", stderr)
	}
	if strings.Contains(stderr, "could not check kube-context") {
		t.Errorf("a missing kubectl must not be reported as a failure to check the kube-context: %q", stderr)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if !strings.Contains(string(obs), "kind: PodMonitor") {
		t.Errorf("an unanswered probe keeps today's default (include the PodMonitor), got:\n%s", obs)
	}
}

func TestRenderK8s_PodmonitorAuto_ClusterUnreachable_KeepsItsOwnWording(t *testing.T) {
	stubKubectlOnPath(t, "Unable to connect to the server: dial tcp 10.0.0.1:6443: i/o timeout")
	rc, out, _, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--kube-context", "k3d-argus")
	if rc != exitOK {
		t.Fatalf("exit %d: %+v", rc, out)
	}
	if !strings.Contains(stderr, "could not check kube-context") || !strings.Contains(stderr, "Unable to connect") {
		t.Errorf("a cluster failure keeps the could-not-check wording with kubectl's own words, got stderr: %q", stderr)
	}
	if strings.Contains(stderr, "kubectl is not available where render-k8s runs") {
		t.Errorf("a cluster failure must NOT be reported as a missing kubectl: %q", stderr)
	}
}

func TestRenderK8s_PodmonitorAuto_CRDAbsent_OmitsViaTheNotFoundBranch(t *testing.T) {
	stubKubectlOnPath(t, `Error from server (NotFound): customresourcedefinitions "podmonitors.monitoring.coreos.com" not found`)
	rc, out, outDir, _, stderr := renderK8sRaw(t, noObsConfig, "--collect-sut-logs", "--kube-context", "k3d-argus")
	if rc != exitOK {
		t.Fatalf("exit %d: %+v", rc, out)
	}
	obs, err := os.ReadFile(filepath.Join(outDir, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if strings.Contains(string(obs), "kind: PodMonitor") {
		t.Errorf("a definitive NotFound must omit the PodMonitor, got:\n%s", obs)
	}
	if !strings.Contains(stderr, "no podmonitors.monitoring.coreos.com CRD found") {
		t.Errorf("expected the CRD-absent note, got stderr: %q", stderr)
	}
}

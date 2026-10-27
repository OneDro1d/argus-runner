package k8srender

import (
	"strings"
	"testing"
)

// Observability.loki.shared_cluster is reachable from --obs=adopt and from nothing else, and it
// renders NOTHING: an adopt instance on a shared Loki is byte-identical to an adopt instance on any other Loki.

func adoptInstance() Instance {
	in := sampleInstance()
	in.ObsMode = "adopt"
	in.ObsLokiURL = "http://loki.observability.svc:3100"
	return in
}

func TestSharedLogCluster_AdoptRendersExactlyWhatAdoptRenders(t *testing.T) {
	plain, shared := adoptInstance(), adoptInstance()
	shared.ObsSharedLogCluster = "cluster-a"

	a, err := RenderExecutor(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderExecutor(shared)
	if err != nil {
		t.Fatalf("adopt + shared cluster must render: %v", err)
	}
	if a != b {
		t.Fatal("the shared cluster label rides in the embedded argus-config; the executor manifest must not change")
	}
	oa, _ := RenderObs(plain, cfgJSON(t))
	ob, err := RenderObs(shared, cfgJSON(t))
	if err != nil || oa != ob || ob != "" {
		t.Fatalf("adopt deploys no obs, shared or not: %q / %q / %v", oa, ob, err)
	}
	if !strings.Contains(b, `"http://loki.observability.svc:3100"`) {
		t.Errorf("the executor must read the configured Loki:\n%s", b)
	}
}

func TestSharedLogCluster_OnlyAdoptMayCarryIt(t *testing.T) {
	for _, mode := range []string{"", "bundled", "export", "shared", "none"} {
		in := sampleInstance()
		in.ObsMode = mode
		in.ObsLokiURL, in.ObsLokiPushURL, in.ObsCredentialVarName = "http://l", "http://l/push", "C"
		in.ObsSharedURL = SharedLokiInClusterURL
		in.ObsSharedLogCluster = "cluster-a"
		_, err := RenderExecutor(in)
		if err == nil || !strings.Contains(err.Error(), "observability.loki.shared_cluster") || !strings.Contains(err.Error(), "--obs=adopt") {
			t.Errorf("--obs=%q with shared_cluster must be refused naming both, got %v", mode, err)
		}
	}
}

// 3: an instance that does not set it renders as before (the bundled and adopt goldens already in this package
// hold the bytes; this holds that the new field's zero value cannot change them).
func TestSharedLogCluster_ZeroValueChangesNothing(t *testing.T) {
	for _, mode := range []string{"", "bundled", "none"} {
		a, b := sampleInstance(), sampleInstance()
		a.ObsMode, b.ObsMode = mode, mode
		b.ObsSharedLogCluster = "   "
		ra, err := RenderExecutor(a)
		if err != nil {
			t.Fatal(err)
		}
		rb, err := RenderExecutor(b)
		if err != nil || ra != rb {
			t.Fatalf("--obs=%q: a blank shared cluster must change nothing (%v)", mode, err)
		}
	}
}

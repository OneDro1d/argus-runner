package main

import (
	"strings"
	"testing"
)

// The REAL render-k8s: --executor-cpu reaches the executor Deployment's CPU limit (and the quota
// that holds it), the default is unchanged, and a bad value is refused before any file is written.

func TestRenderK8s_ExecutorCPUReachesTheLimitAndTheQuota(t *testing.T) {
	rc, out, _ := runRenderK8s(t)
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	def := readFileT(t, out.Executor)
	if !strings.Contains(def, `limits:   {cpu: "1", memory: "1536Mi"}`) || !strings.Contains(def, `limits.cpu: "6"`) {
		t.Fatalf("default executor.yaml must carry the 1 CPU limit and a 6 CPU quota")
	}

	rc, out, _ = runRenderK8s(t, "--executor-cpu", "2")
	if rc != exitOK {
		t.Fatalf("render-k8s --executor-cpu 2 exited %d: %+v", rc, out)
	}
	m := readFileT(t, out.Executor)
	if !strings.Contains(m, `limits:   {cpu: "2", memory: "1536Mi"}`) || !strings.Contains(m, `requests: {cpu: "150m", memory: "384Mi"}`) || !strings.Contains(m, `limits.cpu: "9"`) {
		t.Errorf("--executor-cpu 2: the limit must be 2, the request 150m and the quota 9:\n%s", m)
	}

	rc, out, _ = runRenderK8s(t, "--executor-cpu", "1500m")
	if rc != exitOK {
		t.Fatalf("render-k8s --executor-cpu 1500m exited %d: %+v", rc, out)
	}
	if m := readFileT(t, out.Executor); !strings.Contains(m, `limits:   {cpu: "1500m", memory: "1536Mi"}`) || !strings.Contains(m, `limits.cpu: "7500m"`) {
		t.Errorf("--executor-cpu 1500m: the limit must be 1500m and the quota 7500m:\n%s", m)
	}
}

func TestRenderK8s_ExecutorCPURefusals(t *testing.T) {
	for _, v := range []string{"lots", "2 cpu", "149m", "0"} {
		rc, _, outDir := runRenderK8s(t, "--executor-cpu", v)
		if rc == exitOK {
			t.Errorf("--executor-cpu %q must be refused", v)
		}
		if matches := filesIn(outDir); len(matches) != 0 {
			t.Errorf("--executor-cpu %q: a refusal must write nothing, found %v", v, matches)
		}
	}
}

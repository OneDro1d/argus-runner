package main

import (
	"os"
	"strings"
	"testing"
)

// filesIn lists the names in dir; a directory that was never created has none.
func filesIn(dir string) []string {
	des, _ := os.ReadDir(dir)
	var names []string
	for _, d := range des {
		names = append(names, d.Name())
	}
	return names
}

// The REAL render-k8s: --executor-memory reaches the executor Deployment's limit (and only the
// limit), the default is unchanged, and a bad value is refused before any file is written.

func TestRenderK8s_ExecutorMemoryReachesTheLimitOnly(t *testing.T) {
	rc, out, _ := runRenderK8s(t)
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, out)
	}
	def := readFileT(t, out.Executor)
	if !strings.Contains(def, `memory: "1536Mi"`) || !strings.Contains(def, `memory: "384Mi"`) {
		t.Fatalf("default executor.yaml must carry the 384Mi request and the 1536Mi limit")
	}

	rc, out, _ = runRenderK8s(t, "--executor-memory", "3Gi")
	if rc != exitOK {
		t.Fatalf("render-k8s --executor-memory 3Gi exited %d: %+v", rc, out)
	}
	m := readFileT(t, out.Executor)
	if !strings.Contains(m, `limits:   {cpu: "1", memory: "3Gi"}`) || !strings.Contains(m, `requests: {cpu: "150m", memory: "384Mi"}`) || strings.Contains(m, "1536Mi") {
		t.Errorf("--executor-memory 3Gi: the limit must be 3Gi, the request 384Mi, and 1536Mi gone:\n%s", m)
	}
}

func TestRenderK8s_ExecutorMemoryRefusals(t *testing.T) {
	for _, v := range []string{"lots", "3 Gi", "383Mi", "0"} {
		rc, _, outDir := runRenderK8s(t, "--executor-memory", v)
		if rc == exitOK {
			t.Errorf("--executor-memory %q must be refused", v)
		}
		if matches := filesIn(outDir); len(matches) != 0 {
			t.Errorf("--executor-memory %q: a refusal must write nothing, found %v", v, matches)
		}
	}
}

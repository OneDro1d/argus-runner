package updatecmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/k8srender"
)

// override_reading_test.go — AC-D53, round-5 review R5-GATE-1 (high): THE VERSION IS THE BINARY'S OWN, NEVER THE
// ARGUS_VERSION OVERRIDE.
//
// With ARGUS_VERSION set, `argus version` reports that value (buildinfo.Resolve: a supported pin). discover.sh took it
// for the executor's version, so it became the not-confirmed marker's from_version, and the gate — which counts every
// version a marker names — refused every plan after one unconfirmed update: the re-run of that same block too, while
// "update to 0.9.9-pinned or above" names no release. The same value was written as the version of an executor a run
// did not move (round-6 design check), which refused every update after. Every reading below is taken by the REAL
// discover.sh from the stubbed container, which answers as a binary stamped <version> running with the override does.

const img35 = "ghcr.io/x/exec@sha256:v35"

// stubFiles writes the named stub files (content as given) before discover runs.
func stubFiles(t *testing.T, files map[string]string) func(root string) {
	return func(root string) {
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// readingOf is what discover.sh records for an executor whose binary is stamped version and which runs with
// ARGUS_VERSION=pin ("" = no override).
func readingOf(t *testing.T, tier, version, pin string) Observed {
	t.Helper()
	files := map[string]string{"executor-version": version}
	if pin != "" {
		files["executor-override"] = pin
	}
	return runDiscover(t, tier, stubFiles(t, files))
}

// The pods discover lists are the ones the rendered executor Deployment selects: a selector that drifted from the
// renderer's would list none, and the version would never be read on k3d or managed again.
func TestDiscover_TheExecutorPodSelectorIsTheRenderedDeployments(t *testing.T) {
	out, err := k8srender.RenderExecutor(k8srender.Instance{
		ID: "probe-inst", Tier: "aks", SUTNamespace: "sut-ns",
		Image: "ghcr.io/example/suite@sha256:deadbeef", Replicas: 1,
		CPURL: "https://cp.example", WorkspaceID: "ws", SUTName: "sut",
		ArgusConfig: "project: sut\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	k, v, _ := strings.Cut(executorPodSelector, "=")
	re := regexp.MustCompile(`(?m)^kind: Deployment\n(?:.*\n)*?\s*name: executor\n(?:.*\n)*?\s*selector:\s*\n\s*matchLabels:\s*\n\s*` +
		regexp.QuoteMeta(k) + `: ` + regexp.QuoteMeta(v) + `\n`)
	if !re.MatchString(out) {
		t.Errorf("the rendered executor Deployment does not select its pods by %s — discover would list none", executorPodSelector)
	}
}

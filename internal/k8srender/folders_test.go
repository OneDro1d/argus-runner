package k8srender

// U7 / CP-M3-III-80: the k8s tiers must report the folders they were onboarded from.
//
// CP-M3-III-73 built the FOLDERS row and wired it in docker-compose.byo-m3.yml only. Measured on the
// rebuilt estate 2026-08-07: all three COMPOSE instances reported product_dir/test_dir, and all three
// k3d instances plus the managed one reported nothing — so the row was blank on exactly the tiers
// where the question is hardest to answer. A pod on a cluster has no visible relationship to anyone's
// disk; compose at least has the mount in `docker inspect`.

import (
	"strings"
	"testing"
)

func TestFoldersEnvBlock_RendersAllThree(t *testing.T) {
	// VR5-U1 (2026-08-15) made it FOUR: the kit directory joined the trio, by the same route and for
	// the same reason — a pod cannot derive it, and the control plane cannot compose an absolute
	// update command without it.
	got := foldersEnvBlock(`C:\tmp\SMCP-product-agent-aks`, `C:\tmp\SMCP-test-agent-aks\scenarios`, "DESKTOP-EXAMPLE", `C:\Users\api\argus-kits\social-aks-v1`, "", "")
	for _, want := range []string{
		"ARGUS_PRODUCT_DIR_HOST", `C:\\tmp\\SMCP-product-agent-aks`,
		"ARGUS_TEST_DIR_HOST", "ARGUS_ONBOARD_HOST", "DESKTOP-EXAMPLE",
		"ARGUS_KIT_DIR_HOST", `C:\\Users\\api\\argus-kits\\social-aks-v1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Absent must stay ABSENT. A present-but-empty env var makes the executor report "" as a real answer,
// and the control plane cannot tell that from "this tier does not know" — which is the whole
// absence-is-not-health trap. Each key is dropped independently, so a partially-known set still
// reports what it does know.
func TestFoldersEnvBlock_OmitsEmptyIndividually(t *testing.T) {
	if got := foldersEnvBlock("", "", "", "", "", ""); got != "" {
		t.Errorf("all-empty must render nothing, got %q", got)
	}
	got := foldersEnvBlock("", "/scen", "", "", "", "")
	if strings.Contains(got, "ARGUS_PRODUCT_DIR_HOST") || strings.Contains(got, "ARGUS_ONBOARD_HOST") || strings.Contains(got, "ARGUS_KIT_DIR_HOST") {
		t.Errorf("empty keys must be omitted entirely, got:\n%s", got)
	}
	if !strings.Contains(got, "ARGUS_TEST_DIR_HOST") {
		t.Errorf("a known value must still be reported, got:\n%s", got)
	}
}

// The rendered block must sit inside the container's env list at the same indentation as its
// neighbours, or the manifest is not valid YAML and the whole onboard fails at apply time.
func TestFoldersEnvBlock_IndentationMatchesNeighbours(t *testing.T) {
	got := foldersEnvBlock("/p", "/t", "h", "/kit", "k3d-argus", "/home/api/.kube/config")
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if !strings.HasPrefix(line, "            - name: ") && !strings.HasPrefix(line, "              value: ") {
			t.Errorf("unexpected indentation: %q", line)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("block must end with a newline so the next template line starts clean")
	}
}

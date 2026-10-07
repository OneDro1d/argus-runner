package k8srender

import (
	"strings"
	"testing"
)

// (a): on every k8s tier an executor of an `--obs none` instance receives the mode in
// its own environment (ARGUS_OBS_MODE=none), so its validate_config (relayed verb and
// runner__validate_config) answers as onboarding's did, with no Grafana URL declared. Every other
// mode renders no such entry, so those manifests are byte-for-byte what they were.

const obsModeEnvNone = "            - name: ARGUS_OBS_MODE\n              value: \"none\"\n"

var obsModeTiers = []string{"k3d", "aks", "eks", "gke", "managed", "k8s-dev", "k3s", ""}

func TestRenderExecutor_ObsNoneCarriesTheModeOnEveryTier(t *testing.T) {
	for _, tier := range obsModeTiers {
		in := sampleInstance()
		in.Tier, in.Cluster, in.ObsMode = tier, "", "none"
		out, err := RenderExecutor(in)
		if err != nil {
			t.Fatalf("tier %q: RenderExecutor(none): %v", tier, err)
		}
		if got := strings.Count(out, obsModeEnvNone); got != 1 {
			t.Errorf("tier %q: the executor env must carry ARGUS_OBS_MODE=none exactly once, found %d", tier, got)
		}
		// it is in the executor container's env list, not anywhere else
		envAt, modeAt := strings.Index(out, "          env:\n"), strings.Index(out, obsModeEnvNone)
		fromAt := strings.Index(out, "          envFrom:\n")
		if envAt < 0 || modeAt < envAt || modeAt > fromAt {
			t.Errorf("tier %q: ARGUS_OBS_MODE is not inside the executor container's env list", tier)
		}
	}
}

func TestRenderExecutor_OtherObsModesCarryNoMode(t *testing.T) {
	for _, tier := range obsModeTiers {
		for _, mode := range []string{"", "bundled", "adopt", "export", "shared"} {
			in := sampleInstance()
			in.Tier, in.Cluster, in.ObsMode = tier, "", mode
			in.ObsLokiURL = "http://operator-loki.example:3100"
			in.ObsLokiPushURL = "https://logs.example/loki/api/v1/push"
			in.ObsCredentialVarName = "LOKI_CREDENTIAL"
			in.ObsSharedURL = SharedLokiInClusterURL
			out, err := RenderExecutor(in)
			if err != nil {
				t.Fatalf("tier %q mode %q: RenderExecutor: %v", tier, mode, err)
			}
			if strings.Contains(out, "ARGUS_OBS_MODE") {
				t.Errorf("tier %q mode %q: the manifest must not mention ARGUS_OBS_MODE (it is byte-identical to before)", tier, mode)
			}
		}
	}
}

// Removing the one entry from the none render leaves exactly the render a none instance had before:
// the entry is the ONLY thing this adds.
func TestRenderExecutor_ObsNoneAddsOnlyTheModeEntry(t *testing.T) {
	in := sampleInstance()
	in.ObsMode = "none"
	got, err := RenderExecutor(in)
	if err != nil {
		t.Fatal(err)
	}
	withoutEntry := strings.Replace(got, obsModeEnvNone, "", 1)
	if strings.Contains(withoutEntry, "ARGUS_OBS_MODE") || len(got)-len(withoutEntry) != len(obsModeEnvNone) {
		t.Fatalf("the none render must differ from the pre-change render by the one two-line env entry only")
	}
}

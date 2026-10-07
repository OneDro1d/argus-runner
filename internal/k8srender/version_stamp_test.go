package k8srender

// INT-001 (wave 1.5): a k8s executor must report the version of the BINARY IT RUNS, not the version of
// the onboarding kit that rendered its manifest.
//
// render-k8s populated the Deployment's ARGUS_VERSION with buildinfo.Resolve(os.Getenv(...)). With no
// operator override set, Resolve falls through to the KIT BINARY's own build stamp, which was then baked
// into the manifest as a literal. ARGUS_VERSION has the HIGHEST precedence inside the executor
// (buildinfo.go:56-59), and update.sh:256 on the k8s path only ever runs `kubectl set image` — it never
// touches the env. So: change the image, keep the env, and the executor runs binary A while reporting
// version B.
//
// Reproduced live on orderservice-k3d (rollback pinned first, reverted after). The pod ran m3-iii32 and
// said:
//
//	"commit": "5c67835"                                 <- m3-iii32's commit
//	"full":   "0.3.0+m3-iii34 (commit 5c67835, ...)"    <- version and commit CONTRADICT each other
//	"version":"0.3.0+m3-iii34"                          <- and this is what the CP was told
//
// It fires on exactly the operations F14/F15 exist to perform: after a self-update or an update.sh on
// k8s, the CP evaluates F13's minimum and F11's outdated-blocking against a STALE string, so an executor
// below the minimum reports itself `current` and keeps taking work — the precise failure F13 was built to
// prevent. It also weakens 4E retroactively: 4E proved the POD was replaced, not that the VERSION tracked
// the build.
//
// It is a TIER DIVERGENCE too: compose ships an EMPTY ARGUS_VERSION (docker-compose.byo-m3.yml uses
// ${ARGUS_VERSION:-}) and self-reports correctly. Only the k8s tiers baked a literal.
//
// The fix is to pass the RAW override through — empty when the operator set nothing — which is exactly
// what this file's own k8srender.go:174-177 already says it expects: "An empty value travels as an empty
// ARGUS_VERSION, and the binary reports its own build stamp instead."

import (
	"strings"
	"testing"
)

// versionEnvOf returns the rendered ARGUS_VERSION value for an executor manifest.
func versionEnvOf(t *testing.T, in Instance) string {
	t.Helper()
	out, err := RenderExecutor(in)
	if err != nil {
		t.Fatalf("RenderExecutor: %v", err)
	}
	for i, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "name: ARGUS_VERSION") {
			parts := strings.SplitN(strings.Split(out, "\n")[i+1], "value:", 2)
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), `"`)
			}
		}
	}
	return "<ARGUS_VERSION not rendered>"
}

func TestRenderExecutor_NoOverride_ShipsAnEmptyVersionSoTheBinarySelfReports(t *testing.T) {
	in := sampleInstance()
	in.Version = "" // the fix: the raw override, which is empty unless the operator set one

	if got := versionEnvOf(t, in); got != "" {
		t.Errorf("ARGUS_VERSION = %q, want empty.\n"+
			"A non-empty literal here OUTRANKS the running binary's own build stamp, so after a\n"+
			"`kubectl set image` (update.sh:256 — which never touches the env) the executor reports the\n"+
			"version it was ONBOARDED with, not the one it RUNS. F13's minimum and F11's\n"+
			"outdated-blocking then evaluate a stale string (INT-001).", got)
	}
}

func TestRenderExecutor_ExplicitOverrideIsStillHonoured(t *testing.T) {
	// The override exists for pinning and experiments. The fix must not remove that path — only stop
	// SYNTHESISING a value when the operator supplied none.
	in := sampleInstance()
	in.Version = "0.9.9-pinned"
	if got := versionEnvOf(t, in); got != "0.9.9-pinned" {
		t.Errorf("ARGUS_VERSION = %q, want the operator's explicit %q", got, "0.9.9-pinned")
	}
}

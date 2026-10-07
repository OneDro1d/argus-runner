package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VR6-W1 — the probe is actually WIRED, and refuses to guess when it cannot read the SUT's config.
//
// ── WHY A WIRING TEST EXISTS AT ALL ───────────────────────────────────────────────────────────────
//
// The probe, the tri-state and the wire field can all be correct and the feature still ship dead: if
// nothing ever sets Executor.SUTConfig, newSUTProbe leaves `probe` nil, every Observe returns nil, every
// poll omits the field, and the page shows grey on every instance forever. That state COMPILES, passes
// every other test in this package, and looks exactly like "no SUT has been probed yet" — which is a
// legitimate reading. It is the "a compile is not a render" failure with a different surface, and the
// same structural-assertion answer that caught the tierGrafana defect twice.

func writeSUTCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 🚩 THE STRUCTURAL ONE. Without this line the whole requirement is inert.
func TestSUTConfigIsWiredIntoTheExecutor(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "SUTConfig") {
		t.Fatal("Bootstrap never sets Executor.SUTConfig.\n" +
			"  Every other piece of VR6-W1 can be correct and the feature still ships dead: the probe " +
			"stays nil, every poll omits the field, and every instance shows grey forever — which is " +
			"indistinguishable from 'not probed yet' and so raises no alarm anywhere.")
	}
	if !strings.Contains(string(src), "sutConfigFor(") {
		t.Error("Bootstrap does not go through sutConfigFor — the loader whose failure modes are tested below")
	}
}

func TestSUTConfigFor_NoConfigPathMeansNotMeasured(t *testing.T) {
	if c := sutConfigFor(""); c != nil {
		t.Error("an executor with no config path was given something to dial")
	}
}

func TestSUTConfigFor_AnUnreadableConfigIsNotMeasuredRatherThanAPanic(t *testing.T) {
	if c := sutConfigFor(filepath.Join(t.TempDir(), "nope.yaml")); c != nil {
		t.Error("a missing config produced a dialable config")
	}
}

// ⚠ RESOLVED, NOT UNRESOLVED. The deployment watcher beside this deliberately uses ParseUnresolved, so
// an unexpanded ${VAR} elsewhere cannot block it. The probe must NOT copy that: it DIALS what it reads,
// and an unresolved config yields a literal "${DB_HOST}" as a hostname. That dial fails, and the page
// would then paint the SUT red on the strength of a template that was never expanded.
func TestSUTConfigFor_AnUnresolvedConfigIsNotMeasuredRatherThanDialledLiterally(t *testing.T) {
	p := writeSUTCfg(t, "project:\n  name: x\ntargets:\n  http:\n    base_url: http://${ARGUS_NO_SUCH_VAR_VR6W1}:8080\n")
	if c := sutConfigFor(p); c != nil {
		t.Fatal("a config with an unexpanded ${VAR} was accepted for dialling.\n" +
			"  The probe would dial the literal template text, fail, and the page would show the SUT " +
			"UNREACHABLE — a red verdict caused entirely by a missing environment variable on the " +
			"executor, not by anything wrong with the SUT.")
	}
}

func TestSUTConfigFor_AResolvableConfigIsDialable(t *testing.T) {
	p := writeSUTCfg(t, "project:\n  name: x\ntargets:\n  http:\n    base_url: http://sut:8080\n")
	c := sutConfigFor(p)
	if c == nil {
		t.Fatal("a perfectly good config was not accepted for dialling")
	}
	if !c.TargetPresentExported("http") {
		t.Error("the loaded config lost its http target")
	}
}

// End to end through the gate: a real config produces a real verdict rather than silence.
func TestNewSUTProbe_WithARealConfigProducesAVerdict(t *testing.T) {
	p := writeSUTCfg(t, "project:\n  name: x\ntargets:\n  http:\n    base_url: http://127.0.0.1:9\n")
	got, at := newSUTProbe(sutConfigFor(p)).Observe()
	if got == nil {
		t.Fatal("dialling a closed port produced NOT MEASURED instead of UNREACHABLE — the probe is " +
			"wired but not actually reaching ProbeTargets")
	}
	if *got {
		t.Error("port 9 on loopback reported as reachable")
	}
	if at == nil {
		t.Error("a verdict arrived with no timestamp; the control plane cannot age it out")
	}
}

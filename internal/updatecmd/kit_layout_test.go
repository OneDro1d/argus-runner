package updatecmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// kit_layout_test.go — THE UPDATE PATH AGREES WITH THE SCRIPTS THAT BUILT THE MACHINE.
//
// ⭐⭐ THIS GUARD EXISTS BECAUSE ITS ABSENCE TWICE ALMOST SHIPPED A DEAD UPDATE PATH.
//
// First the LAYOUT: the generated scripts resolved `env.<id>` at the kit root, where onboarding has
// never written it. Then the NAMES and MECHANISMS: the scripts addressed `argus-executor-<id>`
// containers, a `argus-<id>` namespace, `onboarding/lib/k3d.sh`, `docker-compose.obs.yml` and a
// `skills --install` verb — not one of which exists. Both times EVERY test was green, including the
// ones that execute real bash, because the fixtures were written from the same belief as the code.
//
// ⛔ So nothing here restates a name. Each test READS the shipped script that creates or uses the
// thing — onboard.sh, update.sh, the libraries, the compose files, the Dockerfile — and requires the
// planner to agree with it.

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s must exist — it is the authority this test derives from: %v", rel, err)
	}
	return string(b)
}

func onboardSh(t *testing.T) string { return repoFile(t, "onboarding/onboard.sh") }

// updateSh is the update.sh that ran against real machines through 0.3.31, FROZEN at its retirement.
//
// ⛔ 1-SHIM REDUCED onboarding/update.sh TO A TWO-LINE SHIM, and these guards must not lose their
// authority with it. They derive the instance's names and the executor's mechanisms from the script that
// was PROVEN on the owner's estate — a restatement here would be a third copy of a belief. The frozen
// copy is `git show <the commit before the shim>:onboarding/update.sh`, byte for byte.
func updateSh(t *testing.T) string { return repoFile(t, updateShRetired) }

const updateShRetired = "internal/updatecmd/testdata/update.sh.retired-0.3.32"

func mustMatch(t *testing.T, src, name, re string) []string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s no longer matches %s — re-point this case at the new line, never delete it", name, re)
	}
	return m
}

func renderedAll(t *testing.T, tier string) (pre, apply string) {
	t.Helper()
	in := fixtureInput(tier)
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	return RenderPreflight(p, in), RenderApply(p, in)
}

func TestKitLayout_BothGeneratedScriptsOpenTheRealEnvFile(t *testing.T) {
	pre, apply := renderedAll(t, "compose")
	want := `ENV_FILE="$KIT/` + KitEnvFile("$INSTANCE") + `"`
	for name, sh := range map[string]string{"preflight.sh": pre, "apply.sh": apply} {
		if !strings.Contains(sh, want) {
			t.Errorf("%s does not open the env file where onboarding writes it (want %s)", name, want)
		}
	}
}

// ⛔ THE INSTANCE'S NAMES ARE update.sh's, spelled once.
func TestKitLayout_TheInstanceNamesAreTheOnesUpdateShUses(t *testing.T) {
	src := updateSh(t)
	ns := mustMatch(t, src, "update.sh", `NS="([a-z-]+)\$INSTANCE_ID"`)[1]
	proj := mustMatch(t, src, "update.sh", `PROJECT="([a-z-]+)\$INSTANCE_ID"`)[1]
	if InstanceNamespace("X") != ns+"X" || InstanceProject("X") != proj+"X" {
		t.Fatalf("update.sh names the namespace %sX and the project %sX; the planner says %s and %s",
			ns, proj, InstanceNamespace("X"), InstanceProject("X"))
	}
	for _, tier := range []string{"compose", "k3d", "managed"} {
		pre, apply := renderedAll(t, tier)
		disc := RenderDiscover(DiscoverInput{InstanceID: "i1", Tier: tier, KitDir: "/k", StageDir: "/s"})
		for name, sh := range map[string]string{"preflight.sh": pre, "apply.sh": apply, "discover.sh": disc} {
			for _, want := range []string{`PROJECT="` + proj + `$INSTANCE"`, `NS="` + ns + `$INSTANCE"`} {
				if !strings.Contains(sh, want) {
					t.Errorf("%s %s does not define %s — it would address an instance no onboarding created", tier, name, want)
				}
			}
			// the executor's pod LABEL, which the renderer does set (executorPodSelector), is not a name
			if strings.Contains(strings.ReplaceAll(sh, executorPodSelector, ""), "argus-executor") {
				t.Errorf("%s %s still names `argus-executor` — no onboarding has ever produced that name", tier, name)
			}
		}
	}
}

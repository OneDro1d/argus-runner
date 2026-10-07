package updatecmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// V31-001 C-24 — THE MACHINE ROUTER KEEPS A FORWARD PATH: `update plan --router-only`.
//
// `onboarding/update.sh --router` was the only way to update the router on a machine with NO instances —
// the machine most likely to run a stale one (GETTING-STARTED.md "Upgrading later"). 1-SHIM retires
// update.sh, so the forward path moves into the planner: a plan that is the router step and nothing else.
//
// ⛔ IT RECORDS NO INSTANCE. The router is MACHINE-WIDE. A per-instance manifest written after a router-only
// move would describe an instance nothing measured — its executor, kit and skills re-recorded as "unknown"
// over the real versions a previous update wrote. C-24's "A-7 + A-8" is therefore A-7 alone.
//
// ⛔ AND IT MOVES THE ROUTER FROM THE NEW KIT WITHOUT SWAPPING ANY KIT. The router's compose definition and
// the version library are read from the kit `update plan` staged out of the new image; the operator's kit —
// and every instance onboarded from it — is not touched.

func routerOnlyInput(t *testing.T, root string) PlanInput {
	t.Helper()
	in := fakeMachine(t, root, "compose")
	in.RouterOnly = true
	in.InstanceID = ""
	in.Observed = Observed{}
	in.KitInstances = nil
	return in
}

func stepIDs(p Plan) string {
	ids := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ",")
}

func TestRouterOnlyPlan_IsTheRouterStepAndNothingElse(t *testing.T) {
	in := fixtureInput("compose")
	in.RouterOnly = true
	in.InstanceID = ""
	in.Observed = Observed{}
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("a router-only plan needs no instance and no observation — that is the machine it exists for: %v", err)
	}
	if got := stepIDs(p); got != "A-7" {
		t.Errorf("router-only steps = %s, want A-7 alone — the router is machine-wide and records no instance", got)
	}
	// an instance id given anyway changes nothing: no manifest is written for a machine-wide move
	in.InstanceID = "i1"
	if p, _ := BuildPlan(in); stepIDs(p) != "A-7" {
		t.Errorf("with an instance named, router-only steps = %s, want A-7 alone", stepIDs(p))
	}
	in.ImageDigest = "ghcr.io/x/exec:0.3.32"
	if _, err := BuildPlan(in); err == nil {
		t.Error("an unpinned --image-digest was accepted for a router-only plan")
	}
}

func runRouterOnly(t *testing.T, in PlanInput, failing map[string]string) (string, int) {
	t.Helper()
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(in.KitDir)
	for name, body := range map[string]string{"preflight.sh": RenderPreflight(p, in), "apply.sh": RenderApply(p, in)} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pc := exec.Command("bash", filepath.Join(root, "preflight.sh"))
	pc.Dir = root
	pc.Env = stubEnv(t, root, nil)
	if out, err := pc.CombinedOutput(); err != nil {
		t.Fatalf("the router-only preflight failed on a healthy machine: %v\n%s", err, out)
	}
	cmd := exec.Command("bash", filepath.Join(root, "apply.sh"))
	cmd.Dir = root
	cmd.Env = stubEnv(t, root, failing)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("could not run the router-only apply.sh: %v\n%s", err, out)
	}
	return string(out), code
}

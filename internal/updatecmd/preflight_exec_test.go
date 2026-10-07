package updatecmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// preflight_exec_test.go — V31-001 (VR13-UP): PREFLIGHT IS THE PRODUCER OF `untouched`, AND IT RUNS.
//
// These tests execute the generated script, because the claim is about what happens when it runs.

// runPreflight renders and executes preflight.sh against a fake machine.
func runPreflight(t *testing.T, in PlanInput, breakIt func(PlanInput)) (string, int) {
	t.Helper()
	return runPreflightWith(t, in, breakIt, nil)
}

// runPreflightWith is runPreflight with host programs failing on the given argv substrings.
func runPreflightWith(t *testing.T, in PlanInput, breakIt func(PlanInput), failing map[string]string) (string, int) {
	t.Helper()
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if breakIt != nil {
		breakIt(in)
	}
	root := filepath.Dir(in.KitDir)
	sh := filepath.Join(root, "preflight.sh") // beside, not inside, the stage — a case removes the stage's kit
	if err := os.WriteFile(sh, []byte(RenderPreflight(p, in)), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", sh)
	cmd.Dir = root
	cmd.Env = stubEnv(t, root, failing)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("could not run preflight.sh: %v\n%s", err, out)
	}
	return string(out), code
}

// rollbackInput turns fakeMachine's forward-update fixture into a rollback targeting the previous
// image — the shape the Environments page's rollback block actually produces.
func rollbackInput(t *testing.T, root string) PlanInput {
	t.Helper()
	in := fakeMachine(t, root, "compose")
	in.RollbackTo = "0.3.31"
	in.Version = "0.3.31"
	in.ImageDigest = "ghcr.io/x/exec@sha256:old"
	in.Installed = Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		Previous: &Previous{Version: "0.3.31", Image: "ghcr.io/x/exec@sha256:old"}}
	return in
}

// writeRunnerState writes what `argus runner-state` prints, produced the way it produces it.
func writeRunnerState(t *testing.T, root, running string) {
	t.Helper()
	b, _ := json.MarshalIndent(map[string]any{"running_run_id": running}, "", "  ") // = main.go:59's emit
	if err := os.WriteFile(filepath.Join(root, "runner-state.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

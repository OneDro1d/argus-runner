package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// V31-001 C-24 through the CLI: `update plan --router-only` plans the machine router alone, on a machine
// that may have no instance at all — so it needs neither --instance-id nor --observed. Without the flag
// the same arguments still refuse: an instance update never plans from no observation.
func TestUpdatePlan_RouterOnlyNeedsNoInstanceAndNoObservation(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	root := t.TempDir()
	kit := filepath.Join(root, "kit")
	stage := filepath.Join(root, "stage")
	state := filepath.Join(root, "state")
	bundle := filepath.Join(root, "bundle")
	for _, d := range []string{kit, state, filepath.Join(bundle, "onboarding")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "onboarding", "onboard.sh"), []byte("#!/usr/bin/env bash"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_BUNDLE_DIR", bundle)

	// ⚠ --router-only is a BOOL flag placed BEFORE value flags on purpose: update's own argv partition must
	// not swallow `--stage` as its value.
	if code := dispatch([]string{"update", "plan", "--router-only", "--stage", stage, "--kit", kit,
		"--router-state", state, "--image-digest", "ghcr.io/x/exec@sha256:new"}); code != exitOK {
		t.Fatalf("update plan --router-only exited %d with no instance and no observation", code)
	}
	b, err := os.ReadFile(filepath.Join(stage, "plan.json"))
	if err != nil {
		t.Fatalf("plan.json was not written: %v", err)
	}
	var p updatecmd.Plan
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 1 || p.Steps[0].ID != "A-7" {
		t.Errorf("router-only plan steps = %+v, want A-7 alone", p.Steps)
	}
	for _, f := range []string{"preflight.sh", "apply.sh", filepath.Join("kit", "onboarding", "onboard.sh")} {
		if _, err := os.Stat(filepath.Join(stage, f)); err != nil {
			t.Errorf("%s was not written: %v", f, err)
		}
	}

	if code := dispatch([]string{"update", "plan", "--stage", filepath.Join(root, "stage2"), "--kit", kit,
		"--router-state", state, "--image-digest", "ghcr.io/x/exec@sha256:new"}); code == exitOK {
		t.Error("without --router-only, a plan with no --observed and no --instance-id was accepted")
	}
}

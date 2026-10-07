package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FX5: `argus init <dir>` extracts the baked onboarding bundle so a colleague with ONLY
// the image gets the onboarder + compose + skills locally. copyTree is the testable core.
func TestCopyTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("onboarding/onboard.sh", "#!/usr/bin/env bash\necho hi\n")
	mk("deploy/compose/docker-compose.yaml", "name: x\n")
	mk("deploy/compose/grafana/dashboards/d.json", "{}")
	mk("skills/scenario-author/SKILL.md", "# skill")

	// VR4-K1: copyTree now also reports which files it REPLACED. This extraction is into an empty
	// directory, so it must report none — asserted here rather than discarded, because "nothing was
	// lost" is exactly the claim the operator is being asked to trust.
	replaced, err := copyTree(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != 0 {
		t.Fatalf("a first extraction reported overwrites: %v", replaced)
	}
	for _, rel := range []string{"onboarding/onboard.sh", "deploy/compose/docker-compose.yaml", "deploy/compose/grafana/dashboards/d.json", "skills/scenario-author/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing %s after copyTree: %v", rel, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dst, "onboarding", "onboard.sh"))
	if !strings.Contains(string(b), "echo hi") {
		t.Errorf("content not preserved: %q", b)
	}
}

// ── VR4-K1 (V18-005): `argus init` must not silently discard the operator's own edits ──────────
//
// THE DEFECT, measured 2026-08-14. `init` overwrote a kit's locally-patched onboard.sh with the
// image's copy — no warning, no diff. The kit's mtime moved to 13:39 over a patch written at 13:20
// and every marker of that patch went from present to absent.
//
// WHY IT MATTERS BEYOND TIDINESS. This round's standing rule is "fix locally, batch the build", so
// kit-side fixes live only on disk until a publish — and a_onboarding_samples.md runs `argus init`
// as step 1. The DOCUMENTED HAPPY PATH therefore discards the round's own workaround, and the onboard
// that follows runs on reverted code. A green run on reverted code is not evidence the fix works.
//
// The requirement is only that the operator is not SURPRISED. So the assertion is on what init
// REPORTS, not on it refusing: a file whose content genuinely changed must be named.

func TestCopyTree_ReportsWhatItOverwrote(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	write := func(dir, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// three shapes, and only ONE of them is a loss
	write(src, "onboarding/onboard.sh", "IMAGE VERSION")   // operator patched this -> must be named
	write(dst, "onboarding/onboard.sh", "LOCALLY PATCHED") //
	write(src, "onboarding/teardown.sh", "SAME")           // byte-identical -> nothing lost
	write(dst, "onboarding/teardown.sh", "SAME")           //
	write(src, "onboarding/update.sh", "BRAND NEW")        // absent locally -> nothing lost

	replaced, err := copyTree(src, dst)
	if err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	got := map[string]bool{}
	for _, p := range replaced {
		got[filepath.ToSlash(p)] = true
	}

	if !got["onboarding/onboard.sh"] {
		t.Errorf("the locally-patched file was overwritten and NOT reported; replaced=%v\n"+
			"This is V18-005: the operator's own work vanished with no warning.", replaced)
	}
	if got["onboarding/teardown.sh"] {
		t.Errorf("an IDENTICAL file was reported as overwritten — nothing was lost, and a warning "+
			"that cries wolf is one people learn to ignore; replaced=%v", replaced)
	}
	if got["onboarding/update.sh"] {
		t.Errorf("a NEW file was reported as overwritten — there was nothing there to lose; "+
			"replaced=%v", replaced)
	}

	// and it must still actually copy
	b, err := os.ReadFile(filepath.Join(dst, "onboarding/onboard.sh"))
	if err != nil || string(b) != "IMAGE VERSION" {
		t.Errorf("init must still extract the kit: got %q, %v", string(b), err)
	}
}

func TestCopyTree_CleanTargetReportsNothing(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.sh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	replaced, err := copyTree(src, dst)
	if err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if len(replaced) != 0 {
		t.Errorf("a first extraction into an empty directory replaced nothing, but reported %v", replaced)
	}
}

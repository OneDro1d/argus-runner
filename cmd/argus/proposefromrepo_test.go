package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// proposefromrepo_test.go — T5.1+T5.3 end to end through the real `dispatch`, proving the wiring:
// propose-from-repo is reachable with NO token (pre-auth, like package-check), --repo/--out/--json
// actually reach internal/reporoute, and an unknown flag is refused rather than silently dropped
// (the VR4-B3 partition — unknownflags_test.go covers the general rule; this proves this specific
// command is on the refusing side of it, since it does not read subArgs itself).

func TestProposeFromRepo_NoTokenNeeded(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "app.js"), []byte(
		"const app = require('express')();\napp.get('/health', function (req, res) { res.json({}); });\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "scenarios")

	rc := -1
	got := captureStdout(t, func() {
		rc = dispatch([]string{"propose-from-repo", "--repo", repo, "--out", out, "--json"})
	})
	if rc != exitOK {
		t.Fatalf("propose-from-repo with no token in env should not be denied; exit=%d out=%s", rc, got)
	}
	if strings.Contains(got, "auth not configured") || strings.Contains(got, "denied") {
		t.Fatalf("propose-from-repo demanded auth — it must be pre-auth like package-check: %s", got)
	}
	if !strings.Contains(got, `"GET"`) {
		t.Errorf("expected the GET /health route in the JSON report: %s", got)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected at least one scenario written under --out, got err=%v entries=%v", err, entries)
	}
}

func TestProposeFromRepo_MissingRepoRefused(t *testing.T) {
	rc := -1
	got := captureStdout(t, func() {
		rc = dispatch([]string{"propose-from-repo", "--out", t.TempDir()})
	})
	if rc != exitUsage {
		t.Fatalf("missing --repo should exit usage (%d), got %d: %s", exitUsage, rc, got)
	}
	if !strings.Contains(got, "--repo is required") {
		t.Errorf("refusal should name --repo: %s", got)
	}
}

func TestProposeFromRepo_MissingOutRefused(t *testing.T) {
	rc := -1
	got := captureStdout(t, func() {
		rc = dispatch([]string{"propose-from-repo", "--repo", t.TempDir()})
	})
	if rc != exitUsage {
		t.Fatalf("missing --out should exit usage (%d), got %d: %s", exitUsage, rc, got)
	}
	if !strings.Contains(got, "--out is required") {
		t.Errorf("refusal should name --out: %s", got)
	}
}

func TestProposeFromRepo_OutInsideRepoRefused(t *testing.T) {
	repo := t.TempDir()
	out := filepath.Join(repo, "scenarios")
	rc := -1
	got := captureStdout(t, func() {
		rc = dispatch([]string{"propose-from-repo", "--repo", repo, "--out", out})
	})
	if rc != exitUsage {
		t.Fatalf("--out inside --repo should exit usage (%d), got %d: %s", exitUsage, rc, got)
	}
	if !strings.Contains(got, "must not be inside --repo") {
		t.Errorf("refusal should explain --out-inside-repo: %s", got)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("--out must not have been created when refused")
	}
}

// TestProposeFromRepo_UnknownFlagRefused: since propose-from-repo does not own its argv (it is not
// in readsSubArgs), a flag it and the common set both do not recognise must be refused, not
// silently dropped — the exact defect class unknownflags.go exists for.
func TestProposeFromRepo_UnknownFlagRefused(t *testing.T) {
	rc := -1
	got := captureStdout(t, func() {
		rc = dispatch([]string{"propose-from-repo", "--repo", t.TempDir(), "--out", t.TempDir(), "--frobnicate", "x"})
	})
	if rc != exitUsage {
		t.Fatalf("unknown flag should exit usage (%d), got %d: %s", exitUsage, rc, got)
	}
	if !strings.Contains(got, "unknown flag") || !strings.Contains(got, "--frobnicate") {
		t.Errorf("refusal should name the unknown flag: %s", got)
	}
}

func TestProposeFromRepo_ListedInUsage(t *testing.T) {
	got := captureStdout(t, func() { dispatch(nil) })
	if !strings.Contains(got, "propose-from-repo") {
		t.Errorf("`argus --help` (bare dispatch) should list propose-from-repo: %s", got)
	}
}

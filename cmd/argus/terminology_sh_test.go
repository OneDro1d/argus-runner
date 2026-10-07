package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VR10-T6 (V27-009, the owner's STANDING terminology decision of 2026-09-05): the token minted for a machine during
// onboarding is the "author token (minted during onboarding)" everywhere a person reads it — never the retired
// two-word name. This test is V29-02 §1.4.T6's grep, made executable: it walks the scope the SA named (ui/src,
// onboarding/*.sh, onboarding/*.md, internal, cmd; tests and docs/dark-factory excluded) and fails on the first
// file that still carries the phrase, naming it. Positive control: red on e1d0962 (seven files), green on the
// swept tree.
func TestTerminology_TheRetiredTokenNameIsGone(t *testing.T) {
	root := repoRootForTerminology(t)
	scope := []string{"ui/src", "onboarding", "internal", "cmd"}
	exts := map[string]bool{".go": true, ".jsx": true, ".js": true, ".sh": true, ".md": true}
	var hits []string
	for _, dir := range scope {
		_ = filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !exts[filepath.Ext(path)] || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if dir == "onboarding" && filepath.Dir(path) != filepath.Join(root, "onboarding") {
				return nil // §1.4.T6 names onboarding/*.sh and onboarding/*.md, not the lib/ tree — the lib is covered by cmd/internal callers
			}
			blob, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			if strings.Contains(strings.ToLower(string(blob)), "onboarding token") {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, filepath.ToSlash(rel))
			}
			return nil
		})
	}
	if len(hits) > 0 {
		t.Fatalf("VR10-T6: the retired two-word token name survives in %d file(s) a person reads — say \"author token (minted during onboarding)\":\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

func repoRootForTerminology(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root (go.mod) not found above the test's working directory")
	return ""
}

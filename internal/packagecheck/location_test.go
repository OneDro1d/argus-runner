package packagecheck

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// AC-32: a finding's location is read on every platform (the report is JSON, and the tests compare
// literal "dir/file:line" strings), so it must use forward slashes wherever the check ran. On Windows
// filepath.Rel and filepath.Join produce backslashes; before this normaliser TestManifestsFail and
// TestEnvSchemaFail failed there on "k8s\base\control.yaml:10".

func TestFindingLocation_AnOSJoinedPathComesOutWithForwardSlashes(t *testing.T) {
	// filepath.Join yields the OS separator — a backslash on Windows — which is exactly what the
	// scanners get back from filepath.Rel there.
	got := findingLocation(filepath.Join("k8s", "base", "control.yaml"), 10)
	if want := "k8s/base/control.yaml:10"; got != want {
		t.Errorf("findingLocation(Join(k8s, base, control.yaml), 10) = %q, want %q", got, want)
	}
}

func TestFindingLocation_ABackslashPathIsNormalisedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a backslash is an ordinary filename character off Windows, so filepath.ToSlash rightly leaves it; the OS-joined test above covers this platform")
	}
	got := findingLocation(`cmd\toy\main.go`, 9)
	if want := "cmd/toy/main.go:9"; got != want {
		t.Errorf(`findingLocation("cmd\toy\main.go", 9) = %q, want %q`, got, want)
	}
}

func TestFindingLocation_NoScannedFindingCarriesABackslash(t *testing.T) {
	// The two clauses that build a location from a walked file, over their failing fixtures: every
	// location must be forward-slashed, on every platform.
	for _, c := range []Clause{checkManifests("testdata/manifests-fail"), checkEnvSchema("testdata/env-fail")} {
		if len(c.Findings) == 0 {
			t.Fatalf("clause %s produced no findings on its failing fixture — nothing was checked", c.Name)
		}
		for _, f := range c.Findings {
			if strings.Contains(f.Location, `\`) {
				t.Errorf("clause %s: location %q contains a backslash", c.Name, f.Location)
			}
		}
	}
}

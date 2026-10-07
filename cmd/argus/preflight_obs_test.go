package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// `argus preflight --obs <mode>` takes the onboarder's values; a bad one is refused with
// the valid list, before any probe runs.
func TestPreflight_InvalidObsIsRefusedWithTheValidList(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	rc := cmdPreflight([]string{"--obs", "grafana"})
	w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if rc != exitUsage {
		t.Fatalf("exit = %d, want exitUsage (%d)", rc, exitUsage)
	}
	for _, v := range []string{"bundled", "adopt", "export", "shared", "none", "grafana"} {
		if !strings.Contains(string(out), v) {
			t.Errorf("refusal does not mention %q:\n%s", v, out)
		}
	}
}

func TestPreflightUsage_ListsObsFlag(t *testing.T) {
	if !strings.Contains(preflightUsage, "--obs") {
		t.Fatalf("preflightUsage does not list --obs:\n%s", preflightUsage)
	}
}

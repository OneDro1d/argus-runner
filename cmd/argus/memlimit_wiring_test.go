package main

import (
	"os"
	"strings"
	"testing"
)

// the executor's start-up must call memlimit.Apply. The package's own tests prove the
// limit is read and applied; this proves the runner mode asks for it, before it bootstraps.
func TestRunnerModeAppliesTheContainerMemoryLimit(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, `case "runner":`)
	j := strings.Index(s, "runner.Bootstrap(ctx, rcfg)")
	if i < 0 || j < 0 || j < i {
		t.Fatalf("could not find the runner case and its Bootstrap call (%d, %d)", i, j)
	}
	if !strings.Contains(s[i:j], "memlimit.Apply(") {
		t.Fatal("the runner mode does not call memlimit.Apply before runner.Bootstrap")
	}
}

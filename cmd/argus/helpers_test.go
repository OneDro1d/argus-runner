package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildStampedArgusBinary builds this package with the release version stamped in, the way the image
// build stamps it (VERSION build arg -> buildinfo.Version).
func buildStampedArgusBinary(t *testing.T, version string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH; cannot build the binary under test")
	}
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "argus-stamped-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin := filepath.Join(dir, "argus")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-ldflags", "-X github.com/OneDro1d/argus-runner/internal/buildinfo.Version="+version, "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\r\n \t"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func readTestFile(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

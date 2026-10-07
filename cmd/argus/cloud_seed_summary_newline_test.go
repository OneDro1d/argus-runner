package main

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// TestPrintSeedSummary_EmitsRealNewlines (, reported by Talos, Bartek's agent, from a
// Windows run): the summary was written with a doubled backslash, so the operator saw a literal
// backslash-n where the line should end, everything on one line. The older seed tests look for
// substrings, which a literal backslash-n satisfies.
func TestPrintSeedSummary_EmitsRealNewlines(t *testing.T) {
	var b strings.Builder
	printSeedSummary(&b, 1, 2, []onboard.SeedFailure{{Path: "a.md", Error: "bad\nreason"}})
	out := b.String()
	if strings.Contains(out, `\n`) {
		t.Fatalf("summary carries a literal backslash-n: %q", out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "  a.md") || lines[1] != "imported 1 of 2 — 1 rejected" {
		t.Fatalf("want a reject line then the arithmetic on its own line, got %q", out)
	}
	b.Reset()
	printSeedSummary(&b, 2, 2, nil)
	if b.String() != "imported 2 of 2\n" {
		t.Fatalf("clean import: got %q", b.String())
	}
}

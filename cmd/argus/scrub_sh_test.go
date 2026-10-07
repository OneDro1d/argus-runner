package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The transcript scrub (VR8-R1) is bash, so it is tested by RUNNING it — the same way this package
// already tests the other shell surfaces. Feeding it synthetic strings is the whole point: a
// happy-path onboard produces NO token in its output (measured: onboard.sh:1474 redirects to
// $ENV_FILE at :1488, and there is no `set -x`), so "no token appeared in a real run" would test the
// run and not the scrub. These cases are the only thing that exercises it.
func runScrub(t *testing.T, in string) string {
	t.Helper()
	lib := filepath.Join("..", "..", "onboarding", "lib", "scrub.sh")
	cmd := exec.Command("bash", "-c", ". "+filepath.ToSlash(lib)+"; scrub_stream")
	cmd.Stdin = strings.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("scrub_stream failed: %v\nstderr: %s", err, errb.String())
	}
	return out.String()
}

// ── VR8-K3 (V26-011 + D9) and VR8-K4 (V26-006) ───────────────────────────────────────────────────

func TestRouterWire_TheCommentNamesTheRealSourceOfFolderPaths(t *testing.T) {
	blob, err := os.ReadFile(filepath.Join("router_wire.go"))
	if err != nil {
		t.Fatalf("read router_wire.go: %v", err)
	}
	src := string(blob)

	// The truth: the CP holds folder paths in the migration-015 COLUMNS.
	if !strings.Contains(src, "instances.product_dir / test_dir") {
		t.Error("the comment does not name where the control plane actually keeps folder paths " +
			"(instances.product_dir / test_dir, migration 015)")
	}
	// The false claim may only survive as something being CORRECTED.
	if i := strings.Index(src, "Migration 016 gives the CONTROL PLANE"); i >= 0 {
		ctx := src[max(0, i-320):i]
		if !strings.Contains(ctx, "used to get it") && !strings.Contains(ctx, "wrong") {
			t.Error("the comment still asserts that migration 016 gives the control plane the answer. " +
				"Nothing writes instance_folders; a grep finds it only in a cascade-delete test")
		}
	}
	// And the table must NOT have been dropped as a tidy-up.
	if strings.Contains(src, "DROP TABLE instance_folders") {
		t.Error("instance_folders was dropped. It answers the one question the columns cannot — which " +
			"instances share a folder — and one product folder legitimately serves several at once")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// maxInt keeps the index arithmetic above readable when a search misses.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

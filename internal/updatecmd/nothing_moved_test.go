package updatecmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// nothing_moved_test.go — AC-D53: A MOVE THAT NEVER STARTED IS UNDONE AS NOTHING.
//
// A-3 registers for undo BEFORE its effect (scripts_apply.go renderSteps), so its undo runs for a move that failed on
// its first line — "EVERY UNDO TOLERATES AN EFFECT THAT NEVER STARTED" (scripts_undo.go). The fix round's undo of A-3
// did not: it recorded A-3 unconfirmed, marked the record and recreated (or `set image`d) the executor although the
// forward move had never touched it. So a move refused for want of its marker restarted the executor anyway, and on
// k3d a pull that failed with the API server down turned a correct record into "unknown" with a sentence that said
// the move "was started".

// ⛔ THE MARKER HELPERS, AS SHIPPED: kept before this run's own mark, put back exactly as it was — an earlier run's
// marker byte for byte, and none where there was none — when the move never started.
func TestAC_D53_AMoveThatNeverStartedPutsTheMarkerBackAsItWas(t *testing.T) {
	requireBash(t)
	helpers := renderedVerdictHelpers(t)
	for _, c := range []struct{ name, before string }{
		{"an earlier run's marker", "image=ghcr.io/x/exec@sha256:v32\nversion=0.3.32\nrollback_to=0.3.32\nat=2026-09-29T10:00:00Z\n"},
		{"no marker", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			state, stage := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(stage, "undo"), 0o700); err != nil {
				t.Fatal(err)
			}
			if c.before != "" {
				writeMarker(t, state, "i1", c.before)
			}
			script := "set -euo pipefail\nSTAGE=" + shq(stage) + "\nROUTER_STATE=" + shq(state) +
				"\nINSTANCE=i1\nIMAGE_DIGEST=ghcr.io/x/exec@sha256:new\nTARGET_VERSION=0.3.34\nROLLBACK_TO=''\nFROM_VERSION=0.3.33\nFROM_IMAGE=ghcr.io/x/exec@sha256:v33\n" +
				"FROM_PREVIOUS_VERSION=0.3.32\nFROM_PREVIOUS_IMAGE=ghcr.io/x/exec@sha256:v32\nFROM_PREVIOUS_AT=2026-09-29T10:00:00Z\nFROM_ONE_OF=''\n" + helpers +
				"\nkeep_marker\nmark_unconfirmed\ngrep -q 'version=0.3.34' \"$ROUTER_STATE/installed/$INSTANCE.unconfirmed\"\nrestore_marker\n"
			if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
				t.Fatalf("the marker helpers did not run: %v\n%s", err, out)
			}
			got, err := os.ReadFile(unconfirmedPath(state, "i1"))
			if c.before == "" {
				if err == nil {
					t.Errorf("a marker is left where there was none:\n%s", got)
				}
				return
			}
			if string(got) != c.before {
				t.Errorf("the earlier marker was not put back as it was:\n got %q\nwant %q", got, c.before)
			}
		})
	}
}

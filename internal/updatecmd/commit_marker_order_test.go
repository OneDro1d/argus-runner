package updatecmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// commit_marker_order_test.go — AC-D53 merged with AC-D58: apply.sh's A-8a keeps the run's own word
// (never `unrecorded-…`) when the commit "failed" but the not-confirmed marker that stood when it began is gone —
// because CommitManifest removes the marker only AFTER WriteManifest has written the record. That order now decides an
// outcome word, so it is held here: a record that could not be written leaves the marker standing.
func TestCommitManifest_TheMarkerIsRemovedOnlyAfterTheRecordIsWritten(t *testing.T) {
	rs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rs, "installed"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := []byte("image=ghcr.io/x/exec@sha256:new\nversion=0.3.33\nfrom_version=0.3.32\nfrom_image=ghcr.io/x/exec@sha256:old\nat=2026-10-01T00:00:00Z\n")
	if err := os.WriteFile(unconfirmedPath(rs, "i1"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	// the record's path is a link into a folder that does not exist: reading it finds no record (absent is not an
	// error), writing it fails — so only WriteManifest fails, whoever runs the test (root included)
	if err := os.Symlink(filepath.Join(rs, "no-such-folder", "i1.json"), manifestPath(rs, "i1")); err != nil {
		t.Skipf("UNPROBED: cannot create a symlink here: %v", err)
	}
	next := Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.33", Executor: "ghcr.io/x/exec@sha256:new"}
	if _, err := CommitManifest(rs, next, CommitOpts{Outcome: "updated"}); err == nil {
		t.Fatal("precondition: the record could not be written, yet CommitManifest returned no error")
	}
	if _, err := os.Stat(unconfirmedPath(rs, "i1")); err != nil {
		t.Errorf("the marker is gone although no record was written (%v): A-8a would then say a record WAS written and keep the word `updated`", err)
	}

	// the other direction: once the record is written, the marker of a known executor is removed
	if err := os.Remove(manifestPath(rs, "i1")); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitManifest(rs, next, CommitOpts{Outcome: "updated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manifestPath(rs, "i1")); err != nil {
		t.Fatalf("no record after a commit that succeeded: %v", err)
	}
	if _, err := os.Stat(unconfirmedPath(rs, "i1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker still stands after the record of a known executor was written: %v", err)
	}
}

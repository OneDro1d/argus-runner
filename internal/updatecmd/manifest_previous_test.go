package updatecmd

import (
	"strings"
	"testing"
)

// V32 Release QA finding (d), the commit half. rehash_previous_test.go pins that the re-hash reads the
// executor it replaced; these pin that CommitManifest KEEPS that reading when there is no prior
// manifest to take `previous` from — and that a recorded manifest still wins when there is one.

func TestManifest_AFirstCommitKeepsThePreviousTheRehashRead(t *testing.T) {
	state := t.TempDir()
	next := Manifest{InstanceID: "i1", Tier: "k3d", Version: "0.3.32", Executor: "ghcr.io/x@sha256:new",
		Previous: &Previous{Version: "0.3.30", Image: "ghcr.io/x@sha256:old"}}

	got, err := CommitManifest(state, next, CommitOpts{Outcome: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Previous == nil || got.Previous.Version != "0.3.30" || got.Previous.Image != "ghcr.io/x@sha256:old" {
		t.Fatalf("the first manifest of a pre-0.3.32 instance dropped the previous version the re-hash read: %+v", got.Previous)
	}
	if got.Previous.UpdatedAt == "" {
		t.Error("previous.updated_at is empty — the page cannot say when the instance left that version")
	}
	onDisk, err := ReadManifest(state, "i1")
	if err != nil || onDisk.Previous == nil || onDisk.Previous.Version != "0.3.30" {
		t.Errorf("the manifest on disk lost previous (%v): %+v", err, onDisk.Previous)
	}
	if !RollbackOffered(got) {
		t.Error("0.3.30 below 0.3.32: the rollback block must be offered after the first update")
	}
}

// ⛔ A RECORDED MANIFEST IS THE AUTHORITY. The re-hash's reading is a fallback for the one case with no
// record at all; where the machine already recorded what it ran, that record decides.
func TestManifest_ARecordedManifestIsTheAuthorityForPrevious(t *testing.T) {
	state := t.TempDir()
	if err := WriteManifest(state, Manifest{InstanceID: "i1", Tier: "k3d", Version: "0.3.31", Executor: "ghcr.io/x@sha256:mid"}); err != nil {
		t.Fatal(err)
	}
	next := Manifest{InstanceID: "i1", Tier: "k3d", Version: "0.3.32", Executor: "ghcr.io/x@sha256:new",
		Previous: &Previous{Version: "0.3.30", Image: "ghcr.io/x@sha256:old"}}

	got, err := CommitManifest(state, next, CommitOpts{Outcome: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Previous == nil || got.Previous.Version != "0.3.31" || got.Previous.Image != "ghcr.io/x@sha256:mid" {
		t.Errorf("previous = %+v, want the RECORDED 0.3.31 / sha256:mid, not the re-hash's fallback reading", got.Previous)
	}
}

// ⛔ AC-D49 (#296): RollbackOffered's doc comment and HostRollbackCommand's both promise "no block,
// and the reason is stated" — this is that reason, and it must name both versions except where there
// is truly nothing recorded yet.
func TestRollbackReason(t *testing.T) {
	cases := []struct {
		name       string
		m          Manifest
		wantEmpty  bool
		wantSubstr []string // every one of these must appear in the reason
	}{
		{
			name:      "never updated: nothing to explain",
			m:         Manifest{InstanceID: "i1"}, // no Version at all
			wantEmpty: true,
		},
		{
			name:       "no previous recorded, but the instance IS on a known version",
			m:          Manifest{InstanceID: "i1", Version: "0.3.32"},
			wantSubstr: []string{"no previous version is recorded"},
		},
		{
			name: "just rolled back: previous equals installed",
			m: Manifest{InstanceID: "i1", Version: "0.3.30",
				Previous: &Previous{Version: "0.3.30"}},
			wantSubstr: []string{"0.3.30"},
		},
		{
			name: "previous is newer: would move forward, not back",
			m: Manifest{InstanceID: "i1", Version: "0.3.30",
				Previous: &Previous{Version: "0.3.32"}},
			wantSubstr: []string{"0.3.30", "0.3.32"},
		},
		{
			name: "AC-D49: neither side (or one side) cannot be ordered",
			m: Manifest{InstanceID: "i1", Version: "0.3.32",
				Previous: &Previous{Version: "dev"}},
			wantSubstr: []string{"dev", "0.3.32"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if RollbackOffered(c.m) {
				t.Fatal("test setup error: this case must NOT be offered, or RollbackReason has nothing to explain")
			}
			got := RollbackReason(c.m)
			if c.wantEmpty {
				if got != "" {
					t.Errorf("RollbackReason = %q, want empty (nothing has ever been recorded)", got)
				}
				return
			}
			if got == "" {
				t.Fatal("RollbackReason is empty for a withheld offer — the page and the CLI would show nothing")
			}
			for _, s := range c.wantSubstr {
				if !strings.Contains(got, s) {
					t.Errorf("RollbackReason = %q, want it to name %q", got, s)
				}
			}
		})
	}
}

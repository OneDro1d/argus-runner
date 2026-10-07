package updatecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifest_unconfirmed_test.go — AC-D53 criterion (k): AFTER AN UNCONFIRMED EXECUTOR MOVE, THE RECORD READS THE
// EXECUTOR "unknown" — ALSO WHEN NOTHING COULD REWRITE THE RECORD.
//
// On compose, rollback-unreachable means the container runtime stopped answering; `update rehash` and `update
// commit` run in containers too, so A-8a cannot write the "unknown" record, and the record from before the update
// stays — naming the version before for an executor that may be on the new one. apply.sh therefore writes a plain
// marker beside the record BEFORE the move (no runtime needed), and every reader goes through ReadManifest.

const (
	oldImg = "ghcr.io/x/exec@sha256:old"
	newImg = "ghcr.io/x/exec@sha256:new"
)

func recordBefore(t *testing.T, state string) {
	t.Helper()
	if err := WriteManifest(state, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31", Executor: oldImg,
		Artefacts: []Artefact{
			{Kind: "executor", Name: "executor", Version: "0.3.31", Image: oldImg},
			{Kind: "kit", Name: "kit", Version: "0.3.31"},
		}}); err != nil {
		t.Fatal(err)
	}
}

func markUnconfirmed(t *testing.T, state, id, img string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(state, "installed"), 0o700); err != nil {
		t.Fatal(err)
	}
	// the exact bytes apply.sh's mark_unconfirmed writes
	if err := os.WriteFile(unconfirmedPath(state, id), []byte("image="+img+"\nat=2026-09-30T13:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestManifest_AC_D53_AMarkerMakesTheExecutorUnknown(t *testing.T) {
	state := t.TempDir()
	recordBefore(t, state)
	markUnconfirmed(t, state, "i1", newImg)

	m, err := ReadManifest(state, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if m.InstanceID != "i1" {
		t.Fatalf("the record is present and was read as absent (instance %q)", m.InstanceID)
	}
	if m.Version != "" || m.Executor != "" {
		t.Errorf("⛔ with the executor move unconfirmed the record still asserts version %q / executor %q — "+
			"the version before, for an executor that may be on the new one", m.Version, m.Executor)
	}
	for _, a := range m.Artefacts {
		switch a.Kind {
		case "executor":
			if a.Version != "unknown" || a.Image != "" {
				t.Errorf("the executor artefact says version %q image %q, want unknown and no image", a.Version, a.Image)
			}
		case "kit":
			if a.Version != "0.3.31" {
				t.Errorf("the marker is about the executor only, and it changed the kit to %q", a.Version)
			}
		}
	}
	if m.Unconfirmed == nil || m.Unconfirmed.Image != newImg {
		t.Errorf("the image the unconfirmed move was going to is not carried (%+v) — `argus up` needs it to re-run the update", m.Unconfirmed)
	}
}

// ⛔ THE MARKER ALONE IS NOT A RECORD. An instance onboarded before 0.3.32 has none; the marker must not turn
// `argus up`'s first run into a second run.
func TestManifest_AC_D53_AMarkerWithNoRecordKeepsTheInstanceAbsent(t *testing.T) {
	state := t.TempDir()
	markUnconfirmed(t, state, "i1", newImg)
	m, err := ReadManifest(state, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if m.InstanceID != "" {
		t.Errorf("a marker with no record was read as a record for %q", m.InstanceID)
	}
	if m.Unconfirmed == nil || m.Unconfirmed.Image != newImg {
		t.Errorf("the marker was not read (%+v)", m.Unconfirmed)
	}
}

func TestManifest_AC_D53_NoMarkerChangesNothing(t *testing.T) {
	state := t.TempDir()
	recordBefore(t, state)
	m, err := ReadManifest(state, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "0.3.31" || m.Executor != oldImg || m.Unconfirmed != nil {
		t.Errorf("with no marker the record changed: version %q executor %q unconfirmed %+v", m.Version, m.Executor, m.Unconfirmed)
	}
}

// ⛔ A COMMIT WITH A KNOWN EXECUTOR REMOVES THE MARKER — and records `previous` from where the marker says the move
// came from, never from ReadManifest's "unknown": every update writes the marker before its move, so a commit that
// saw "unknown" as the prior version would stop recording where the instance came from, and no rollback would ever
// be offered again. (The marker is this update's own, exactly as mark_unconfirmed writes it.)
func TestManifest_AC_D53_ACommitWithAKnownExecutorRemovesTheMarker(t *testing.T) {
	state := t.TempDir()
	recordBefore(t, state)
	writeMarker(t, state, "i1", markerBody(newImg, "0.3.32", "", "0.3.31", oldImg))

	got, err := CommitManifest(state, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32", Executor: newImg},
		CommitOpts{Outcome: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unconfirmedPath(state, "i1")); !os.IsNotExist(err) {
		t.Errorf("the marker is still there after a commit of a known executor (stat: %v)", err)
	}
	if got.Previous == nil || got.Previous.Version != "0.3.31" || got.Previous.Image != oldImg {
		t.Errorf("previous is %+v, want 0.3.31 / %s — the commit read the marker's view instead of the record", got.Previous, oldImg)
	}
	back, _ := ReadManifest(state, "i1")
	if back.Version != "0.3.32" || back.Unconfirmed != nil {
		t.Errorf("read back version %q unconfirmed %+v, want 0.3.32 and no marker", back.Version, back.Unconfirmed)
	}
}

// ⛔ A COMMIT OF AN UNKNOWN EXECUTOR KEEPS IT: the runtime came back in time for the re-hash, which recorded the
// executor unknown — the marker still carries the image the move was going to, for the re-run.
func TestManifest_AC_D53_ACommitOfAnUnknownExecutorKeepsTheMarker(t *testing.T) {
	state := t.TempDir()
	recordBefore(t, state)
	markUnconfirmed(t, state, "i1", newImg)

	if _, err := CommitManifest(state, Manifest{InstanceID: "i1", Tier: "compose"},
		CommitOpts{Outcome: "rollback-unreachable", FailedStep: "A-3"}); err != nil {
		t.Fatal(err)
	}
	back, err := ReadManifest(state, "i1")
	if err != nil {
		t.Fatal(err)
	}
	if back.Unconfirmed == nil || back.Unconfirmed.Image != newImg {
		t.Errorf("the marker was dropped by a commit that could not confirm the executor (%+v)", back.Unconfirmed)
	}
	if back.Version != "" {
		t.Errorf("read back version %q, want unknown", back.Version)
	}
}

// ⛔ DESIGN STEP 8: THE COMMIT KEEPS A-3's HEALTH VERDICT. It rewrites last_outcome's word, time and step from the
// commit's own flags; the verdict the re-hash put there must survive that, or the report never carries it.
func TestManifest_AC_D53_ACommitKeepsTheHealthVerdict(t *testing.T) {
	state := t.TempDir()
	got, err := CommitManifest(state, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31",
		LastOutcome: &LastOutcome{Health: "unreachable", Detail: "Cannot connect to the Docker daemon"}},
		CommitOpts{Outcome: "rolled-back", FailedStep: "A-3"})
	if err != nil {
		t.Fatal(err)
	}
	if got.LastOutcome == nil || got.LastOutcome.Word != "rolled-back" || got.LastOutcome.Health != "unreachable" ||
		got.LastOutcome.Detail != "Cannot connect to the Docker daemon" {
		t.Errorf("the commit's last_outcome is %+v — the verdict was dropped", got.LastOutcome)
	}
	back, _ := ReadManifest(state, "i1")
	if back.LastOutcome == nil || back.LastOutcome.Health != "unreachable" {
		t.Errorf("read back: %+v", back.LastOutcome)
	}
}

// ⛔ BR-6: AN UNKNOWN VERSION AFTER AN UPDATE IS NOT "NEVER UPDATED". RollbackReason's "" means no record at all;
// after an unconfirmed move the page must say why no rollback is offered.
func TestManifest_AC_D53_TheRollbackReasonForAnUnknownVersion(t *testing.T) {
	if r := RollbackReason(Manifest{}); r != "" {
		t.Errorf("an instance with no record got a reason %q — that is the ordinary state before a first update", r)
	}
	r := RollbackReason(Manifest{InstanceID: "i1", LastOutcome: &LastOutcome{Word: "rollback-unreachable", FailedStep: "A-3"}})
	if r == "" || !strings.Contains(r, "unknown") || !strings.Contains(r, "rollback-unreachable") {
		t.Errorf("the reason for an unknown version after rollback-unreachable is %q — it must say the version is unknown and why", r)
	}
}

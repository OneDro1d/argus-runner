package updatecmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// V31-001 1-MANIFEST (VR13-UP) — WHAT IS INSTALLED, WRITTEN BY ONE WRITER.
//
// The row's subject is that an update left things stale and nobody could say WHICH. The manifest is
// the answer: per artefact, what version is on this machine, and when it got there. It is written
// by onboarding's last step and by `update commit`, and by nothing else — two writers would
// disagree, and the disagreement would surface as a page that contradicts the machine.
//
// ⛔ IT IS A RE-HASH OF REALITY, NEVER A COPY OF THE PLAN. On `rollback-failed` especially: the plan
// says what was meant to happen, and the whole point of that outcome is that it did not.

func TestManifest_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		InstanceID: "memstore-compose",
		Tier:       "compose",
		Version:    "0.3.32",
		Artefacts: []Artefact{
			{Kind: "executor", Name: "onedroid-argus-execution-plane", Version: "0.3.32", Image: "ghcr.io/x@sha256:new"},
			{Kind: "skills", Name: "scenario-author", Version: "0.3.32", Hash: "sha256:abc"},
			{Kind: "obs_stack", Name: "loki", Version: "2.9.0", Image: "grafana/loki@sha256:l", Runtime: "compose_service"},
		},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest(dir, "memstore-compose")
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Version != "0.3.32" || len(got.Artefacts) != 3 {
		t.Fatalf("round trip lost something: %+v", got)
	}
	if got.Artefacts[2].Runtime != "compose_service" {
		t.Errorf("the obs_stack runtime discriminator must survive: %+v", got.Artefacts[2])
	}
}

// ⛔ THE RULE THE ROLLBACK BLOCK DEPENDS ON. `previous` records what the machine was on BEFORE a
// forward update. A rollback must NOT overwrite it — otherwise rolling back to 0.3.31 would record
// `previous = 0.3.32`, the page would offer a rollback TO the version just left, and an operator
// could ping-pong between two versions forever without either being wrong on its face.
func TestManifest_ARollbackDoesNotOverwritePrevious(t *testing.T) {
	dir := t.TempDir()
	// a record carries the executor it ran — pinned, as every commit records it; only a pinned image can become
	// `previous` (the V32 rule, manifest.go pinnedPrevious)
	base := Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31", Executor: "ghcr.io/x@sha256:old"}
	if err := WriteManifest(dir, base); err != nil {
		t.Fatal(err)
	}

	// a forward update to 0.3.32 records where we came from
	fwd, err := CommitManifest(dir, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		Executor: "ghcr.io/x@sha256:new"}, CommitOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if fwd.Previous == nil || fwd.Previous.Version != "0.3.31" {
		t.Fatalf("a forward update must record the version it left: %+v", fwd.Previous)
	}

	// …and rolling back does NOT move it
	back, err := CommitManifest(dir, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31",
		Executor: "ghcr.io/x@sha256:old"}, CommitOpts{RollbackTo: "0.3.31"})
	if err != nil {
		t.Fatal(err)
	}
	if back.Previous == nil || back.Previous.Version != "0.3.31" {
		t.Fatalf("a rollback must LEAVE previous alone, got %+v", back.Previous)
	}
	// and the consequence the page reads: previous == version, so no rollback block renders
	if back.Previous.Version != back.Version {
		t.Errorf("after a rollback previous.version must equal version (%q vs %q) — that is what stops the page offering a rollback to the version just left",
			back.Previous.Version, back.Version)
	}
	if RollbackOffered(back) {
		t.Error("no rollback block may be offered after a rollback")
	}
}

func TestManifest_RollbackIsOfferedOnlyWhenPreviousIsOlder(t *testing.T) {
	older := Manifest{Version: "0.3.32", Previous: &Previous{Version: "0.3.31"}}
	if !RollbackOffered(older) {
		t.Error("a previous version BELOW the installed one is exactly when the block belongs")
	}
	for _, m := range []Manifest{
		{Version: "0.3.32"}, // never updated
		{Version: "0.3.32", Previous: &Previous{Version: "0.3.32"}},  // just rolled back
		{Version: "0.3.31", Previous: &Previous{Version: "0.3.32"}},  // previous is NEWER
		{Version: "0.3.32", Previous: &Previous{Version: "not-sem"}}, // unparseable
	} {
		if RollbackOffered(m) {
			t.Errorf("no block may be offered for %+v", m)
		}
	}
}

// C-15 — on `rollback-failed` the manifest is a FRESH RE-HASH of what is really installed, never the
// plan. The outcome exists precisely because the machine is not where either version says it should
// be, so writing the plan would be writing a comfortable fiction over the one state that needs the
// truth.
func TestManifest_LastOutcomeRecordsWhatHappened(t *testing.T) {
	dir := t.TempDir()
	if err := WriteManifest(dir, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31"}); err != nil {
		t.Fatal(err)
	}
	got, err := CommitManifest(dir, Manifest{InstanceID: "i1", Tier: "compose", Version: "0.3.31"},
		CommitOpts{Outcome: "rollback-failed", FailedStep: "A-3"})
	if err != nil {
		t.Fatal(err)
	}
	if got.LastOutcome == nil || got.LastOutcome.Word != "rollback-failed" || got.LastOutcome.FailedStep != "A-3" {
		t.Fatalf("last_outcome must name the word AND the step: %+v", got.LastOutcome)
	}
	if got.LastOutcome.At == "" {
		t.Error("…and when, or 'as of the last update' on the page means nothing")
	}
}

// ⛔ NO SECRET EVER REACHES THIS FILE. It sits on disk beside the kit, the control plane holds a
// copy, and the page renders from it — three places a token must never be.
func TestManifest_CarriesNoSecret(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		Artefacts: []Artefact{{Kind: "executor", Name: "exec", Version: "0.3.32", Image: "ghcr.io/x@sha256:d"}},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "installed", "i1.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "TOKEN", "password", "secret", "identity_key", "Bearer"} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("the manifest carries %q:\n%s", forbidden, b)
		}
	}
	// …and the shape is a plain object a human can read
	var any map[string]any
	if err := json.Unmarshal(b, &any); err != nil {
		t.Fatalf("the manifest must be readable JSON: %v", err)
	}
}

func TestManifest_MissingFileIsNotAnError(t *testing.T) {
	// An instance onboarded before 0.3.32 has no manifest, and that is a fact about the estate
	// rather than a failure: `update plan` reconstructs one. Reading must say "absent", not "broken".
	got, err := ReadManifest(t.TempDir(), "never-onboarded")
	if err != nil {
		t.Fatalf("a missing manifest must not be an error: %v", err)
	}
	if got.InstanceID != "" {
		t.Errorf("an absent manifest must come back empty: %+v", got)
	}
}

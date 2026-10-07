package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/updatecmd"
)

// update_rehash_test.go — V31-001 (VR13-UP) C-15, THROUGH THE CLI.
//
// ⭐ THIS VERB EXISTS BECAUSE A-8a CONSUMED A FILE NOTHING WROTE. `apply.sh` ended with
// `update commit --manifest "$STAGE/manifest.next.json"` and no step produced it; the execution tests
// only passed because the fixture wrote one by hand. Writing the producer forced the real question:
// WHICH state does the manifest record?
//
// ⛔ NOT THE PLAN. The plan says where the machine was going. `progress.json` says what happened.

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRehash_RecordsWhatProgressSaysHappened(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	stage := t.TempDir()
	plan := updatecmd.Plan{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		ImageDigest: "ghcr.io/x/exec@sha256:new", GeneratedAt: "2026-09-13T00:00:00Z",
		Steps: []updatecmd.Step{{ID: "A-1"}, {ID: "A-3"}, {ID: "A-5"}},
		Artefacts: []updatecmd.Artefact{
			{Kind: "kit", Name: "argus-kit", Version: "0.3.32"},
			{Kind: "executor", Name: "argus-executor-i1", Version: "0.3.32"},
			{Kind: "skills", Name: "/c/agents/product", Version: "0.3.32"},
		},
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "observed.json"),
		[]byte(`{"tier":"compose","env":{"ARGUS_INSTALLED_VERSION":"0.3.31"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A-1 and A-3 applied; A-5 was SKIPPED by the shared-folder exception.
	writeLines(t, filepath.Join(stage, "progress.json"),
		`{"id":"A-1","state":"applied"}`,
		`{"id":"A-3","state":"applied"}`,
		`{"id":"A-5","state":"skipped"}`)

	if code := dispatch([]string{"update", "rehash", "--stage", stage, "--outcome", "updated"}); code != exitOK {
		t.Fatalf("update rehash exited %d", code)
	}

	var m updatecmd.Manifest
	raw, err := os.ReadFile(filepath.Join(stage, "manifest.next.json"))
	if err != nil {
		t.Fatalf("manifest.next.json was not written — A-8a would then commit a file that does not exist: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, a := range m.Artefacts {
		got[a.Kind] = a.Version
	}
	if got["kit"] != "0.3.32" || got["executor"] != "0.3.32" {
		t.Errorf("the steps that applied are not recorded at the target version: %v", got)
	}
	// ⛔ THE SKIPPED ONE KEEPS THE VERSION THE MACHINE REALLY HAS. Recording it at the target would
	// tell an operator a folder is current that was deliberately left behind — and it would clear the
	// amber chip that exists to say so.
	if got["skills"] != "0.3.31" {
		t.Errorf("A-5 was SKIPPED and the skills read %q, want 0.3.31 (what the machine really has)", got["skills"])
	}
	if m.LastOutcome == nil || m.LastOutcome.Word != "updated" {
		t.Errorf("last_outcome = %+v", m.LastOutcome)
	}
}

// ⛔ NO progress.json AT ALL MEANS NOTHING APPLIED. A preflight abort leaves none, and the honest
// manifest for that is "nothing moved" — not "everything did".
func TestUpdateRehash_NoProgressMeansNothingMoved(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	stage := t.TempDir()
	plan := updatecmd.Plan{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		ImageDigest: "ghcr.io/x/exec@sha256:new",
		Steps:       []updatecmd.Step{{ID: "A-1"}, {ID: "A-3"}},
		Artefacts: []updatecmd.Artefact{
			{Kind: "kit", Name: "argus-kit", Version: "0.3.32"},
			{Kind: "executor", Name: "argus-executor-i1", Version: "0.3.32"},
		},
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "observed.json"),
		[]byte(`{"tier":"compose","env":{"ARGUS_INSTALLED_VERSION":"0.3.31"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := dispatch([]string{"update", "rehash", "--stage", stage, "--outcome", "untouched"}); code != exitOK {
		t.Fatalf("update rehash exited %d with no progress file", code)
	}
	raw, _ := os.ReadFile(filepath.Join(stage, "manifest.next.json"))
	var m updatecmd.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, a := range m.Artefacts {
		if a.Version == "0.3.32" {
			t.Errorf("%s is recorded at the target version although no step ever reported applying — "+
				"absence read as success, which is the exact reading this release removes", a.Kind)
		}
	}
	if m.Version != "0.3.31" {
		t.Errorf("the manifest header reads %q; nothing moved, so the instance is still on 0.3.31", m.Version)
	}
}

// A step recorded applied and then UNDONE is undone: the last record for a step wins.
func TestUpdateRehash_TheLastRecordForAStepWins(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	stage := t.TempDir()
	plan := updatecmd.Plan{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32", ImageDigest: "ghcr.io/x/exec@sha256:new",
		Steps:     []updatecmd.Step{{ID: "A-1"}},
		Artefacts: []updatecmd.Artefact{{Kind: "kit", Name: "argus-kit", Version: "0.3.32"}},
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "observed.json"),
		[]byte(`{"tier":"compose","env":{"ARGUS_INSTALLED_VERSION":"0.3.31"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// applied, then the trap undid it — which is exactly what a rollback writes.
	writeLines(t, filepath.Join(stage, "progress.json"),
		`{"id":"A-1","state":"applied"}`,
		`{"id":"A-1","state":"undone"}`)

	if code := dispatch([]string{"update", "rehash", "--stage", stage, "--outcome", "rolled-back",
		"--failed-step", "A-3"}); code != exitOK {
		t.Fatal("update rehash refused a rollback's progress file")
	}
	raw, _ := os.ReadFile(filepath.Join(stage, "manifest.next.json"))
	var m updatecmd.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, a := range m.Artefacts {
		if a.Version == "0.3.32" {
			t.Fatalf("%s reads 0.3.32 after its step was applied AND THEN UNDONE — the first record won, "+
				"so the page would report a version the machine is not running", a.Kind)
		}
	}
	if m.LastOutcome == nil || m.LastOutcome.FailedStep != "A-3" {
		t.Errorf("the failing step is not carried: %+v", m.LastOutcome)
	}
}

// ⛔ AC-D53 (k), THROUGH THE CLI: an executor whose move could be neither confirmed nor undone is recorded
// UNKNOWN — the progress file exactly as apply.sh writes it for a runtime that stopped answering after the
// recreate (the health fields ride along and are not in the way), then the kit's clean undo.
func TestUpdateRehash_AnUnconfirmedExecutorIsRecordedUnknown(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "")
	t.Setenv("ARGUS_TOKEN", "")

	stage := t.TempDir()
	plan := updatecmd.Plan{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32", ImageDigest: "ghcr.io/x/exec@sha256:new",
		Steps: []updatecmd.Step{{ID: "A-1"}, {ID: "A-3"}},
		Artefacts: []updatecmd.Artefact{
			{Kind: "kit", Name: "argus-kit", Version: "0.3.32"},
			{Kind: "executor", Name: "executor", Version: "0.3.32", Image: "ghcr.io/x/exec@sha256:new"},
		},
	}
	b, _ := json.MarshalIndent(plan, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, "plan.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "observed.json"),
		[]byte(`{"tier":"compose","executor_version":"0.3.31","env":{"ARGUS_INSTALLED_VERSION":"0.3.31"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(stage, "progress.json"),
		`{"id":"A-1","state":"applied","at":"2026-09-30T10:00:00Z"}`,
		`{"id":"A-3","state":"unconfirmed","at":"2026-09-30T10:00:01Z"}`,
		`{"id":"A-3","state":"unconfirmed","health":"unreachable","detail":"docker could not list the executor of argus-inst-i1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?","at":"2026-09-30T10:03:01Z"}`,
		`{"id":"A-1","state":"undone","at":"2026-09-30T10:03:02Z"}`)

	if code := dispatch([]string{"update", "rehash", "--stage", stage, "--outcome", "rollback-unreachable",
		"--failed-step", "A-3"}); code != exitOK {
		t.Fatal("update rehash refused a progress file carrying an unconfirmed step")
	}
	raw, _ := os.ReadFile(filepath.Join(stage, "manifest.next.json"))
	var m updatecmd.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "" || m.Executor != "" {
		t.Errorf("the header asserts version %q executor %q for an executor nobody could confirm — want both unknown (empty)", m.Version, m.Executor)
	}
	got := map[string]string{}
	for _, a := range m.Artefacts {
		got[a.Kind] = a.Version
	}
	if got["executor"] != "unknown" {
		t.Errorf("the executor artefact reads %q, want unknown — neither the previous 0.3.31 nor the target 0.3.32", got["executor"])
	}
	if got["kit"] != "0.3.31" {
		t.Errorf("the kit was undone cleanly and reads %q, want 0.3.31", got["kit"])
	}
	if m.LastOutcome == nil || m.LastOutcome.Word != "rollback-unreachable" || m.LastOutcome.FailedStep != "A-3" {
		t.Errorf("last_outcome = %+v", m.LastOutcome)
	}
}

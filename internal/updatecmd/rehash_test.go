package updatecmd

import (
	"testing"
)

// rehash_test.go — V31-001 (VR13-UP) C-15: THE MANIFEST IS WHAT HAPPENED, NOT WHAT WAS PLANNED.
//
// ⭐ A-8a CONSUMED A FILE NOTHING WROTE. `apply.sh` ends with
// `update commit --manifest "$STAGE/manifest.next.json"`, and no step produced it — the execution
// tests only passed because the fixture wrote one by hand. This is the producer, and writing it
// forced the question the row is actually about: WHICH state does it record?
//
// ⛔ THE PLAN IS NOT THE ANSWER. The plan describes where the machine was going. On a skip it did not
// go there, on a rollback it went and came back, and on `rollback-failed` it is somewhere neither
// version describes. Writing the plan would lay a comfortable fiction over the one outcome that needs
// the truth — so every artefact is resolved against what PROGRESS says actually happened to its step.

func planWithSteps(steps ...Step) Plan {
	return Plan{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32",
		ImageDigest: "ghcr.io/x/exec@sha256:new", Steps: steps,
		Artefacts: []Artefact{
			{Kind: "executor", Name: "argus-executor-i1", Version: "0.3.32", Image: "ghcr.io/x/exec@sha256:new"},
			{Kind: "kit", Name: "argus-kit", Version: "0.3.32"},
			{Kind: "skills", Name: "product folder", Version: "0.3.32", Loc: &Loc{HostPath: "/c/agents/product"}},
		},
	}
}

// The happy path: every step applied, so every artefact is on the target version.
func TestRehash_EverythingAppliedRecordsTheTargetVersion(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"}, Step{ID: "A-5"})
	prog := map[string]string{"A-1": "applied", "A-3": "applied", "A-5": "applied"}

	m := Rehash(p, prog, Observed{Tier: "compose"}, "updated", "")
	for _, a := range m.Artefacts {
		if a.Version != "0.3.32" {
			t.Errorf("%s is on %q after a clean apply", a.Kind, a.Version)
		}
	}
	if m.Version != "0.3.32" || m.InstanceID != "i1" {
		t.Errorf("manifest header = %s/%s", m.InstanceID, m.Version)
	}
}

// ⛔ A SKIPPED STEP'S ARTEFACT KEEPS THE VERSION IT REALLY HAS. This is 1-SKILLS on the recording
// side: the shared-folder exception SKIPS A-5, so the skills are still on the old release. Recording
// them at the target would be the page telling an operator a folder is current that is not — and it
// would clear the amber chip that exists to say so.
func TestRehash_ASkippedStepKeepsTheVersionOnTheMachine(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"},
		Step{ID: "A-5", SkipReason: "shared with i2 (0.3.31)"})
	prog := map[string]string{"A-1": "applied", "A-3": "applied", "A-5": "skipped"}
	obs := Observed{Tier: "compose"}

	m := Rehash(p, prog, obs, "updated", "")
	got := map[string]string{}
	for _, a := range m.Artefacts {
		got[a.Kind] = a.Version
	}
	if got["skills"] == "0.3.32" {
		t.Fatalf("A-5 was SKIPPED and the skills are recorded at the target version anyway (%v). The "+
			"page would show a folder as current that was deliberately left behind, and the amber chip "+
			"that exists to say so would clear.", got)
	}
	if got["executor"] != "0.3.32" || got["kit"] != "0.3.32" {
		t.Errorf("the steps that DID apply are not recorded as applied: %v", got)
	}
}

// ⛔ AN UNDONE STEP IS NOT AN APPLIED ONE. After a rollback the artefact is back where it started, and
// the manifest has to say so or the page reports a version the machine is not running.
func TestRehash_AnUndoneStepIsRecordedAsNotMoved(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"})
	prog := map[string]string{"A-1": "undone", "A-3": "undone"}

	m := Rehash(p, prog, Observed{Tier: "compose"}, "rolled-back", "")
	for _, a := range m.Artefacts {
		if a.Version == "0.3.32" {
			t.Errorf("%s is recorded at the target version after its step was UNDONE", a.Kind)
		}
	}
	if m.LastOutcome == nil || m.LastOutcome.Word != "rolled-back" {
		t.Errorf("last_outcome = %+v", m.LastOutcome)
	}
}

// ⭐ `rollback-failed` IS THE CASE THE WHOLE RULE EXISTS FOR. One step applied, another could not be
// undone: the machine is where NEITHER version says it should be, and the per-artefact record is the
// only place an operator can read which half is which.
func TestRehash_RollbackFailedRecordsTheMixedStatePerArtefact(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"}, Step{ID: "A-5"})
	prog := map[string]string{"A-1": "undone", "A-3": "applied", "A-5": "undone"}

	m := Rehash(p, prog, Observed{Tier: "compose"}, "rollback-failed", "A-5")
	got := map[string]string{}
	for _, a := range m.Artefacts {
		got[a.Kind] = a.Version
	}
	if got["executor"] != "0.3.32" {
		t.Errorf("the half that stuck (A-3, the executor) is not recorded as moved: %v", got)
	}
	if got["kit"] == "0.3.32" {
		t.Errorf("the half that came back (A-1, the kit) is recorded as moved: %v", got)
	}
	if m.LastOutcome == nil || m.LastOutcome.FailedStep != "A-5" {
		t.Fatalf("the failing step is not named: %+v", m.LastOutcome)
	}
}

// ⛔ A STEP WITH NO PROGRESS ENTRY DID NOT HAPPEN. The trap fires mid-run, so steps after the failure
// never ran at all — and "no record" must not read as "applied".
func TestRehash_AStepWithNoRecordDidNotHappen(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"}, Step{ID: "A-5"})
	prog := map[string]string{"A-1": "applied"} // A-3 failed before recording; A-5 never ran

	m := Rehash(p, prog, Observed{Tier: "compose"}, "rolled-back", "A-3")
	for _, a := range m.Artefacts {
		if a.Kind == "skills" && a.Version == "0.3.32" {
			t.Fatal("a step that never ran was recorded as applied — absence read as success, which is " +
				"the exact reading this release exists to remove")
		}
	}
}

// ⛔ AND NO CREDENTIAL REACHES IT. The manifest is POSTed to the control plane and rendered on a page.
func TestRehash_CarriesNoCredential(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"})
	obs := Observed{Tier: "compose", Env: map[string]string{
		"ARGUS_RUNNER_TOKEN":     "rt_LIVE_TOKEN_VALUE",
		"ARGUS_IDENTITY_KEY_B64": "aXRJc0FLZXk=",
		"ARGUS_MCP_PORT":         "8765",
	}}
	m := Rehash(p, map[string]string{"A-1": "applied"}, obs, "updated", "")
	b, err := marshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"rt_LIVE_TOKEN_VALUE", "aXRJc0FLZXk="} {
		if containsStr(string(b), secret) {
			t.Errorf("a credential reached the manifest: %q", secret)
		}
	}
}

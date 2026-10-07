package updatecmd

import (
	"testing"
)

// manifest_versions_test.go — V31-001 (VR13-UP), P-11: EVERY ARTEFACT CARRIES ITS REAL VERSION.
//
// ⭐ THIS IS THE TEST THAT MAKES 1-SKILLS WORK AT ALL. The shared-folder rule reads "absent = unknown
// → held back, said so". That is the right default for something nobody can measure — and it is a
// TRAP if the manifest is lazy about versions, because every artefact left blank would hold every
// shared folder back forever and the amber chip would never clear. A rule that fails safe still has
// to be given real data.
//
// ⛔ "unknown" IS LEGAL IN EXACTLY TWO PLACES (PO): a THIRD-PARTY obs program, whose version is not
// ours to read, and an instance RECONSTRUCTED from observed.json, where the machine genuinely never
// recorded one. Anywhere else it is a manifest that was not filled in.

// ourKinds are the artefacts WE install and therefore know the version of. A blank version on any of
// these is a defect in whatever wrote the manifest, not a fact about the machine.
var ourKinds = []string{"executor", "kit", "skills", "agentcfg", "promtail_cfg", "k8s_object"}

func TestManifest_EveryArtefactCarriesItsRealVersionAfterApply(t *testing.T) {
	state := t.TempDir()

	// what an apply to 0.3.32 commits: a re-hash of what is really installed now.
	next := Manifest{
		InstanceID: "i1", Tier: "compose", Version: "0.3.32", Executor: "ghcr.io/x@sha256:new",
		Artefacts: []Artefact{
			{Kind: "executor", Name: "argus-executor-i1", Version: "0.3.32", Image: "ghcr.io/x@sha256:new"},
			{Kind: "kit", Name: "argus-kit", Version: "0.3.32"},
			{Kind: "skills", Name: "product folder", Version: "0.3.32", Loc: &Loc{HostPath: "/c/agents/product"}},
			{Kind: "agentcfg", Name: ".mcp.json", Version: "0.3.32"},
			{Kind: "promtail_cfg", Name: "promtail-config.i1.yaml", Version: "0.3.32"},
			// ⛔ THE LEGAL "unknown": a third-party program. Its version is not ours to read, and
			// recording that honestly is better than recording a number we made up.
			{Kind: "obs_stack", Name: "loki", Version: "unknown", Runtime: "compose_service"},
		},
	}
	if _, err := CommitManifest(state, next, CommitOpts{Outcome: "updated"}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(state, "i1")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, a := range got.Artefacts {
		seen[a.Kind] = true
		if a.Version == "" {
			t.Errorf("%s/%s carries NO version. An empty version is not the same as \"unknown\": the "+
				"held-back rule treats not-knowing as a reason to leave a folder alone, so a blank here "+
				"keeps a shared folder amber forever.", a.Kind, a.Name)
			continue
		}
		if a.Version == "unknown" && isOurs(a.Kind) {
			t.Errorf("%s/%s reports \"unknown\". That is legal only for a THIRD-PARTY obs program or an "+
				"instance reconstructed from observed.json — this is one WE install, so not knowing its "+
				"version means the manifest was not filled in.", a.Kind, a.Name)
		}
	}
	for _, k := range ourKinds {
		if k == "k8s_object" {
			continue // compose instance
		}
		if !seen[k] {
			t.Errorf("the manifest records no %q artefact at all — the page cannot report a mixed state "+
				"for something it is never told about", k)
		}
	}

	// ⭐ AND THE VERSIONS ARE THE TARGET'S, not the ones it came from. This is what the page reads to
	// say "these three moved and that one did not".
	for _, a := range got.Artefacts {
		if isOurs(a.Kind) && a.Version != "0.3.32" {
			t.Errorf("%s/%s is on %q after an apply to 0.3.32 — an artefact that did not move must be "+
				"recorded as not moved, and one that did must say so", a.Kind, a.Name, a.Version)
		}
	}
}

// ⛔ A HALF-DONE APPLY RECORDS THE MIXED STATE, PER ARTEFACT. On `rollback-failed` the machine is
// where NEITHER version says it should be, and the manifest is the only place an operator can read
// which half is which.
func TestManifest_AMixedStateIsRecordedPerArtefact(t *testing.T) {
	state := t.TempDir()
	rehash := Manifest{
		InstanceID: "i1", Tier: "compose", Version: "0.3.31",
		Artefacts: []Artefact{
			{Kind: "executor", Name: "argus-executor-i1", Version: "0.3.32"}, // moved
			{Kind: "kit", Name: "argus-kit", Version: "0.3.31"},              // did not
			{Kind: "skills", Name: "product folder", Version: "0.3.31"},      // did not
		},
	}
	got, err := CommitManifest(state, rehash, CommitOpts{Outcome: "rollback-failed", FailedStep: "A-3"})
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for _, a := range got.Artefacts {
		versions[a.Kind] = a.Version
	}
	if versions["executor"] != "0.3.32" || versions["kit"] != "0.3.31" {
		t.Fatalf("the mixed state was flattened: %v — an operator reading this cannot tell which half "+
			"moved, which is the only question `rollback-failed` leaves them with", versions)
	}
	if got.LastOutcome == nil || got.LastOutcome.FailedStep != "A-3" {
		t.Errorf("the failing step is not named: %+v", got.LastOutcome)
	}
}

func isOurs(kind string) bool {
	for _, k := range ourKinds {
		if k == kind {
			return true
		}
	}
	return false
}

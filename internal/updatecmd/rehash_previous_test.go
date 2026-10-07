package updatecmd

import "testing"

// V32 Release QA finding (d) — A FIRST UPDATE LEFT NOTHING TO ROLL BACK TO.
//
// Measured 2026-09-14 on orderservice-k3d, updated 0.3.30 → 0.3.32 through the new path: the manifest
// carries no `previous`, the control plane's rollback_block is "", and the page offers no way back.
// The cause is structural, not a slip: CommitManifest takes `previous` from the PRIOR manifest, and no
// instance onboarded before 0.3.32 has one — so the release that introduces the rollback block could
// never render it for any instance that exists today.
//
// The machine KNOWS where it was: discover.sh asks the running executor for its own `argus version`
// before anything moves, and observed.json already carries the image it ran. The re-hash records that
// reading as `previous` — and only when it is a real, orderable, OLDER version with a known image.

func observedBefore(version, image string) Observed {
	return Observed{Tier: "compose", ExecutorVersion: version,
		Compose: ComposeState{Project: "argus-inst-i1",
			Services: []ServiceState{{Name: "executor", Image: image, Digest: image}}}}
}

func TestRehash_AFirstUpdateRecordsTheExecutorItReplaced(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"})
	p.GeneratedAt = "2026-09-14T15:30:00Z"
	m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied"},
		observedBefore("0.3.30", "ghcr.io/x/exec@sha256:old"), "updated", "")

	if m.Previous == nil {
		t.Fatal("an update from 0.3.30 to 0.3.32 recorded NO previous version. On an instance with no prior " +
			"manifest — every instance onboarded before 0.3.32 — the page can then never offer the rollback block")
	}
	if m.Previous.Version != "0.3.30" || m.Previous.Image != "ghcr.io/x/exec@sha256:old" {
		t.Errorf("previous = %+v, want the version and image the executor REPORTED before the update (0.3.30, sha256:old)", *m.Previous)
	}
	if !RollbackOffered(m) {
		t.Error("previous 0.3.30 is below the installed 0.3.32, so the rollback block must be offered")
	}
}

// ⛔ NEVER INVENTED. A rollback block that points at a version nobody measured is a command that does
// something other than what it says. These are the cases where there genuinely is NOTHING to record —
// no reading at all, or a reading that cannot become a working rollback command (an unpinned image).
// Whether the reading can be ORDERED is a different question — see
// TestRehash_APreviousThatCannotBeOrderedIsStillRecorded below (AC-D49 / #296).
func TestRehash_NoPreviousIsInvented(t *testing.T) {
	for name, obs := range map[string]Observed{
		"the executor was already on the target": observedBefore("0.3.32", "ghcr.io/x/exec@sha256:same"),
		"the executor could not say its version": observedBefore("", "ghcr.io/x/exec@sha256:old"),
		"the image it ran is unknown":            observedBefore("0.3.30", ""),
		// k8s tiers read the Deployment's image string, a TAG when onboarded from one: the rollback block would
		// name it as --image-digest, which `update plan` refuses every time
		"the image it ran is a floating tag": observedBefore("0.3.30", "ghcr.io/x/exec:slim"),
	} {
		t.Run(name, func(t *testing.T) {
			p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"})
			m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied"}, obs, "updated", "")
			if m.Previous != nil {
				t.Errorf("a previous version was recorded from a reading that cannot support one: %+v", *m.Previous)
			}
		})
	}
}

// ⛔ AC-D49 (#296) — THE DEFECT ITSELF. Measured 2026-09-28 updating orderservice-k3d from
// 0.3.37-dev+9816842 to 0.3.40: observed.json HAD the previous version, the old image WAS
// digest-pinned, and the manifest still ended up with no `previous` at all — because recording used
// to ask "is before below the target" (SemverLess) as its OWN precondition, and a dev build suffix
// made that ordering test fail. Recording and ordering are separate questions: a reading that cannot
// be ordered is exactly the reading most worth keeping, because RollbackOffered is what decides
// whether to OFFER it, and it can only make that call from a recorded fact.
func TestRehash_APreviousThatCannotBeOrderedIsStillRecorded(t *testing.T) {
	for name, tc := range map[string]struct {
		obs         Observed
		wantOffered bool
	}{
		// the exact shape of the issue: a dev build suffix SemverLess cannot parse
		"the version carries a dev suffix": {
			obs: observedBefore("0.3.30-dev+9816842", "ghcr.io/x/exec@sha256:old"), wantOffered: true,
		},
		// a version with no numeric core at all — still recorded, but RollbackOffered must stay false:
		// a comparison we cannot make is not a comparison that passes.
		"the version cannot be ordered at all": {
			obs: observedBefore("dev", "ghcr.io/x/exec@sha256:old"), wantOffered: false,
		},
		// the executor reported a version NEWER than the target: still a real reading, worth keeping,
		// but RollbackOffered must not offer it forward.
		"the executor reported a NEWER version": {
			obs: observedBefore("0.3.40", "ghcr.io/x/exec@sha256:newer"), wantOffered: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"})
			m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied"}, tc.obs, "updated", "")
			if m.Previous == nil {
				t.Fatal("the reading was discarded instead of recorded — the previous version was in hand " +
					"and the ordering test (SemverLess) is not a reason to drop it")
			}
			if got := RollbackOffered(m); got != tc.wantOffered {
				t.Errorf("RollbackOffered = %v, want %v (previous=%+v, version=%s)", got, tc.wantOffered, *m.Previous, m.Version)
			}
			if !tc.wantOffered {
				reason := RollbackReason(m)
				if reason == "" {
					t.Error("rollback withheld and RollbackReason is empty — the page and the CLI would show nothing")
				}
			}
		})
	}
}

// A deliberate rollback is recorded by CommitManifest's own rule (previous is left alone, P-9); the
// re-hash must not hand it a fresh `previous` that would point forward.
func TestRehash_ARollbackRecordsNoPreviousOfItsOwn(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"})
	p.RollbackTo = "0.3.30"
	p.Version = "0.3.30"
	m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied"},
		observedBefore("0.3.32", "ghcr.io/x/exec@sha256:new"), "rolled-back-to 0.3.30", "")
	if m.Previous != nil {
		t.Errorf("a rollback's re-hash recorded previous %+v — that would offer a rollback FORWARD", *m.Previous)
	}
}

// ⛔ WHAT THE EXECUTOR REPORTS IS ITS OWN VERSION, NOT THE KIT'S (V32 adversary gate). update.sh moved the
// executor alone for releases, so a kit or skills folder that did not move this time may be OLDER than the
// executor — and recording it at the executor's version hides exactly the mixed state the page exists to
// show. Unread is "unknown".
func TestRehash_AnArtefactThatDidNotMoveIsNotGivenTheExecutorsVersion(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"}, Step{ID: "A-5", SkipReason: "held back"})
	m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied", "A-5": "skipped"},
		observedBefore("0.3.31", "ghcr.io/x/exec@sha256:old"), "updated", "")
	for _, a := range m.Artefacts {
		if a.Kind == "skills" && a.Version != "unknown" {
			t.Errorf("skills that did not move were recorded at %q — the executor's pre-update version, which nobody read for the skills", a.Version)
		}
	}
}

// An executor that came back (A-3 undone) did not move forward: no `previous`, and the header version
// is the one the executor itself reported — not "unknown", which it used to be on every pre-0.3.32
// instance because the fallback read an env key discovery never records.
func TestRehash_AnExecutorThatDidNotMoveKeepsTheVersionItReported(t *testing.T) {
	p := planWithSteps(Step{ID: "A-1"}, Step{ID: "A-3"})
	m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "undone"},
		observedBefore("0.3.30", "ghcr.io/x/exec@sha256:old"), "rolled-back", "A-3")
	if m.Version != "0.3.30" {
		t.Errorf("the executor came back to the version it reported (0.3.30); the manifest says %q", m.Version)
	}
	if m.Previous != nil {
		t.Errorf("nothing moved forward, yet previous = %+v", *m.Previous)
	}
}

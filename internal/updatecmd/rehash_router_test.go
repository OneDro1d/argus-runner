package updatecmd

import "testing"

// rehash_router_test.go — V32 Release QA defect Q1: AFTER A ROLLBACK THE PAGE SHOWED THE MACHINE ROUTER AT THE
// ROLLBACK TARGET'S IMAGE.
//
// Measured 2026-09-14 on orderservice-k3d, rolled back to 0.3.30: the manifest recorded the router artefact at
// image `…f976d139` (the rollback target) with version "unknown", while `docker inspect argus-router` read
// `…b3918321` — a rollback deliberately does not move the machine router (A-7 skips). plannedArtefacts gives the
// router the TARGET image, and Rehash corrected the image only for an EXECUTOR that did not move. The same held
// for a forward update whose A-7 kept a newer router.
//
// ⛔ A ROUTER THAT DID NOT MOVE IS RECORDED AS THE ROUTER THE MACHINE RUNS — the image and version discover.sh
// read before anything moved. Never the target: that is an image this machine's router has never run.

func routerPlan(steps ...Step) Plan {
	p := planWithSteps(steps...)
	p.Artefacts = append(p.Artefacts, Artefact{Kind: "router", Name: "argus-router", Version: p.Version, Image: p.ImageDigest})
	return p
}

func routerArtefact(t *testing.T, m Manifest) Artefact {
	t.Helper()
	for _, a := range m.Artefacts {
		if a.Kind == "router" {
			return a
		}
	}
	t.Fatal("the manifest records no router artefact")
	return Artefact{}
}

func TestRehash_ARouterThatDidNotMoveIsRecordedAsTheRouterTheMachineRuns(t *testing.T) {
	// ⚠ DISTINCT FROM THE EXECUTOR'S READING (image sha256:running, version 0.3.32) — a branch that copied the
	// executor's image or version into the router would otherwise pass (0.3.32 rebuild adversary gate)
	running := RouterState{Image: "ghcr.io/x/router@sha256:router-only", Version: "0.3.40"}
	cases := map[string]struct {
		rollbackTo string
		a7         string
		outcome    string
	}{
		"a rollback, which never moves the machine router": {rollbackTo: "0.3.30", a7: "skipped", outcome: "rolled-back-to 0.3.30"},
		"a forward update that kept a NEWER router":        {a7: "skipped", outcome: "updated"},
		// AC-D59 (#393): "a forward update whose router move was undone" used to be the third case here and
		// expected the pre-update reading. It is now `unknown` — A-7 was ATTEMPTED, so the router may have moved
		// half-way. It lives in rehash_router_shared_test.go, asserting the opposite on purpose.
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := routerPlan(Step{ID: "A-1"}, Step{ID: "A-3"}, Step{ID: "A-7"})
			if tc.rollbackTo != "" {
				p.RollbackTo, p.Version, p.ImageDigest = tc.rollbackTo, tc.rollbackTo, "ghcr.io/x/exec@sha256:rollback-target"
				p.Artefacts[len(p.Artefacts)-1].Image = p.ImageDigest
			}
			obs := observedBefore("0.3.32", "ghcr.io/x/exec@sha256:running")
			obs.Router = running
			m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied", "A-7": tc.a7}, obs, tc.outcome, "")
			r := routerArtefact(t, m)
			if r.Image != running.Image {
				t.Errorf("router image = %q, want %q — the router the machine RUNS; %q is an image this router never ran",
					r.Image, running.Image, p.ImageDigest)
			}
			if r.Version != running.Version {
				t.Errorf("router version = %q, want %q — the version the running router reported before anything moved",
					r.Version, running.Version)
			}
		})
	}
}

// No router on the machine: nothing to record but the fact. ⛔ Never the target image, never a guessed version.
func TestRehash_NoRouterOnTheMachineRecordsNoImageAndAnUnknownVersion(t *testing.T) {
	p := routerPlan(Step{ID: "A-1"}, Step{ID: "A-3"}, Step{ID: "A-7"})
	m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied", "A-7": "skipped"},
		observedBefore("0.3.31", "ghcr.io/x/exec@sha256:old"), "updated", "")
	r := routerArtefact(t, m)
	if r.Image != "" {
		t.Errorf("no router was running, yet the manifest records router image %q", r.Image)
	}
	if r.Version != "unknown" {
		t.Errorf("no router was running, yet the manifest records router version %q", r.Version)
	}
}

// The control: a router A-7 really moved is on the target image and version.
func TestRehash_ARouterThatMovedIsRecordedAtTheTarget(t *testing.T) {
	p := routerPlan(Step{ID: "A-1"}, Step{ID: "A-3"}, Step{ID: "A-7"})
	obs := observedBefore("0.3.31", "ghcr.io/x/exec@sha256:old")
	obs.Router = RouterState{Image: "ghcr.io/x/exec@sha256:old", Version: "0.3.31"}
	m := Rehash(p, map[string]string{"A-1": "applied", "A-3": "applied", "A-7": "applied"}, obs, "updated", "")
	r := routerArtefact(t, m)
	if r.Image != p.ImageDigest || r.Version != p.Version {
		t.Errorf("a router A-7 moved is recorded as %s / %s, want the target %s / %s", r.Image, r.Version, p.ImageDigest, p.Version)
	}
}

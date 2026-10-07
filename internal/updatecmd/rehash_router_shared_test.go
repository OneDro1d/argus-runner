package updatecmd

import (
	"encoding/json"
	"testing"
)

// rehash_router_shared_test.go — AC-D59 (#393): AN INSTANCE'S MANIFEST NEVER PRESENTS THE SHARED ROUTER'S
// VERSION AS ITS OWN CURRENT FACT.
//
// There is ONE argus-router per machine and every instance's manifest recorded its version as if it owned it.
// Two mechanisms made those records wrong:
//  1. INFERENCE — after a failed A-7 the router "did not move", so the re-hash recorded the pre-update reading.
//     That rule is right for the instance's own artefacts and wrong for the router: A-7 may have moved it
//     half-way before it failed, and another instance's update may have moved it too.
//  2. STALENESS — a record correct when written is wrong the moment a different instance moves the router.
//
// The decision: a router whose A-7 was ATTEMPTED and did not end `applied` is `unknown`;
// every router entry is marked SHARED with the time it was read. Wire names are checked through the JSON the
// manifest is written as — that is what the control plane and every older reader sees.

const sharedReadAt = "2026-09-30T10:15:00Z"

// routerWire is the router artefact as it is WRITTEN: the keys, not the Go fields.
func routerWire(t *testing.T, m Manifest) map[string]any {
	t.Helper()
	r := routerArtefact(t, m)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rehashRouter(t *testing.T, progress map[string]string, steps []Step, outcome, failedStep string) Manifest {
	t.Helper()
	p := routerPlan(steps...)
	p.GeneratedAt = sharedReadAt
	obs := observedBefore("0.3.46", "ghcr.io/x/exec@sha256:old")
	obs.Router = RouterState{Image: "ghcr.io/x/router@sha256:before", Version: "0.3.46"}
	return Rehash(p, progress, obs, outcome, failedStep)
}

var a7Steps = []Step{{ID: "A-1"}, {ID: "A-3"}, {ID: "A-7"}}

// MECHANISM 1, the failure direction. A-7 was attempted, it did not end applied: what the router is now is NOT
// what discover.sh read before, because it may have moved half-way. Never the pre-update reading.
func TestRehashRouter_AnAttemptedA7ThatDidNotEndAppliedIsUnknown(t *testing.T) {
	cases := map[string]struct {
		progress   map[string]string
		outcome    string
		failedStep string
	}{
		"A-7 failed and its undo completed (recorded undone)": {
			map[string]string{"A-1": "undone", "A-3": "undone", "A-7": "undone"}, "rolled-back", "A-7"},
		"A-7 recorded failed": {
			map[string]string{"A-1": "applied", "A-3": "applied", "A-7": "failed"}, "rolled-back", "A-7"},
		"A-7 failed and its undo failed too (NO record, the failing step names it)": {
			map[string]string{"A-1": "undone", "A-3": "undone"}, "rollback-failed", "A-7"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := rehashRouter(t, tc.progress, a7Steps, tc.outcome, tc.failedStep)
			r := routerArtefact(t, m)
			if r.Version != "unknown" {
				t.Errorf("router version = %q after an attempted A-7 that did not end applied, want \"unknown\" — "+
					"\"0.3.46\" is what the router ran BEFORE A-7 touched it", r.Version)
			}
			if r.Image != "" {
				t.Errorf("router image = %q, want none — the pre-update image is not a fact about the router now", r.Image)
			}
			if w := routerWire(t, m); w["shared"] != true {
				t.Errorf("router entry is not marked shared: %v", w)
			}
		})
	}
}

// The reading STANDS where A-7 never touched the router: not in the plan, skipped, or never reached.
func TestRehashRouter_ARouterA7NeverTouchedKeepsThePreUpdateReading(t *testing.T) {
	cases := map[string]struct {
		progress   map[string]string
		steps      []Step
		outcome    string
		failedStep string
	}{
		"A-7 is not in the plan at all": {
			map[string]string{"A-1": "applied", "A-3": "applied"}, []Step{{ID: "A-1"}, {ID: "A-3"}}, "updated", ""},
		"A-7 skipped (a newer router is kept, or a rollback)": {
			map[string]string{"A-1": "applied", "A-3": "applied", "A-7": "skipped"}, a7Steps, "updated", ""},
		"an EARLIER step failed, so A-7 was never reached": {
			map[string]string{"A-1": "undone", "A-3": "undone"}, a7Steps, "rolled-back", "A-3"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := rehashRouter(t, tc.progress, tc.steps, tc.outcome, tc.failedStep)
			r := routerArtefact(t, m)
			if r.Version != "0.3.46" || r.Image != "ghcr.io/x/router@sha256:before" {
				t.Errorf("router = %q @ %q, want the pre-update reading 0.3.46 — A-7 never touched it",
					r.Version, r.Image)
			}
			if w := routerWire(t, m); w["shared"] != true {
				t.Errorf("router entry is not marked shared: %v", w)
			}
		})
	}
}

// The success direction: a router A-7 really moved is the version that LANDED, still marked shared.
func TestRehashRouter_ASuccessfulA7RecordsTheVersionThatLandedMarkedShared(t *testing.T) {
	m := rehashRouter(t, map[string]string{"A-1": "applied", "A-3": "applied", "A-7": "applied"}, a7Steps, "updated", "")
	r := routerArtefact(t, m)
	if r.Version != "0.3.32" || r.Image != "ghcr.io/x/exec@sha256:new" {
		t.Errorf("router = %q @ %q, want the target that landed 0.3.32", r.Version, r.Image)
	}
	w := routerWire(t, m)
	if w["shared"] != true {
		t.Errorf("a router A-7 moved is still the machine's ONE router and must be marked shared: %v", w)
	}
	if w["read_at"] != sharedReadAt {
		t.Errorf("read_at = %v, want %q — the time of the reading, so the page can say \"as read at\"", w["read_at"], sharedReadAt)
	}
}

// Only the router is shared. A neighbour marked shared would put the caveat on artefacts this instance owns.
func TestRehashRouter_OnlyTheRouterIsMarkedShared(t *testing.T) {
	m := rehashRouter(t, map[string]string{"A-1": "applied", "A-3": "applied", "A-7": "applied"}, a7Steps, "updated", "")
	for _, a := range m.Artefacts {
		if a.Kind == "router" {
			continue
		}
		b, _ := json.Marshal(a)
		var w map[string]any
		_ = json.Unmarshal(b, &w)
		if _, ok := w["shared"]; ok {
			t.Errorf("artefact %q (%s) is marked shared: %v", a.Name, a.Kind, w)
		}
	}
}

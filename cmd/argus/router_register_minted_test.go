package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// VR9-I1 rule 3 — "Onboarding may then force-recreate when a register MINTED — and say so."
//
// ── THE GAP THIS CLOSES, WHICH IS THIS ROUND'S OWN THEME TURNED ON THE BUILD ──────────────────────
//
// Rule 2 added LoadOrCreateIdentityCreated so a caller could tell a fresh MINT from a LOAD. It then had
// zero production callers: the fact existed, was well-formed, and nothing asked for it — which is the
// exact shape of defect (a value present and empty of information) that round 9 exists to close.
//
// ⛔ WHY THE DISTINCTION IS LOAD-BEARING. `router register` mints only when identity.key is ABSENT. On a
// first onboard that is ordinary and uninteresting: the register runs BEFORE the router container
// starts, so the container simply loads what was just written. The dangerous case is a mint while a
// router is ALREADY RUNNING — a wiped or replaced state dir — because the running process is then
// signing with a key the control plane no longer has. Rule 1 makes a current router notice that within
// a poll interval; a router on an OLDER image cannot, and 401s silently forever.
//
// Onboarding cannot act on any of that unless the register SAYS which it did. So it says.
func TestRouterRegister_ReportsWhetherItMintedTheIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"registered":true}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	run := func(t *testing.T) map[string]any {
		t.Helper()
		return captureEmit(t, func() {
			if rc := cmdRouterRegister([]string{"--state", dir, "--control-plane", srv.URL, "--token", "sess_x"}); rc != 0 {
				t.Fatalf("router register exited %d", rc)
			}
		})
	}

	t.Run("a first register reports that it minted", func(t *testing.T) {
		out := run(t)
		created, ok := out["identity_created"]
		if !ok {
			t.Fatalf("router register printed no identity_created: %v — onboarding cannot tell a fresh "+
				"mint from a load, so it cannot warn that a RUNNING router is now signing with a key "+
				"the control plane does not have", out)
		}
		if created != true {
			t.Fatalf("identity_created = %v on a fresh state dir, want true", created)
		}
	})

	// ⛔ AND THE SECOND CALL MUST SAY false. A register that always claimed a mint would make the
	// warning fire on every re-onboard, which is the alarm-fatigue failure in a different costume.
	t.Run("a second register reports that it loaded", func(t *testing.T) {
		out := run(t)
		if out["identity_created"] != false {
			t.Fatalf("identity_created = %v on an existing identity, want false — a warning that fires "+
				"on every onboard is one nobody reads", out["identity_created"])
		}
	})

	// The identity must be the SAME one across both calls: reporting "loaded" while quietly minting a
	// second key would be worse than not reporting at all.
	t.Run("the loaded identity is the one that was minted", func(t *testing.T) {
		first := captureEmit(t, func() {
			_ = cmdRouterRegister([]string{"--state", dir, "--control-plane", srv.URL, "--token", "sess_x"})
		})
		second := captureEmit(t, func() {
			_ = cmdRouterRegister([]string{"--state", dir, "--control-plane", srv.URL, "--token", "sess_x"})
		})
		if first["router_id"] != second["router_id"] || first["router_id"] == nil {
			t.Fatalf("router_id changed between calls: %v then %v", first["router_id"], second["router_id"])
		}
	})
}

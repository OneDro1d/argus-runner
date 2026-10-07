package runner

// deregister_kitdir_test.go — what the de-register response still carries, and what it no longer does.
//
// ── THIS FILE REPLACES deregister_tristate_test.go, AND THE REASON IS THE POINT ───────────────────
//
// That file pinned hop 2 of a four-hop carrier for the fact "what became of the ACCOUNT's onboarding
// token": control-plane response → this decode → cloud-deregister's emit → teardown.sh's parse. Its
// hardest case was the NIL one — an absent `auto_token_revoked` had to stay absent, never flatten to
// `false`, because a missing signal rendered as the reassuring one is the defect this project keeps
// re-learning.
//
// The carrier is gone because the FACT is gone (VR10-T4-7). 0.3.29 gives every machine its own author
// token (minted during onboarding), and an instance delete cannot name a machine — so `deleteInstance`
// revokes nothing, the control plane sends no such field, and a reader for it would be nil forever.
// Teardown would then hedge on every single run about a question nobody asks. Deleted with the rule
// it served, not weakened to pass.
//
// ⚠ WHAT SURVIVES IS THE OTHER PASSENGER ON THE SAME RESPONSE: VR5-T1's `kit_dir`, which teardown
// needs in order to scrub the credentials written where this instance was onboarded from. It rode
// beside the token verdict and must not be lost while the verdict is removed — which is exactly the
// "a correct producer and a correct consumer with nothing watching the join" shape the deleted file's
// header warned about. So the decode keeps a test.

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func deregTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	return NewClient(srvURL, "inst-dereg", priv)
}

func TestDeregisterWithKitDir_TheKitPathSurvivesTheDecode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"a kit dir", `{"deregistered":true,"kit_dir":"K"}`, "K"},
		// A control plane onboarded before migration 027 records no kit dir. Empty must reach teardown
		// as empty, so it can say it could not find out rather than scrub nothing and report success.
		{"no kit dir", `{"deregistered":true}`, ""},
		// ⛔ FORWARD COMPATIBILITY, and the reason this case exists at all: a control plane that still
		// sends `auto_token_revoked` (one deployed before this build) must not break the decode. The
		// field is IGNORED — not read, not reported, not turned back into a sentence.
		{"a control plane still sending the retired field",
			`{"deregistered":true,"kit_dir":"K","auto_token_revoked":true}`, "K"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			out, err := deregTestClient(t, srv.URL).DeregisterWithKitDir(context.Background())
			if err != nil {
				t.Fatalf("DeregisterWithKitDir: %v", err)
			}
			if out.KitDir != c.want {
				t.Errorf("KitDir = %q, want %q — the VR5-T1 value must survive the removal of the "+
					"token verdict it used to travel beside", out.KitDir, c.want)
			}
			if out.NotPresent {
				t.Error("NotPresent = true for a 200 — the instance was there and was removed")
			}
		})
	}
}

// The idempotent re-run: the instance is already gone, so this call deleted nothing. VR8-V1 — that is
// a different OUTCOME from a real de-registration, and teardown must be able to tell them apart
// rather than print "the name is free, the data is purged" about an instance nobody had heard of.
func TestDeregisterWithKitDir_AnAlreadyGoneInstanceSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"reason":"` + federation.ReasonUnknownInstance + `"}`))
	}))
	defer srv.Close()
	out, err := deregTestClient(t, srv.URL).DeregisterWithKitDir(context.Background())
	if err != nil {
		t.Fatalf("an already-gone instance must be a success (idempotent teardown): %v", err)
	}
	if !out.NotPresent {
		t.Error("NotPresent = false for an unknown instance — this call removed nothing and must say so")
	}
	if out.KitDir != "" {
		t.Errorf("KitDir = %q; the row that held it is gone", out.KitDir)
	}
}

package onboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// THE TEST THAT WAS MISSING.
//
// V15-015, found live 2026-08-12 on a seven-instance estate. Onboard #1 succeeded; #2 and #3 died at
// `cloud-mint-token` with "this account already has an author token (minted during onboarding), but cp-author.token does not
// hold it" — a message that was FALSE, because the file held exactly that token and it answered 200
// on the control plane's /mcp.
//
// The cause was a probe pointed at the wrong tenancy. TokenWorks() decided reuse by calling
// GET /api/workspaces, which the B-block (0.3.18) had deliberately closed to author PATs: those are
// USER-GLOBAL, carry no workspace, and every workspace-scoped web endpoint answers 403. So a live
// token was judged dead, reuse was skipped, a mint followed, and the store's one-auto-token-per-owner
// rule (VR-B3) rejected it.
//
// Every existing unit test passed throughout, because the FIXTURE answered 200 where the server
// answers 403 — and because each test performed exactly ONE onboard. The first onboard cannot see
// this bug: `existing` is empty, so the probe never runs. It takes TWO.
//
// So this file onboards twice against a control plane that behaves like the real one, and asserts
// the second is a no-write reuse.
type twiceCP struct {
	srv   *httptest.Server
	mints int
	live  string // the ONE live auto token this account holds ("" = none yet)
}

func newTwiceCP(t *testing.T) *twiceCP {
	t.Helper()
	f := &twiceCP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/api/workspaces":
			// web.go:210 — workspace-scoped, and a user-global author token has no workspace.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"a workspace-bound token is required for the web"}`))
		case r.URL.Path == "/api/routers":
			// web.go:111 — authedOwner, so a user-global token is exactly the right credential.
			if f.live != "" && auth == "Bearer "+f.live {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"routers":[]}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/meta":
			// Unauthenticated on the real control plane. Present so that a probe "fixed" by pointing
			// at /api/meta still fails the discrimination test below.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mode":"control"}`))
		case r.URL.Path == "/api/tokens" && r.Method == http.MethodPost:
			if f.live != "" { // VR-B3: one auto token per owner
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"auto token exists","reason":"auto_token_exists"}`))
				return
			}
			f.mints++
			f.live = "odts_minted_by_onboard_one"
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"tok_1","token":"` + f.live + `","endpoint":"x/mcp","sse_endpoint":"x/sse"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *twiceCP) client() *CloudClient { return NewCloudClient(f.srv.URL) }

// Two onboards on one machine — the estate's actual shape, and the shape no test covered.
func TestOnboardTwice_TheSecondReusesInsteadOfFailing(t *testing.T) {
	f := newTwiceCP(t)
	c := f.client()

	// --- onboard #1: nothing held locally, so a token is minted.
	first, reused, err := c.MintOrReuseAuthorToken(context.Background(), "session", "orderservice-compose-onboarding", "", "rtr_t")
	if err != nil {
		t.Fatalf("first onboard failed: %v", err)
	}
	if reused {
		t.Fatal("first onboard reported REUSE with nothing held locally")
	}
	if f.mints != 1 {
		t.Fatalf("first onboard minted %d times, want 1", f.mints)
	}

	// --- onboard #2: the same machine, holding the token onboard #1 wrote to disk.
	second, reused, err := c.MintOrReuseAuthorToken(context.Background(), "session", "orderservice-k3d-onboarding", first.Token, "rtr_t")
	if err != nil {
		t.Fatalf("SECOND onboard failed with a live token on disk: %v\n"+
			"this is V15-015: the reuse probe judged a working token dead, so it tried to mint and "+
			"hit the one-auto-token-per-owner rule", err)
	}
	if !reused {
		t.Fatal("second onboard did not REUSE the token this machine already holds")
	}
	if second.Token != first.Token {
		t.Fatalf("second onboard returned a different token: %q vs %q", second.Token, first.Token)
	}
	if f.mints != 1 {
		t.Fatalf("the control plane was asked to mint %d times across two onboards, want exactly 1", f.mints)
	}
}

// The probe must accept a USER-GLOBAL author token. Stated on its own so a failure names the cause
// rather than surfacing three layers up as a confusing 409.
func TestTokenWorks_AcceptsAUserGlobalAuthorToken(t *testing.T) {
	f := newTwiceCP(t)
	c := f.client()
	if _, _, err := c.MintOrReuseAuthorToken(context.Background(), "session", "kit-onboarding", "", "rtr_t"); err != nil {
		t.Fatalf("setup mint: %v", err)
	}
	if !c.TokenWorks(context.Background(), f.live) {
		t.Fatal("TokenWorks said NO to the account's live user-global author token — it is probing a " +
			"workspace-scoped endpoint, which such a token can never satisfy (web.go:210)")
	}
}

// ...and it must still say NO to a token the control plane does not know. Without this, "fixing" the
// probe by pointing it at an unauthenticated endpoint (/api/meta) would pass the test above while
// declaring every string a valid credential.
func TestTokenWorks_RejectsAnUnknownToken(t *testing.T) {
	f := newTwiceCP(t)
	c := f.client()
	if _, _, err := c.MintOrReuseAuthorToken(context.Background(), "session", "kit-onboarding", "", "rtr_t"); err != nil {
		t.Fatalf("setup mint: %v", err)
	}
	if c.TokenWorks(context.Background(), "odts_never_issued") {
		t.Fatal("TokenWorks accepted a token the control plane never issued — the probe is not " +
			"authenticated and proves nothing")
	}
}

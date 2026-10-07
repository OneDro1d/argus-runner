package onboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// VR-B3 as onboarding has to implement it: at most ONE auto token per user, REUSED across onboards,
// minted only when none exists.
//
// The shape is forced by VR-B7 — a token is shown once and only its hash is stored, so the control
// plane genuinely cannot return an existing token's plaintext. "Reuse it" can therefore only mean
// the credential this machine already holds, if it still authenticates.
//
// This also has to work, not just be nice: onboard.sh minted UNCONDITIONALLY on every run, which is
// how 195 tokens accumulated, and once VR-B8's label opts them into the one-per-owner index a second
// onboard would fail outright without this path.

type fakeCP struct {
	srv        *httptest.Server
	mints      int    // how many times a NEW token was minted
	validFor   string // the one token the OWNER-SCOPED probe accepts
	mintStatus int    // what POST /api/tokens answers
}

// newFakeCP models the control plane AS IT ACTUALLY BEHAVES since 0.3.18 (the B-block), which is the
// whole point of this fixture and the reason the bug below shipped green.
//
// The previous version answered 200 on /api/workspaces for an author PAT. The real control plane has
// answered 403 "a workspace-bound token is required for the web" since web.go:210 — author tokens are
// USER-GLOBAL and carry no workspace, and every workspace-scoped web endpoint refuses them by design.
// So the fixture asserted a contract the server does not offer, TokenWorks() kept probing
// /api/workspaces, and reuse could never succeed in production while all four tests stayed green.
//
// /api/workspaces is now hard-wired to 403 here. That is a TRAP ON PURPOSE: point the reuse probe
// back at any workspace-scoped endpoint and every reuse test fails immediately instead of in a
// customer's second onboard.
func newFakeCP(t *testing.T, validFor string, mintStatus int) *fakeCP {
	t.Helper()
	f := &fakeCP{validFor: validFor, mintStatus: mintStatus}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/workspaces":
			// NEVER 200 for a bearer token here — see the doc comment above.
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"a workspace-bound token is required for the web"}`))
		case "/api/routers":
			// OWNER-scoped (web.go:111 authedOwner): the correct tenancy for a user-global token.
			// Authenticated for real — an unknown token is 401 — so it discriminates, unlike
			// /api/meta, which answers 200 unauthenticated and would call any string a live token.
			if f.validFor != "" && auth == "Bearer "+f.validFor {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"routers":[]}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/api/tokens":
			if f.mintStatus != 0 && f.mintStatus != http.StatusOK {
				w.WriteHeader(f.mintStatus)
				_, _ = w.Write([]byte(`{"error":"auto token exists","reason":"auto_token_exists"}`))
				return
			}
			f.mints++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"tok_new","token":"odts_freshly_minted","endpoint":"x/mcp","sse_endpoint":"x/sse"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCP) client() *CloudClient { return NewCloudClient(f.srv.URL) }

// THE HEADLINE: a re-onboard on a machine that already holds a working token performs NO write.
func TestMintOrReuse_ReusesAWorkingLocalToken(t *testing.T) {
	f := newFakeCP(t, "odts_already_here", http.StatusOK)
	mt, reused, err := f.client().MintOrReuseAuthorToken(context.Background(), "session", "kit-onboarding", "odts_already_here", "rtr_t")
	if err != nil {
		t.Fatalf("MintOrReuseAuthorToken: %v", err)
	}
	if !reused {
		t.Fatal("a working local token was not reused — every re-onboard would mint another")
	}
	if mt.Token != "odts_already_here" {
		t.Fatalf("reuse returned a different token: %q", mt.Token)
	}
	if f.mints != 0 {
		t.Fatalf("the control plane was asked to mint %d time(s) despite a working local token", f.mints)
	}
}

// A local token that no longer authenticates is NOT reused — that is the revoked case, and VR-B3
// says a fresh one is minted silently.
func TestMintOrReuse_MintsWhenTheLocalTokenIsDead(t *testing.T) {
	f := newFakeCP(t, "odts_the_live_one", http.StatusOK)
	mt, reused, err := f.client().MintOrReuseAuthorToken(context.Background(), "session", "kit-onboarding", "odts_revoked_yesterday", "rtr_t")
	if err != nil {
		t.Fatalf("MintOrReuseAuthorToken: %v", err)
	}
	if reused || mt.Token != "odts_freshly_minted" {
		t.Fatalf("a dead local token was reused: reused=%v token=%q", reused, mt.Token)
	}
	if f.mints != 1 {
		t.Fatalf("mints = %d, want exactly 1", f.mints)
	}
}

func TestMintOrReuse_MintsWhenThereIsNoLocalToken(t *testing.T) {
	f := newFakeCP(t, "", http.StatusOK)
	_, reused, err := f.client().MintOrReuseAuthorToken(context.Background(), "session", "kit-onboarding", "", "rtr_t")
	if err != nil || reused || f.mints != 1 {
		t.Fatalf("a fresh machine must mint exactly once: reused=%v mints=%d err=%v", reused, f.mints, err)
	}
}

// The 409: the ACCOUNT holds a live auto token but THIS machine does not have it. Surfaced as a
// typed error so the CLI can say something actionable instead of "mint failed: status 409".
// Deliberately not auto-recovered: silently revoking the live one would break whichever machine is
// still using it.
func TestMintOrReuse_SurfacesTheOneAutoPerOwnerConflict(t *testing.T) {
	f := newFakeCP(t, "", http.StatusConflict)
	_, _, err := f.client().MintOrReuseAuthorToken(context.Background(), "session", "kit-onboarding", "", "rtr_t")
	if !errors.Is(err, ErrAutoTokenExists) {
		t.Fatalf("got %v, want ErrAutoTokenExists", err)
	}
}

// "I could not check" must answer NO. If the reachability probe failed and we reused anyway,
// onboarding would install a credential it never verified and the next step would take the blame.
func TestTokenWorks_AnUnreachableControlPlaneIsNotAPass(t *testing.T) {
	c := NewCloudClient("http://127.0.0.1:1")
	if c.TokenWorks(context.Background(), "odts_anything") {
		t.Fatal("an unreachable control plane reported the token as working")
	}
}

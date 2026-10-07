package runner

// register_credential_test.go — which credential does Register present?  (VR-L2 / V17-020)
//
// THE DEFECT, observed 2026-08-13 on the owner's re-onboard of a LIVE instance:
//
//   onboarding printed two contradictory lines, back to back:
//       "'memstore-compose' is YOUR live instance and this kit holds its machine identity —
//        re-onboarding REFRESHES it in place: same identity, same registration, same history."
//       "S1: minted a single-use enrollment credential for the first register"
//   …and the executor then reported:
//       register-on-start failed: register: 403
//       {"error":"re-register requires the machine JWT of the registered identity"}
//
//   The control plane is RIGHT to refuse. fed.go: an instance that already exists may only be
//   re-registered by proving possession of the registered key; an enr_ string is not a JWT.
//
//   The client's own comment states the correct rule — "the FIRST register presents the
//   onboarding-minted enrollment bearer; every RE-register (restart) presents the machine JWT" —
//   but the code implemented a different one: `if c.enrollment != ""` sends the enrollment WHENEVER
//   IT HOLDS ONE, with no knowledge of which case it is in. Onboarding mints one on every run,
//   refresh included, so a re-onboard always sent the wrong credential.
//
//   Cost: the onboard aborts on a 60s timeout that blames network reachability, AFTER registration —
//   so the author-token mint, the cloud wiring, the scenario import and the banner never run. That
//   is what makes a half-finished onboard unrecoverable without a teardown.
//
// THE RULE (VR-L2a): the choice keys off whether a REGISTERED IDENTITY can be proven, not off
// whether an enrollment credential happens to be in hand. The client cannot know locally whether the
// control plane has seen it before — so it PROVES ITS IDENTITY FIRST and falls back to the
// enrollment only when the control plane says a first-register credential is what it wants.
//
// This deliberately fixes BOTH tiers. The SA's E2 warned that a compose-only fix could leave the
// same defect dormant on k8s (where the same two lines printed and the register somehow succeeded);
// keying on the control plane's own answer rather than on any local environment difference means
// there is no path left where the wrong credential is sent.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// cpStub models the control plane's two register branches (fed.go): an instance it has never seen
// demands an enr_ credential; one it knows demands a machine JWT it can verify.
type cpStub struct {
	known    bool              // does this instance already exist?
	pub      ed25519.PublicKey // the registered key, when known
	seen     []string          // every Authorization value presented, in order
	failFast bool              // 500 on the first call, to prove a real error is not retried blindly
}

func (s *cpStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.seen = append(s.seen, cred)
		if s.failFast && len(s.seen) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if s.known {
			if _, err := federation.VerifyJWT(cred, s.pub, time.Now()); err != nil {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "re-register requires the machine JWT of the registered identity"})
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if !strings.HasPrefix(cred, "enr_") {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "register requires an enrollment credential"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

func newTestClient(t *testing.T, url string) (*Client, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return NewClient(url, "vrl2-inst", priv), pub
}

// A RE-register must succeed. This is the case the owner hit, and today it 403s.
func TestRegister_ReRegisterPresentsTheMachineJWT(t *testing.T) {
	cp := &cpStub{known: true}
	c, pub := newTestClient(t, "")
	cp.pub = pub
	srv := cp.server(t)
	defer srv.Close()
	c.baseURL = srv.URL

	// Onboarding mints one on EVERY run, refresh included — that is the trigger.
	c.SetEnrollment("enr_freshly_minted_even_though_this_is_a_refresh")

	if err := c.Register(context.Background(), federation.RegisterRequest{InstanceID: "vrl2-inst"}); err != nil {
		t.Fatalf("re-register failed: %v\npresented: %v", err, redact(cp.seen))
	}
	if len(cp.seen) == 0 {
		t.Fatal("no credential presented at all")
	}
	// The LAST thing presented must be the proof of identity, not the enrollment.
	last := cp.seen[len(cp.seen)-1]
	if strings.HasPrefix(last, "enr_") {
		t.Errorf("re-register presented the ENROLLMENT credential (%q) where the machine JWT is required — "+
			"the client chose on possession, not on identity", redactOne(last))
	}
}

// A genuine FIRST register must still work: the key is not registered yet, so only the enrollment
// can authorise it. The fix must not trade one broken case for the other.
func TestRegister_FirstRegisterStillUsesTheEnrollment(t *testing.T) {
	cp := &cpStub{known: false}
	c, _ := newTestClient(t, "")
	srv := cp.server(t)
	defer srv.Close()
	c.baseURL = srv.URL
	c.SetEnrollment("enr_first_time")

	if err := c.Register(context.Background(), federation.RegisterRequest{InstanceID: "vrl2-inst"}); err != nil {
		t.Fatalf("first register failed: %v\npresented: %v", err, redact(cp.seen))
	}
	if !strings.HasPrefix(cp.seen[len(cp.seen)-1], "enr_") {
		t.Errorf("first register did not end on the enrollment credential; presented %v", redact(cp.seen))
	}
}

// With NO enrollment in hand (an executor restart), the machine JWT is the only option and must be
// used directly — the pre-existing behaviour, asserted so the fix does not disturb it.
func TestRegister_NoEnrollmentUsesTheMachineJWT(t *testing.T) {
	cp := &cpStub{known: true}
	c, pub := newTestClient(t, "")
	cp.pub = pub
	srv := cp.server(t)
	defer srv.Close()
	c.baseURL = srv.URL

	if err := c.Register(context.Background(), federation.RegisterRequest{InstanceID: "vrl2-inst"}); err != nil {
		t.Fatalf("restart re-register failed: %v", err)
	}
	if len(cp.seen) != 1 {
		t.Errorf("presented %d credentials, want exactly 1 — there is nothing to fall back to", len(cp.seen))
	}
}

// A real server error must NOT be papered over by trying the other credential: that would turn a
// 500 into a confusing auth story, which is the shape of defect this whole round is about.
func TestRegister_ServerErrorIsNotRetriedAsAuth(t *testing.T) {
	cp := &cpStub{known: true, failFast: true}
	c, pub := newTestClient(t, "")
	cp.pub = pub
	srv := cp.server(t)
	defer srv.Close()
	c.baseURL = srv.URL
	c.SetEnrollment("enr_should_not_be_tried_on_a_500")

	err := c.Register(context.Background(), federation.RegisterRequest{InstanceID: "vrl2-inst"})
	if err == nil {
		t.Fatal("a 500 must surface as an error, not be retried into success")
	}
	for _, cred := range cp.seen {
		if strings.HasPrefix(cred, "enr_") {
			t.Errorf("the enrollment was presented after a 500 — the fallback must be scoped to the "+
				"control plane ASKING for an enrollment, not to any failure; presented %v", redact(cp.seen))
		}
	}
}

func redact(all []string) []string {
	out := make([]string, len(all))
	for i, s := range all {
		out[i] = redactOne(s)
	}
	return out
}
func redactOne(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}

package chain

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// — the `unreachable` claim of a chain http step, at RUN time.

func unreachableStep(url string) Step {
	return UnreachableHTTPStep("blocked", "GET", url, nil, "", nil, &scenario.MoneySpendLedger{})
}

// closedURL is a loopback URL nothing listens on: a refused connection.
func closedURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

func withUnreachableKnobs(t *testing.T, timeout time.Duration, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	t.Helper()
	oldT, oldD := unreachableTimeout, unreachableDial
	unreachableTimeout, unreachableDial = timeout, dial
	t.Cleanup(func() { unreachableTimeout, unreachableDial = oldT, oldD })
}

func TestUnreachable_RefusedConnectionPasses(t *testing.T) {
	st := unreachableStep(closedURL(t)).Run("cid", map[string]string{})
	if st.Status != "passed" {
		t.Fatalf("a refused connection is the expected answer; got %+v", st)
	}
	if st.AssertionsEnforcedCount != 1 || st.AssertionsEnforced[0] != "unreachable" {
		t.Errorf("the claim must be listed as enforced: %+v", st)
	}
}

func TestUnreachable_ConnectTimeoutPasses(t *testing.T) {
	// the network drops the SYN: a dial that never completes until the step's own timeout.
	withUnreachableKnobs(t, 150*time.Millisecond, func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	start := time.Now()
	st := unreachableStep("http://10.255.255.1:8080/x").Run("cid", map[string]string{})
	if st.Status != "passed" {
		t.Fatalf("a connect timeout is the expected answer; got %+v", st)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the step must use its own short timeout, took %s", time.Since(start))
	}
}

// A reset AFTER the server accepted is a connection the network let through: it FAILS the claim
// (TestUnreachable_AcceptThenResetFails). A reset AT the dial passes (TestUnreachable_ResetAtTheDialPasses).

func TestUnreachable_AnyHTTPStatusFails(t *testing.T) {
	for _, code := range []int{200, 403, 404, 503} {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		st := unreachableStep(srv.URL).Run("cid", map[string]string{})
		srv.Close()
		if st.Status != "failed" {
			t.Errorf("status %d: the network let the request through, so the claim must FAIL; got %+v", code, st)
		}
		if len(st.FailedClaims) != 1 || st.FailedClaims[0].Claim != "unreachable" {
			t.Errorf("status %d: failed_claims must name the claim: %+v", code, st.FailedClaims)
		}
	}
}

func TestUnreachable_DNSFailureFails(t *testing.T) {
	withUnreachableKnobs(t, 2*time.Second, func(_ context.Context, _, addr string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "typo.invalid", IsNotFound: true}
	})
	st := unreachableStep("http://typo.invalid:8080/x").Run("cid", map[string]string{})
	if st.Status != "failed" || !strings.Contains(st.Observed, "DNS") {
		t.Fatalf("a name that does not resolve is a typo, not a blocked path; got %+v", st)
	}
}

func TestUnreachable_RealDNSMissFails(t *testing.T) {
	st := unreachableStep("http://argus-no-such-host.invalid:8080/x").Run("cid", map[string]string{})
	if st.Status == "passed" {
		t.Fatalf("an unresolvable name must never pass; got %+v", st)
	}
}

// Connected but the server never answers: the network LET IT THROUGH, so this is not unreachability.
func TestUnreachable_ConnectedButSilentFails(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	withUnreachableKnobs(t, 200*time.Millisecond, nil)
	st := unreachableStep(srv.URL).Run("cid", map[string]string{})
	if st.Status != "failed" {
		t.Fatalf("a connection that was established must fail the claim even with no response; got %+v", st)
	}
}

// ── the gate: judged only when an EARLIER step of the same chain passed ───────────────────────────

func okStep(name string) Step {
	return Step{Name: name, Run: func(string, map[string]string) report.StepResult {
		return report.StepResult{Status: "passed", Observed: "ok"}
	}}
}

func failStep(name string) Step {
	return Step{Name: name, Run: func(string, map[string]string) report.StepResult {
		return report.StepResult{Status: "failed", Observed: "no"}
	}}
}

func statusOf(res report.ScenarioResult, name string) string {
	for _, s := range res.Steps {
		if s.Name == name {
			return s.Status
		}
	}
	return "<absent>"
}

func TestUnreachable_PassesAfterAnEarlierPassAndDoesNotStopTheChain(t *testing.T) {
	res := Run("cid", []Step{okStep("alive"), unreachableStep(closedURL(t)), okStep("after")})
	if res.Status != "passed" || statusOf(res, "blocked") != "passed" || statusOf(res, "after") != "passed" {
		t.Fatalf("expected unreachability must not stop or fail the chain: %+v", res)
	}
}

func TestUnreachable_NotMeasuredWhenNoEarlierStepPassed(t *testing.T) {
	// first step failed
	res := Run("cid", []Step{failStep("alive"), unreachableStep(closedURL(t))})
	if got := statusOf(res, "blocked"); got != report.StepNotMeasured {
		t.Fatalf("no earlier passing step: want not-measured, got %q (%+v)", got, res.Steps)
	}
	if res.Status == "passed" {
		t.Errorf("the scenario must not pass: %+v", res)
	}
	// the claim is the chain's first step
	res = Run("cid", []Step{unreachableStep(closedURL(t))})
	if got := statusOf(res, "blocked"); got != report.StepNotMeasured || res.Status == "passed" {
		t.Fatalf("an unreachable claim with nothing before it must be not-measured and never green; got %q %+v", got, res)
	}
}

// A dead SUT stops the chain at the positive step: the negative step is never judged.
func TestUnreachable_DeadClusterDoesNotPassTheNegativeCheck(t *testing.T) {
	pos := HTTPStep("alive", "GET", closedURL(t), nil, "", 200, nil, nil, nil, false, nil, &scenario.MoneySpendLedger{})
	res := Run("cid", []Step{pos, unreachableStep(closedURL(t))})
	if got := statusOf(res, "blocked"); got != report.StepNotMeasured {
		t.Fatalf("a dead positive step must leave the negative one not-measured; got %q", got)
	}
	if res.Status == "passed" {
		t.Fatalf("the scenario must not pass: %+v", res)
	}
}

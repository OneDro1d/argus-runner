package chain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// — THE `unreachable` CLAIM OF AN http STEP.
//
// `- step <name>: unreachable` is the one place a transport failure is the EXPECTED answer. It passes
// ONLY when no HTTP response arrived because of a transport failure: a connect timeout, a refused
// connection, a reset, no route. ANY HTTP status fails it (the network let the request through), and so
// does a DNS "no such host" (a typo must not read as a blocked path). A connection that WAS established
// and then simply got no answer fails it too: the network let it through, the server was slow.
//
// ⛔ THE FALSE-GREEN GUARD is in Run (chain.go, Step.RequiresEarlierPass): the step is judged only when an
// EARLIER step of the same chain passed. Otherwise a dead cluster would pass every negative check.

// unreachableTimeout bounds the one request of an unreachable step. A var only so a test can shorten
// it (and so tests need no 5 s wait).
var unreachableTimeout = 5 * time.Second

// unreachableDial replaces the dialer in tests (a SYN the network drops, a DNS failure). nil = the real
// dialer with unreachableTimeout.
var unreachableDial func(ctx context.Context, network, addr string) (net.Conn, error)

// classifyTransportError judges ONE transport error against the claim. connected says a TCP
// connection to the target was established before the error.
func classifyTransportError(err error, connected bool) (pass bool, why string) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false, "DNS resolution failed — a name that does not resolve is not a blocked path"
	}
	if connected {
		// a TCP connection was established: whatever happened next (EOF, reset, timeout), the network
		// let the request through. Only a failure BEFORE a connection can prove unreachability.
		return false, "the connection was established — the network let the request through (" + err.Error() + ")"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return true, "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return true, "connection reset"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return true, "no route to host"
	case errors.Is(err, syscall.ETIMEDOUT):
		return true, "connect timed out"
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		if connected {
			return false, "the connection was established and then no response arrived — the network let the request through"
		}
		return true, "connect timed out"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true, "connection closed before any response"
	}
	return false, "a transport error that is not a refusal, reset, timeout or missing route"
}

// UnreachableHTTPStep builds the step for `- step <name>: unreachable`. It sends ONE request and never
// polls. The request is built, and the money_writes spend check applied, exactly as HTTPStep does.
func UnreachableHTTPStep(name, method, urlTmpl string, headers map[string]string, bodyTmpl string,
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger) Step {
	return Step{
		Name:                name,
		RequiresEarlierPass: true,
		Needs: func(vars map[string]string) error {
			_, _, err := buildHTTPRequest(method, urlTmpl, headers, bodyTmpl, vars)
			return err
		},
		Run: func(cid string, vars map[string]string) report.StepResult {
			req, resolvedBody, berr := buildHTTPRequest(method, urlTmpl, headers, bodyTmpl, vars)
			if berr != nil {
				return report.StepResult{Status: "failed", Observed: berr.Error()}
			}
			if entry := moneyAllow.Match(req.Method, req.URL.Path); entry != nil && entry.Spends {
				if msg := scenario.EvaluateSpendAmount(entry, resolvedBody); msg != "" {
					return report.StepResult{Status: report.StatusError, Observed: MoneyWriteRefusal + msg}
				}
				if ok, _ := moneyLedger.Reserve(entry); !ok {
					return report.StepResult{Status: report.StatusError, Observed: fmt.Sprintf(
						"%s%s %s already reached its max_per_run (%d) for this run",
						MoneyWriteRefusal, entry.Method, entry.Path, entry.MaxPerRun)}
				}
			}

			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.DisableKeepAlives = true
			dial := unreachableDial
			if dial == nil {
				dial = (&net.Dialer{Timeout: unreachableTimeout}).DialContext
			}
			// connected is marked AT THE DIAL, the moment a TCP connection exists: httptrace's GotConn fires only
			// after the TLS handshake, so a reset inside the handshake of an https target read as "never
			// connected" and passed. The transport dials on its own goroutine, hence the atomic.
			var connected atomic.Bool
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := dial(ctx, network, addr)
				if err == nil {
					connected.Store(true)
				}
				return c, err
			}
			client := &http.Client{Transport: tr, Timeout: unreachableTimeout,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			defer tr.CloseIdleConnections()

			enforced := []string{scenario.UnreachableClaim}
			resp, derr := client.Do(req)
			if derr != nil {
				pass, why := classifyTransportError(derr, connected.Load())
				if pass {
					return report.StepResult{Status: "passed", Observed: "no HTTP response arrived: " + why,
						AssertionsEnforced: enforced, AssertionsEnforcedCount: 1}
				}
				return report.StepResult{Status: "failed", Observed: "the target was not shown to be unreachable: " + why,
					FailedClaims:       []report.FailedClaim{{Claim: scenario.UnreachableClaim, Observed: why}},
					AssertionsEnforced: enforced, AssertionsEnforcedCount: 1}
			}
			_ = resp.Body.Close()
			obs := fmt.Sprintf("http status %d", resp.StatusCode)
			return report.StepResult{Status: "failed",
				Observed:           obs + " arrived — the network let the request through",
				FailedClaims:       []report.FailedClaim{{Claim: scenario.UnreachableClaim, Observed: obs}},
				AssertionsEnforced: enforced, AssertionsEnforcedCount: 1}
		},
	}
}

package chain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// MoneyWriteRefusal prefixes an http chain step's observed text when the money_writes spend check
// (T5.4 follow-up, 2026-09-26) refuses to send a request — mirrors argus.MoneyGuardRefusal's wording
// convention. A separate constant, not a shared one: argus imports this package, so the reverse
// would cycle.
const MoneyWriteRefusal = "refused before sending: its declared money_writes spend limit refused it: "

// AC-D20 — THE "http" CHAIN STEP: a multi-request REST lifecycle a chain sequences alongside its
// `mcp`/`ui` steps. The concrete case is a Coder workspace: POST create (save the id), GET it
// repeatedly until it is ready, POST stop, POST delete — the delete MUST run even when an earlier
// step failed, so nothing is left on the SUT.
//
// ⛔ NO PARALLEL EXECUTOR. This is a Step, built the same way chain.MCPStep is, and it runs
// through the SAME chain.Run (chain.go) — the SAME continue-past-failure (rule 2), the SAME
// not-measured/never-fired gate for an unresolved `${saved.<var>}` (rule 3), the SAME
// ran-after-failure marking (rule 4). It reuses the mcp body-assertion judge (mcp.BodyAssertsMiss)
// rather than inventing a second assertion language, and the same `${saved.<var>}` capture store
// (bindSaved / captureFromJSON) an mcp step publishes into and reads from.

// httpClientTimeout bounds ONE request/response round trip. It is independent of a Poll's own
// Timeout, which bounds the whole retry loop, not any single attempt.
const httpClientTimeout = 30 * time.Second

// maxHTTPBodyBytes caps how much of a response body this engine reads — enough for any REST
// resource's JSON representation, small enough that a misbehaving SUT streaming forever cannot
// hang a run.
const maxHTTPBodyBytes = 1 << 20 // 1 MiB

// httpClient is package-level so tests never need one injected — every caller here fires at a
// loopback httptest server or a real SUT; nothing about this engine is mockable state.
var httpClient = &http.Client{}

// Poll is a chain http step's optional retry block: re-send the request, on Interval, until the
// step's claims pass or Timeout elapses. nil (on HTTPStep) means "fire once".
type Poll struct {
	Timeout  time.Duration
	Interval time.Duration
}

// httpJudged is what one HTTP attempt produced, judged against the step's declared claims.
type httpJudged struct {
	status      int
	pass        bool
	reachedBody bool // status matched (or none declared) — body assertions were actually evaluated
	observed    string
	// failed is the per-claim record of THIS attempt: each claim that did not hold,
	// as written, with what this attempt observed. A polled step keeps the last attempt's.
	failed []report.FailedClaim
	// failedOmitted counts the claims that did not hold on THIS attempt but are past the list's backstop.
	failedOmitted int
}

// HTTPStep builds the native-HTTP step. method/urlTmpl/headers/bodyTmpl are already resolved for
// ${cid}/${cid8}/${correlation_id}/env (chain_scenario.go, before the chain is assembled) and still
// carry any `${saved.<var>}` verbatim — bound here, at RUN time, exactly as an mcp step's args are.
//
// wantStatus 0 means "no status claim was declared" (the body assertions alone decide the step,
// same as an mcp step with no `body …` bullet passing on its error plane alone). save publishes
// into the SAME capture store an mcp step does, from the PLAIN JSON response body (captureFromJSON,
// not captureFrom's MCP-envelope unwrap). poll nil = one request. always exempts this step from
// rule 3's gate (see ChainStep.Always in internal/scenario/chain.go for why).
func HTTPStep(name, method, urlTmpl string, headers map[string]string, bodyTmpl string, wantStatus int, bodyWant []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, always bool,
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger) Step {
	return HTTPStepWithOutput(name, method, urlTmpl, headers, bodyTmpl, wantStatus, bodyWant, save, poll, always, moneyAllow, moneyLedger, nil)
}

// HTTPStepWithOutput is HTTPStep plus an optional OutputCapture (ARGUS-CMP-3): when oc is not nil the
// response that was JUDGED (for a poll, the last attempt) is handed to compare.BuildRecord and the
// record rides StepResult.Output. oc nil is HTTPStep exactly: nothing is recorded and nothing else differs.
func HTTPStepWithOutput(name, method, urlTmpl string, headers map[string]string, bodyTmpl string, wantStatus int, bodyWant []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, always bool,
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger, oc *OutputCapture) Step {
	// needs: can EVERY ${saved.<var>} this step references actually be resolved right now? The same
	// question MCPStep.Needs asks of args+expect, asked here of url+headers+body.
	needs := func(vars map[string]string) error {
		if _, err := bindSaved(urlTmpl, vars); err != nil {
			return err
		}
		for _, v := range headers {
			if _, err := bindSaved(v, vars); err != nil {
				return err
			}
		}
		if _, err := bindSaved(bodyTmpl, vars); err != nil {
			return err
		}
		// the step's CLAIMS may carry ${saved.<var>} too (an unsaved one is
		// not-measured, like the same reference in the url).
		return needsBodyAsserts(bodyWant, vars)
	}

	buildRequest := func(vars map[string]string) (*http.Request, string, error) {
		return buildHTTPRequest(method, urlTmpl, headers, bodyTmpl, vars)
	}

	// judge takes the BOUND copy of the step's body claims (bindBodyAsserts, once per attempt); the
	// claim as written (bodyWant) is what enforced() reports.
	judge := func(status int, raw []byte, boundWant []mcp.BodyAssert, vars map[string]string) httpJudged {
		if wantStatus != 0 && status != wantStatus {
			return httpJudged{status: status, pass: false,
				observed: fmt.Sprintf("http responded status %d (want %d)", status, wantStatus),
				// the status claim missed; the body claims were never evaluated, so none is listed
				failed: []report.FailedClaim{{Claim: fmt.Sprintf("status = %d", wantStatus), Observed: strconv.Itoa(status)}}}
		}
		// item 25: numeric comparisons report a DISTINCT reason when the miss was a non-numeric
		// observed value — still reality-only (never the threshold, never the field), so the test
		// hat learns WHY without either hat learning WHAT was asserted.
		if miss, nonNumeric := mcp.BodyAssertsMissReason(string(raw), boundWant); miss {
			reason := "the response body did not satisfy the scenario's body assertion(s)"
			if nonNumeric {
				reason = "a numeric comparison in the scenario's body assertion(s) found a non-numeric observed value"
			}
			failed, omitted := failedBodyClaims(string(raw), bodyWant, boundWant, vars, nil)
			return httpJudged{status: status, pass: false, reachedBody: true,
				// VR-C8 twin: reality-only, never echoes the asserted value (the test hat reads it via
				// AssertionsEnforced/failure.expected). The status IS named — a poll timeout must name
				// "the last observed status" regardless of which half of the claim missed (AC-D20).
				observed: fmt.Sprintf("http status %d matched but %s %s", status, reason, chainClaimsNote),
				// the claim AS WRITTEN (bodyWant, the closure's) with what this attempt's body showed
				failed:        failed,
				failedOmitted: omitted}
		}
		return httpJudged{status: status, pass: true, reachedBody: true, observed: fmt.Sprintf("http status %d", status)}
	}

	enforced := func() []string {
		var out []string
		if wantStatus != 0 {
			out = append(out, fmt.Sprintf("status = %d", wantStatus))
		}
		out = append(out, EnforcedAssertions(mcp.Expect{Body: bodyWant})...)
		return out
	}

	// run is the step body. chainDeadline is the chain's `## TIMEOUT` deadline (zero = none declared);
	// it bounds each request (requestTimeout, #621). A Poll's own Timeout still bounds the retry loop.
	run := func(cid string, vars map[string]string, chainDeadline time.Time) report.StepResult {
		{
			timeout, interval := time.Duration(0), time.Second
			if poll != nil {
				timeout, interval = poll.Timeout, poll.Interval
			}
			deadline := time.Now().Add(timeout)
			attempts := 0
			var last httpJudged
			for {
				attempts++
				req, resolvedBody, berr := buildRequest(vars)
				if berr != nil {
					// An authoring error (an unresolved ${saved.<var>}), never a SUT failure — fail
					// here, naming the variable, and never send the literal placeholder over the wire
					// (the same contract chain.MCPStep keeps for its args). NEVER retried: vars do not
					// change between attempts of one step's own poll loop, so a second try would fail
					// identically.
					return report.StepResult{Status: "failed", Observed: berr.Error()}
				}
				// bind the claims on EVERY attempt, before anything is sent. An unsaved
				// variable, or a saved threshold that is not a number, fails the step BY NAME (the value
				// itself is never printed) — never a silent miss, never a pass, and never retried.
				boundWant, cerr := bindBodyAsserts(bodyWant, vars)
				if cerr != nil {
					return report.StepResult{Status: "failed", Observed: "content assertion: " + cerr.Error()}
				}
				// money_writes (item 3) — THE SPEND CHECK, AT THE DOOR THAT SEES THE RESOLVED
				// VALUE: req is built (${saved.<var>} bound), so req.URL.Path is the ACTUAL path this
				// attempt is about to hit and resolvedBody is the ACTUAL body. Matched against the
				// SAME allowlist every other door consults. Nothing is sent on a refusal — `error`,
				// never `failed`, and NEVER retried (an authoring/policy refusal, not a SUT problem).
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
				allowed := requestTimeout(chainDeadline)
				ctx, cancel := context.WithTimeout(context.Background(), allowed)
				sent := time.Now()
				resp, derr := httpClient.Do(req.WithContext(ctx))
				if derr != nil {
					cutByDeadline := !chainDeadline.IsZero() && errors.Is(ctx.Err(), context.DeadlineExceeded)
					cancel()
					if cutByDeadline {
						// #621: the CHAIN's budget ended this request, not a refused connection. A timeout,
						// not "SUT unreachable": status failed, so isUnanswered does not stop-and-relabel it.
						return report.StepResult{Status: "failed",
							Observed: deadlineCutObserved(allowed, time.Since(sent), derr)}
					}
					// No response at all — an execution error (VR-C8 unreachable/transport twin), never
					// a content failure: mirrors chain.MCPStep's `v.Plane == "unreachable"` mapping to
					// report.StatusError, which chain.Run's rule 5 stops the WHOLE chain dead on. A poll
					// does not retry through this — a SUT that cannot be reached is not "still starting
					// up", it is gate-infra, and firing more requests into it gains nothing (VR12-CH2).
					return report.StepResult{Status: report.StatusError,
						Observed: "http request failed: " + derr.Error()}
				}
				// One byte more than the cap is read so a recorder can tell "exactly the cap" from "more than
				// the cap" (compare.MaxBodyBytes); the judge below still sees at most maxHTTPBodyBytes,
				// exactly as before.
				full, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes+1))
				respHeader := resp.Header
				_ = resp.Body.Close()
				cancel()
				raw := full
				if len(raw) > maxHTTPBodyBytes {
					raw = raw[:maxHTTPBodyBytes]
				}

				last = judge(resp.StatusCode, raw, boundWant, vars)
				if last.pass {
					st := report.StepResult{Status: "passed", Observed: last.observed}
					if last.pass || last.reachedBody {
						st.AssertionsEnforced = enforced()
						st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
					}
					if len(save) > 0 {
						captured := make(map[string]string, len(save))
						for varName, sp := range save {
							val, fail := saveFromHTTP(raw, varName, sp)
							if fail != "" {
								return report.StepResult{Status: "failed", Observed: fail,
									Output: recordOutput(oc, name, resp.StatusCode, respHeader, full, vars, nil)}
							}
							captured[varName] = val
						}
						st.Captured = captured
					}
					st.Output = recordOutput(oc, name, resp.StatusCode, respHeader, full, vars, st.Captured)
					return st
				}

				if poll == nil || time.Now().After(deadline) {
					st := report.StepResult{Status: "failed", Observed: last.observed, FailedClaims: last.failed, FailedClaimsOmitted: last.failedOmitted}
					if poll != nil {
						st.Observed = fmt.Sprintf("poll timed out after %s (%d attempt(s)); last observed: %s",
							timeout, attempts, last.observed)
					}
					if last.reachedBody {
						st.AssertionsEnforced = enforced()
						st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
					}
					st.Output = recordOutput(oc, name, resp.StatusCode, respHeader, full, vars, nil)
					return st
				}
				time.Sleep(interval)
			}
		}
	}

	return Step{
		Name:   name,
		Always: always,
		Needs: func(vars map[string]string) error {
			if always {
				return nil // AC-D20: the cleanup step attempts regardless — see ChainStep.Always
			}
			return needs(vars)
		},
		Run: func(cid string, vars map[string]string) report.StepResult {
			return run(cid, vars, time.Time{})
		},
		RunUntil: run,
	}
}

// requestTimeout is how long ONE http attempt may take. No chain budget (zero chainDeadline): the
// fixed httpClientTimeout, today's behaviour byte-for-byte. With a budget (#621): the time the chain
// has LEFT, so a slow endpoint is not cut at 30 s inside a longer `## TIMEOUT`; once the budget is
// spent (an `always` cleanup step still fires) the attempt gets only stepGrace, which is also how
// long callStep waits past the deadline. Either way a request cannot outlive deadline + stepGrace.
func requestTimeout(chainDeadline time.Time) time.Duration {
	if chainDeadline.IsZero() {
		return httpClientTimeout
	}
	if left := time.Until(chainDeadline); left > 0 {
		return left
	}
	return stepGrace
}

// deadlineCutObserved words a request that the chain's deadline, not the SUT, cut off.
func deadlineCutObserved(allowed, elapsed time.Duration, derr error) string {
	return fmt.Sprintf("request cut off by the chain's ## TIMEOUT deadline (the chain had %.1fs left when it was sent) after %.1fs: %s",
		allowed.Seconds(), elapsed.Seconds(), derr.Error())
}

// buildHTTPRequest binds ${saved.<var>} into the url, headers and body and builds the request. Shared by
// HTTPStep and UnreachableHTTPStep so the two can never build a request differently.
func buildHTTPRequest(method, urlTmpl string, headers map[string]string, bodyTmpl string, vars map[string]string) (*http.Request, string, error) {
	u, err := bindSaved(urlTmpl, vars)
	if err != nil {
		return nil, "", err
	}
	var body io.Reader
	var resolvedBody string
	hasBody := strings.TrimSpace(bodyTmpl) != ""
	if hasBody {
		b, err := bindSaved(bodyTmpl, vars)
		if err != nil {
			return nil, "", err
		}
		resolvedBody = b
		body = bytes.NewReader([]byte(b))
	}
	req, err := http.NewRequest(strings.ToUpper(strings.TrimSpace(method)), u, body)
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		hv, err := bindSaved(v, vars)
		if err != nil {
			return nil, "", err
		}
		// item 25 — THE BASIC-AUTH HELPER, BUILT AT RUN TIME, PER ATTEMPT. An `Authorization`
		// value of the form `Basic ${basic_auth:<user>:<PASSWORD_ENV_VAR>}` never carries a
		// literal secret (validated at authoring time, scenario.ValidateBasicAuthHeader): the
		// password is read from the named env var HERE, in the executor, every time this step
		// actually fires (a poll re-reads it on every attempt, so a rotated secret takes effect
		// without a restart). An unset env var refuses the step by name rather than encoding an
		// empty credential silently.
		if http.CanonicalHeaderKey(k) == "Authorization" {
			if user, passEnvVar, ok := scenario.BasicAuthMarker(hv); ok {
				resolved, berr := scenario.ResolveBasicAuthHeader(user, passEnvVar)
				if berr != nil {
					return nil, "", berr
				}
				hv = resolved
			}
		}
		req.Header.Set(k, hv)
	}
	if hasBody && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, resolvedBody, nil
}

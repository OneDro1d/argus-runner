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
	// statusMissed: the step's STATUS claim is the one that did not hold (the body claims were never
	// evaluated). Only then does the failed step carry a body excerpt (StepResult.BodyExcerpt).
	statusMissed bool
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
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger, opts ...HTTPOption) Step {
	return HTTPStepWithOutput(name, method, urlTmpl, headers, bodyTmpl, wantStatus, bodyWant, save, poll, always, moneyAllow, moneyLedger, nil, opts...)
}

// HTTPOption adds an optional behaviour to an http step without changing the signature every caller uses.
type HTTPOption func(*httpOpts)

type httpOpts struct {
	elapsed       *scenario.ElapsedClaim
	scrub         func(string) string
	textBody      bool // bodyTmpl is TEXT (WithTextBody)
	moneyHandling bool
}

// WithMoneyHandling tells the step it belongs to a run of a money-handling system:
// the request it is about to send is judged against the money-path rule on its RESOLVED method and
// path, with the same list the file was judged with. Without it the step still applies the check
// whenever it is handed a non-empty list (a list only exists on a money-handling system), but a
// money-handling system that declares NO entry hands an empty one — so the option is how the step
// knows the run is guarded at all.
func WithMoneyHandling(on bool) HTTPOption {
	return func(o *httpOpts) { o.moneyHandling = on }
}

// MoneyResolvedRefusal prefixes an http chain step's observed text when the request it resolved to is
// refused by the money-path rule at the moment of sending. Byte-equal to argus.MoneyGuardRefusal (a
// separate constant for the same reason as MoneyWriteRefusal: argus imports this package).
const MoneyResolvedRefusal = "refused before firing: this SUT is declared money_handling, and "

// moneyResolvedRefusal judges the request a step has just built, when the step belongs to a guarded
// run and its url holds a `${saved.` the file could not be judged on. It returns "" when the request
// may go, else the refusal text. See scenario.ResolvedRequestViolations for the rule.
//
// ⛔ ONLY when the url template names a saved value, or the list covers the file's own url. A url with
// neither is the very text the file's guard already judged (cid and environment values aside), and
// re-judging it on a url that now carries the run's correlation id — which embeds the scenario id —
// would refuse a check that passed the file for a reason the author cannot see.
func moneyResolvedRefusal(guarded bool, name, method, urlTmpl string, moneyAllow scenario.MoneyWriteAllowlist, req *http.Request) string {
	if !guarded && len(moneyAllow) == 0 {
		return ""
	}
	if !strings.Contains(urlTmpl, "${saved.") && moneyAllow.Match(method, urlTmpl) == nil {
		return ""
	}
	v := scenario.ResolvedRequestViolations("chain step "+name, req.Method, urlTmpl, req.URL, moneyAllow)
	if len(v) == 0 {
		return ""
	}
	return MoneyResolvedRefusal + strings.Join(v, "; ")
}

// errRedirectRefused is what a guarded client's CheckRedirect returns for a hop it will not follow; the
// client then closes the redirect response and Do returns an error wrapping it.
var errRedirectRefused = errors.New("redirect refused by the money-path rule")

// guardedRedirectClient is the client a step on a guarded run sends with: a copy of base
// (same Transport, Jar, Timeout) with a CheckRedirect that judges every hop BEFORE it is followed. The
// shared httpClient is untouched, so an unguarded run keeps default redirect following and tests that
// replace httpClient keep working. FAIL CLOSED: when a hop cannot be judged it is not followed.
//
// A hop is refused when it (1) goes to another scheme+host+port than the request the step built — an
// allowlist entry names a path of THIS system; (2) is a request the rule refuses
// (scenario.ResolvedRequestViolations, same list, on the hop's method and url); or (3) is covered by a
// `spends: true` entry — a spending request is never re-sent on a redirect. The sentence names the url
// as written in the file (urlTmpl), never the target. The first refusal is written to *refusal.
func guardedRedirectClient(base *http.Client, name, urlTmpl string, allow scenario.MoneyWriteAllowlist, refusal *string) *http.Client {
	c := *base
	c.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		refuse := func(why string) error {
			*refusal = MoneyResolvedRefusal + why + "; this was a redirect from " + urlTmpl + " and it was not followed"
			return errRedirectRefused
		}
		where := "a redirect of chain step " + name
		if len(via) == 0 || via[0].URL == nil || next.URL == nil {
			return refuse(where + " could not be judged (fail-closed)")
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		first := via[0].URL
		if !strings.EqualFold(next.URL.Scheme, first.Scheme) || !strings.EqualFold(next.URL.Host, first.Host) {
			return refuse(where + " leads to another scheme, host or port than the request the step built — a money_writes.allow entry names a path of this system only")
		}
		if v := scenario.ResolvedRequestViolations(where, next.Method, urlTmpl, next.URL, allow); len(v) > 0 {
			return refuse(strings.Join(v, "; "))
		}
		if e := allow.MatchResolved(next.Method, next.URL); e != nil && e.Spends {
			return refuse(where + " is covered by a spending money_writes.allow entry — a spending request is never re-sent on a redirect")
		}
		return nil
	}
	return &c
}

// WithBodyScrub gives the step the instance's credential scrub (the function compare mode records outputs
// with, internal/argus credentialScrub), applied to the body excerpt of a status miss BEFORE it is cut.
// nil is no credential scrub; the saved values are always redacted regardless.
func WithBodyScrub(f func(string) string) HTTPOption {
	return func(o *httpOpts) { o.scrub = f }
}

// WithElapsed gives the step an `elapsed` claim: the step's one request must answer inside
// the bound. nil is no claim.
func WithElapsed(c *scenario.ElapsedClaim) HTTPOption {
	return func(o *httpOpts) { o.elapsed = c }
}

// elapsedMissObserved is the BUILDER-VISIBLE text of an `elapsed` claim that did not hold. It carries the
// measured duration (the system's own behaviour) and says a timing claim failed — and it never carries the
// bound, which is an expected value. The bound is only in the author-only failed-claim record.
func elapsedMissObserved(tookMs int64) string {
	return fmt.Sprintf("the response took %d ms and a timing claim did not hold", tookMs)
}

// HTTPStepWithOutput is HTTPStep plus an optional OutputCapture (ARGUS-CMP-3): when oc is not nil the
// response that was JUDGED (for a poll, the last attempt) is handed to compare.BuildRecord and the
// record rides StepResult.Output. oc nil is HTTPStep exactly: nothing is recorded and nothing else differs.
func HTTPStepWithOutput(name, method, urlTmpl string, headers map[string]string, bodyTmpl string, wantStatus int, bodyWant []mcp.BodyAssert, save map[string]scenario.SaveSpec, poll *Poll, always bool,
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger, oc *OutputCapture, opts ...HTTPOption) Step {
	var o httpOpts
	for _, opt := range opts {
		opt(&o)
	}
	elapsedClaim := o.elapsed
	bodyScrub := o.scrub
	textBody := o.textBody
	moneyGuarded := o.moneyHandling
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
		if textBody {
			if _, err := bindSavedText(bodyTmpl, vars); err != nil {
				return err
			}
		} else if b, err := bindSavedBody(bodyTmpl, vars); err != nil {
			return err
		} else if err := checkBoundJSON(bodyTmpl, b, vars, "the step's body"); err != nil {
			return err
		}
		// the step's CLAIMS may carry ${saved.<var>} too (an unsaved one is
		// not-measured, like the same reference in the url).
		return needsBodyAsserts(bodyWant, vars)
	}

	buildRequest := func(vars map[string]string) (*http.Request, string, error) {
		return buildHTTPRequest(method, urlTmpl, headers, bodyTmpl, vars, textBody)
	}

	// judge takes the BOUND copy of the step's body claims (bindBodyAsserts, once per attempt); the
	// claim as written (bodyWant) is what enforced() reports.
	judge := func(status int, raw []byte, boundWant []mcp.BodyAssert, vars map[string]string, took time.Duration) httpJudged {
		// the timing claim is judged on THIS attempt's one request. Its miss is a `failed`
		// claim like any other — never an error, never a timeout.
		elapsedMissed := elapsedClaim != nil && !elapsedClaim.Holds(took)
		elapsedFailed := func() report.FailedClaim {
			// AUTHOR-ONLY: the bound is the expected value, so it is only ever written here.
			return report.FailedClaim{Claim: elapsedClaim.String(),
				Observed: fmt.Sprintf("took %d ms, bound %d ms", took.Milliseconds(), elapsedClaim.MaxMs)}
		}
		if wantStatus != 0 && status != wantStatus {
			// the wanted status is an expected value, so it is NOT in this builder-visible
			// text; it is in the author-only failed-claim record below (`status = <want>`).
			return httpJudged{status: status, pass: false, statusMissed: true,
				observed: fmt.Sprintf("http responded status %d; the status claim did not hold", status),
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
			if elapsedMissed {
				failed = append(failed, elapsedFailed())
				reason += "; " + elapsedMissObserved(took.Milliseconds())
			}
			return httpJudged{status: status, pass: false, reachedBody: true,
				// VR-C8 twin: reality-only, never echoes the asserted value (the test hat reads it via
				// AssertionsEnforced/failure.expected). The status IS named — a poll timeout must name
				// "the last observed status" regardless of which half of the claim missed (AC-D20).
				observed: fmt.Sprintf("http status %d matched but %s %s", status, reason, chainClaimsNote),
				// the claim AS WRITTEN (bodyWant, the closure's) with what this attempt's body showed
				failed:        failed,
				failedOmitted: omitted}
		}
		if elapsedMissed {
			return httpJudged{status: status, pass: false, reachedBody: true,
				observed: fmt.Sprintf("http status %d matched but %s %s", status, elapsedMissObserved(took.Milliseconds()), chainClaimsNote),
				failed:   []report.FailedClaim{elapsedFailed()}}
		}
		return httpJudged{status: status, pass: true, reachedBody: true, observed: fmt.Sprintf("http status %d", status)}
	}

	enforced := func() []string {
		var out []string
		if wantStatus != 0 {
			out = append(out, fmt.Sprintf("status = %d", wantStatus))
		}
		out = append(out, EnforcedAssertions(mcp.Expect{Body: bodyWant})...)
		if elapsedClaim != nil {
			out = append(out, elapsedClaim.String())
		}
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
				// AND THE RESOLVED PATH IS WHAT IS ALLOWED, BEFORE THE SPEND CHECK. The file's
				// guard read `${saved.<var>}` as text; a value the SUT answered with can turn the url into an
				// order path, or carry it from the route an entry allowed to another. The request is judged here
				// with the SAME rule and list (scenario.ResolvedRequestViolations); nothing is sent on a refusal.
				if msg := moneyResolvedRefusal(moneyGuarded, name, method, urlTmpl, moneyAllow, req); msg != "" {
					return report.StepResult{Status: report.StatusError, Observed: msg}
				}
				if entry := moneyAllow.MatchResolved(req.Method, req.URL); entry != nil && entry.Spends {
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
				// A redirect is a SECOND request, chosen by the server's answer: on a guarded
				// run every hop is judged before it is followed.
				client := httpClient
				var redirectRefusal string
				if moneyGuarded || len(moneyAllow) > 0 {
					client = guardedRedirectClient(httpClient, name, urlTmpl, moneyAllow, &redirectRefusal)
				}
				resp, derr := client.Do(req.WithContext(ctx))
				if derr != nil && redirectRefusal != "" {
					cancel()
					// A policy refusal, not a SUT problem: `error`, never retried by a poll.
					return report.StepResult{Status: report.StatusError, Observed: redirectRefusal}
				}
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
				// the clock stops when the whole response body has been read — the one
				// request of THIS attempt (a poll's earlier attempts and its sleeps are not in it).
				took := time.Since(sent)
				tookMs := took.Milliseconds()
				respHeader := resp.Header
				_ = resp.Body.Close()
				cancel()
				raw := full
				if len(raw) > maxHTTPBodyBytes {
					raw = raw[:maxHTTPBodyBytes]
				}

				// A body LONGER than the cap is never judged or saved from as
				// though the prefix were the whole. When the step would read the body (a body claim of any form,
				// or a save) the step ends `error` ("could not check"), on the attempt that saw it - a body
				// does not shrink by waiting, so a poll does not go on. A missed STATUS claim does not depend on
				// the body and keeps its verdict below; a step that reads the body for nothing is unchanged.
				if len(full) > maxHTTPBodyBytes && (len(bodyWant) > 0 || len(save) > 0) &&
					!(wantStatus != 0 && resp.StatusCode != wantStatus) {
					return report.StepResult{Status: report.StatusError, Observed: httpBodyOverLimitObserved(resp.StatusCode), ElapsedMs: &tookMs,
						ObservedStatus: observedHTTPStatus(resp.StatusCode)}
				}

				// AC-F2.1: from here on every result carries the status of THIS attempt's response, the one judged (a
				// poll's earlier attempts are gone with their loop iteration), through observedHTTPStatus — never wantStatus.
				last = judge(resp.StatusCode, raw, boundWant, vars, took)
				if last.pass {
					st := report.StepResult{Status: "passed", Observed: last.observed, ElapsedMs: &tookMs, ObservedStatus: observedHTTPStatus(resp.StatusCode)}
					if last.pass || last.reachedBody {
						st.AssertionsEnforced = enforced()
						st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
					}
					if len(save) > 0 {
						captured := make(map[string]string, len(save))
						for varName, sp := range save {
							val, fail := saveFromHTTP(raw, varName, sp)
							if fail != "" {
								return report.StepResult{Status: "failed", Observed: fail, ObservedStatus: observedHTTPStatus(resp.StatusCode),
									Output: recordOutput(oc, name, resp.StatusCode, respHeader, full, vars, nil)}
							}
							recordCapture(captured, varName, sp, val)
						}
						st.Captured = captured
					}
					st.Output = recordOutput(oc, name, resp.StatusCode, respHeader, full, vars, st.Captured)
					return st
				}

				if poll == nil || time.Now().After(deadline) {
					st := report.StepResult{Status: "failed", Observed: last.observed, FailedClaims: last.failed, FailedClaimsOmitted: last.failedOmitted, ElapsedMs: &tookMs,
						ObservedStatus: observedHTTPStatus(resp.StatusCode)}
					if poll != nil {
						st.Observed = fmt.Sprintf("poll timed out after %s (%d attempt(s)); last observed: %s",
							timeout, attempts, last.observed)
					}
					if last.statusMissed {
						st.BodyExcerpt = bodyExcerpt(raw, len(full) > maxHTTPBodyBytes, vars, bodyScrub)
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

// httpBodyOverLimitObserved is the BUILDER-VISIBLE sentence of a response body longer than maxHTTPBodyBytes
// that the step would have had to read. It names the limit and the status, never a byte of the body.
func httpBodyOverLimitObserved(status int) string {
	return fmt.Sprintf("http status %d, and the response body is over the %d-byte limit of what a claim or a save can read, so the body was not judged",
		status, maxHTTPBodyBytes)
}

// observedHTTPStatus is what an http step records in StepResult.ObservedStatus (AC-F2.1): code when it is a
// three-digit status code, 100-999, and 0 (nothing is written) otherwise. Go's HTTP/1.1 client accepts any code from
// "000" to "999", also one written with a plus sign ("+42"), and its HTTP/2 client any integer, negative or huge; the
// limited report sends this field, so only the closed shape of an HTTP status code may reach it.
func observedHTTPStatus(code int) int {
	if code < 100 || code > 999 {
		return 0
	}
	return code
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
//
// textBody: bodyTmpl is TEXT, sent as it stands once ${saved.<var>} is bound raw. The second
// result, which the money guard reads, is then the text as a JSON string (guardViewOfText), never the text.
func buildHTTPRequest(method, urlTmpl string, headers map[string]string, bodyTmpl string, vars map[string]string, textBody bool) (*http.Request, string, error) {
	u, err := bindSaved(urlTmpl, vars)
	if err != nil {
		return nil, "", err
	}
	var body io.Reader
	var resolvedBody string
	hasBody := strings.TrimSpace(bodyTmpl) != ""
	if textBody {
		hasBody = bodyTmpl != "" // a text body is its bytes: only the empty text is no body
	}
	if hasBody && textBody {
		b, err := bindSavedText(bodyTmpl, vars)
		if err != nil {
			return nil, "", err
		}
		resolvedBody = guardViewOfText(b)
		body = bytes.NewReader([]byte(b))
	} else if hasBody {
		b, err := bindSavedBody(bodyTmpl, vars)
		if err != nil {
			return nil, "", err
		}
		if err := checkBoundJSON(bodyTmpl, b, vars, "the step's body"); err != nil {
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

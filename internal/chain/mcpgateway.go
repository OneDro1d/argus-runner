package chain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AN `mcp` STEP THAT READS AN ANSWER A GATEWAY WRAPS.
//
//	extract      the first capture group of the first match in the answer's content text (or, with a path, in the
//	             decoded JSON string at that path) becomes THE DOCUMENT the step's `body extracted …` claims and its
//	             `from: extracted` saves read (JSON when it parses as JSON, else text). A plain `body …` claim and a
//	             save without `from` read the answer itself, as on any step.
//	unavailable  an answer (content text or JSON-RPC error) matching the author's pattern means "could not check":
//	             the step is `error`, never passed, never failed
//	poll         re-send the call while the answer is unavailable or the claims miss (needs idempotent)
//
// ⛔ NOTHING HERE TURNS AN UNAVAILABLE OR MALFORMED ANSWER INTO `passed`. An answer without the shape `extract`
// expects is `failed`; an unavailable one is `error`; both are said in sentences that carry neither the pattern, nor
// the answer, nor a claim. The answer's text goes to the author only (BodyExcerpt, which the product hat never gets).
// ⛔ NO HEURISTIC: nothing is unavailable, and nothing is extracted, unless the author declared it.

// MCPOptions are the new, optional parts of an mcp step. The zero value is the step as it always was.
type MCPOptions struct {
	// Extract is the regex of the `extract` block ("" = none): exactly one capture group (the preflight refuses
	// anything else; the step refuses it again).
	Extract string
	// ExtractPath (nil = none) is the dot path of a JSON string in the content text; the Extract regex then runs on the
	// decoded value of that string instead of the raw text.
	ExtractPath *string
	// Unavailable is the regex of the `unavailable` block ("" = none).
	Unavailable string
	// Poll re-sends the call (nil = once). Idempotent must be true with it, or the step sends nothing.
	Poll       *Poll
	Idempotent bool
	// Scrub cleans an answer excerpt of the target's credentials (nil = none).
	Scrub func(string) string
	// Sign signs every call of the step with an Ed25519 key (nil = unsigned, as it always was).
	// A problem signing is the harness's: the step is `error` (grey) with a sentence naming the key reference.
	Sign *SignPlan
}

// MCPUnavailableObserved is the builder-visible sentence of an unavailable answer. It names neither the declared
// pattern nor the answer.
const MCPUnavailableObserved = "the system's answer matched the unavailable pattern this check declares, so the check could not be made and nothing was judged"

// MCPNoExtractPathObserved is the builder-visible sentence of an answer whose text is not JSON, or whose `extract`
// `path` does not lead to a string in it.
const MCPNoExtractPathObserved = "the answer did not have the shape this check expects (its text is not JSON, or `extract`'s `path` does not lead to a string in it), so the claims were not judged"

// MCPNoExtractObserved is the builder-visible sentence of an answer without the shape `extract` expects (no match,
// or a capture that is empty or only whitespace).
const MCPNoExtractObserved = "the answer did not have the shape this check expects (its `extract` found nothing in it), so the claims were not judged"

func mcpTooLargeObserved() string {
	return fmt.Sprintf("the part of the answer `extract` captured is over the %d-byte limit of what a claim can judge - narrow the pattern",
		scenario.MCPExtractMaxBytes)
}

// splitExtractedClaims separates the claims written `body …` (about the answer's own text) from those written
// `body extracted …` (about the document `extract` cut out), keeping each group's order.
func splitExtractedClaims(all []mcp.BodyAssert) (raw, extracted []mcp.BodyAssert) {
	for _, a := range all {
		if a.Extracted {
			extracted = append(extracted, a)
		} else {
			raw = append(raw, a)
		}
	}
	return raw, extracted
}

func hasExtractedClaim(all []mcp.BodyAssert) bool {
	_, e := splitExtractedClaims(all)
	return len(e) > 0
}

// MCPStepWith is MCPStep plus MCPOptions. See MCPStep for the common parts.
func MCPStepWith(name string, cl *mcp.Client, tool, argsJSON string, expect mcp.Expect, save map[string]scenario.SaveSpec, opt MCPOptions) Step {
	var exRe, unRe *regexp.Regexp
	var cfgErr string
	if opt.Extract != "" {
		re, err := regexp.Compile(opt.Extract)
		switch {
		case err != nil:
			cfgErr = "the `extract` pattern does not compile"
		case re.NumSubexp() != 1:
			cfgErr = "the `extract` pattern must have exactly one capture group"
		default:
			exRe = re
		}
	}
	if opt.Unavailable != "" {
		re, err := regexp.Compile(opt.Unavailable)
		if err != nil {
			cfgErr = "the `unavailable` pattern does not compile"
		} else {
			unRe = re
		}
	}
	if opt.Poll != nil && !opt.Idempotent {
		cfgErr = "`poll` re-sends the tool call and needs \"idempotent\": true - nothing was sent"
	}
	// a claim `body extracted …` or a save "from": "extracted" reads the document `extract` cuts out. Without an
	// `extract` there is no such document: the step sends nothing (the preflight refuses it first; this is the backstop).
	if exRe == nil && cfgErr == "" {
		if hasExtractedClaim(expect.Body) {
			cfgErr = "a `body extracted …` claim needs the step to declare `extract` - nothing was sent"
		}
		for _, sp := range save {
			if sp.From != "" {
				cfgErr = "a save with \"from\": \"extracted\" needs the step to declare `extract` - nothing was sent"
			}
		}
	}
	scrub := opt.Scrub

	st := Step{Name: name,
		// A step that can say "could not check" keeps its own sentence through chain.Run (OwnError).
		OwnError: unRe != nil || exRe != nil || opt.Sign != nil,
		// VR12-CH1 rule 3: can this step be given its inputs AT ALL? Asked before the call, using
		// the SAME bindSaved the args and the assertion use, so the answer cannot drift from what
		// the step would actually do. An unresolved ${saved.…} means the step that produces it did
		// not pass — this step was never tested, and firing it would only add an echo.
		Needs: func(vars map[string]string) error {
			b, err := bindSavedBody(argsJSON, vars)
			if err != nil {
				return err
			}
			if err := checkBoundJSON(argsJSON, b, vars, "the step's args"); err != nil {
				return err
			}
			return needsBodyAsserts(expect.Body, vars)
		},
	}
	run := func(cid string, vars map[string]string, chainDeadline time.Time) report.StepResult {
		if cfgErr != "" {
			return report.StepResult{Status: "failed", Observed: cfgErr}
		}
		bound, err := bindSavedBody(argsJSON, vars)
		if err != nil {
			// an authoring error, not a SUT failure — fail the step here and name the variable
			// rather than letting the literal placeholder reach the SUT and be misread as its bug.
			return report.StepResult{Status: "failed", Observed: err.Error()}
		}
		var args any = map[string]any{}
		if strings.TrimSpace(bound) != "" {
			if err := json.Unmarshal([]byte(bound), &args); err != nil {
				return report.StepResult{Status: "failed",
					Observed: "step args are not valid JSON after binding ${saved.…}: " + err.Error()}
			}
			if refsJSONSaved(argsJSON, vars) {
				// a JSON-saved value keeps every digit it was saved with. A float64 would turn
				// 12345678901234567890 into 12345678901234567000, so a step that carries one decodes its numbers
				// as text; every other step's args are decoded exactly as before.
				dec := json.NewDecoder(strings.NewReader(bound))
				dec.UseNumber()
				var exact any
				if dec.Decode(&exact) == nil {
					args = exact
				}
			}
		}
		// VR10-S2 (owner-locked): the CONTENT assertion binds ${saved.<var>} with the SAME bindSaved
		// as the args, into a fresh per-execution copy — `expect` itself is never mutated, because a
		// chain can be re-run and a mutated Expect would carry one run's id into the next. Bound here,
		// with the args and before the call, for the same reason the args are: an unbound variable is
		// an authoring error that names the variable, and the literal placeholder must never be
		// compared against the answer (it would silently pass or silently fail on the SUT's wording).
		want, err := bindExpect(expect, vars)
		if err != nil {
			return report.StepResult{Status: "failed", Observed: "content assertion: " + err.Error()}
		}

		// sign AFTER the args are bound (a field reads the bound args) and BEFORE anything is sent. A
		// problem here is the harness's, so the step is `error` and nothing reaches the SUT. The signature and the key id
		// ride on a COPY of the client, so the shared client's headers are never changed.
		callArgs, callClient := args, cl
		if opt.Sign != nil {
			so, serr := signCall(opt.Sign, args, vars)
			if serr != nil {
				return report.StepResult{Status: report.StatusError, Observed: serr.Error()}
			}
			callArgs = so.Args
			cp := *cl
			cp.ExtraHeaders = make(map[string]string, len(cl.ExtraHeaders)+len(so.Headers))
			for k, v := range cl.ExtraHeaders {
				cp.ExtraHeaders[k] = v
			}
			for k, v := range so.Headers {
				cp.ExtraHeaders[k] = v
			}
			callClient = &cp
		}

		// attempt sends the call ONCE and judges it. retry says the outcome is one a poll may try again: the
		// answer was unavailable, or the claims (the plane or the body) did not hold. An unreachable endpoint, a
		// transport failure, a rate limit and an over-large capture are never retried by the poll.
		attempt := func() (report.StepResult, bool) {
			// ONE request id per call, prefixed by the chain's correlation id (mcp.PerCallRequestID). It
			// used to be `cid` itself on every step, and a SUT enforcing request-id uniqueness refused
			// every step after the first as "request_id reused".
			res := callClient.Call(mcp.CallInput{Tool: tool, Args: callArgs, RequestID: mcp.PerCallRequestID(cid)})
			// VR10-R1 (owner D12): the chain path converges on the SAME judge, so it gets the same fix —
			// the SUT's declared throttle rides on the client, which is the endpoint this step calls.
			//
			// The judge is asked about the planes and the claims written `body …`, on the real answer, exactly as on
			// a step without `extract`. The claims written `body extracted …` are judged below, on the document.
			judgeWant := want
			judgeWant.Body, _ = splitExtractedClaims(want.Body)
			v := mcp.JudgeWithRateLimit(res, judgeWant, cl.RateLimit)
			notAnswer := v.Plane == "unreachable" || v.Plane == "transport" || v.Plane == mcp.PlaneRateLimited

			// `unavailable` is asked BEFORE the planes and the claims are allowed to decide anything: an answer that
			// says "could not check" is not a verdict on the claims. An unreachable endpoint, a transport failure
			// and a declared rate limit keep their own paths.
			if unRe != nil && !notAnswer && mcp.AnswerMatches(res, unRe) {
				out := report.StepResult{Status: report.StatusError, Observed: MCPUnavailableObserved}
				if text := mcp.BodyClaimText(res, want); text != "" {
					out.BodyExcerpt = bodyExcerpt([]byte(text), false, vars, scrub)
				}
				return out, true
			}

			doc := ""
			extMiss := false
			if exRe != nil && v.Pass {
				text := mcp.BodyClaimText(res, want)
				if opt.ExtractPath != nil {
					// the content text is a JSON document and the data sits inside one of its string fields: the regex
					// runs on that string's DECODED value (in the raw text its quotes are escaped)
					s, ok := mcp.JSONStringAt(text, *opt.ExtractPath)
					if !ok {
						return report.StepResult{Status: "failed", Observed: MCPNoExtractPathObserved,
							BodyExcerpt: bodyExcerpt([]byte(text), false, vars, scrub)}, true
					}
					text = s
				}
				m := exRe.FindStringSubmatch(text)
				// an empty (or whitespace-only) capture - a group that matched nothing, or an optional group that did
				// not take part - is not a document: it is the same outcome as an answer without the shape
				if len(m) < 2 || strings.TrimSpace(m[1]) == "" {
					return report.StepResult{Status: "failed", Observed: MCPNoExtractObserved,
						BodyExcerpt: bodyExcerpt([]byte(text), false, vars, scrub)}, true
				}
				doc = m[1]
				if len(doc) > scenario.MCPExtractMaxBytes {
					return report.StepResult{Status: report.StatusError, Observed: mcpTooLargeObserved()}, false
				}
				if _, extWant := splitExtractedClaims(want.Body); len(extWant) > 0 {
					if miss, _ := mcp.BodyAssertsMissReason(doc, extWant); miss {
						extMiss = true
						obs := mcp.BodyAssertObserved
						switch want.ErrorPlane {
						case mcp.PlaneTool:
							obs = mcp.BodyAssertObservedToolError
						case mcp.PlaneProtocol:
							obs = mcp.BodyAssertObservedProtocolError
						}
						v = mcp.Verdict{Pass: false, Plane: mcp.PlaneBody, Observed: obs}
					}
				}
			}

			st := report.StepResult{Status: "failed", Observed: v.Observed}
			if !v.Pass && v.Plane == mcp.PlaneBody {
				// a content-claim miss. The text points at the step's failed_claims —
				// the per-claim record, built here from what THIS call's answer showed — instead of a
				// failure record that does not exist. `expect` is the claims as written, `want` the
				// bound copy the judge compared.
				st.Observed = chainBodyNote(v.Observed)
				// the claims that were judged: `body extracted …` on the document when that is what missed, the others on
				// the answer's own text
				claimText := mcp.BodyClaimText(res, want)
				writtenRaw, writtenExt := splitExtractedClaims(expect.Body)
				boundRaw, boundExt := splitExtractedClaims(want.Body)
				if extMiss {
					claimText, writtenRaw, boundRaw = doc, writtenExt, boundExt
				}
				st.FailedClaims, st.FailedClaimsOmitted = failedBodyClaims(claimText, writtenRaw, boundRaw, vars, scrub)
			}
			if v.RateLimited {
				// NOT MEASURED, not failed. The rollup below promotes the whole chain scenario on it.
				st.Status = report.StatusError
				st.RateLimited = true
				st.RetryAfterMs = int(v.RetryAfter.Milliseconds())
			}
			// The ENFORCED marker (VR10-S2-12): the content assertions the judge actually evaluated — on a
			// success step once both planes passed, and on an expected error once its error plane matched
			// (V31-005). A plane failure never reaches them. Text for the test hat, count for both hats.
			//
			// ⚠ The `ErrorPlane == PlaneNone` condition is GONE, not widened: enforcedAssertions returns
			// nothing for a step with no body check, so a step that carries none is unchanged either way.
			if v.Pass || v.Plane == mcp.PlaneBody {
				// (AC-D67): EVERY claim is shown AS WRITTEN (`${saved.x}`), as the http,
				// amqp, db and logs steps show theirs; `want` (bound) is for the judge only. Printing the
				// bound copy put a value the SUT returned into the author's report, which the control
				// plane stores.
				st.AssertionsEnforced = enforcedAssertions(expect)
				st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
			}
			// PROB-1: carry the SUT's own envelope on a non-passing step. `Observed` alone
			// ("responder returned result.isError:true") is the same string for every tool-plane
			// failure, so the actual error text — the only thing that separates TEST_BUG from
			// CODE_BUG — was unreachable in-env for every chained scenario. Failures only: a
			// passing step's envelope is transcript, and report.json is already ~120KB for 45
			// scenarios.
			//
			// An `extract` step's claim miss does NOT carry it: the envelope holds the rows the claims were about,
			// and the failed claims (author-only) already show what was observed.
			if !v.Pass && !(exRe != nil && v.Plane == mcp.PlaneBody) {
				st.MCPEnvelope = res.Raw
			}
			// CAPTURE: extract what this step declared to save. Done AFTER judging (so the status is
			// known) but reported as a failure of THIS step when a path does not resolve — the break
			// belongs where the authoring mistake is, not two steps later when something else receives
			// an empty id. Only attempted on a passing step: a failed response has no payload worth
			// threading, and reporting a save error over a real assertion failure would bury it.
			if v.Pass && len(save) > 0 {
				captured := make(map[string]string, len(save))
				for varName, sp := range save {
					var val, fail string
					if exRe != nil && sp.From == scenario.SaveFromExtracted {
						// "from": "extracted": a path reads the document as JSON, a regex reads it as text. An entry
						// without `from` reads the answer itself, as on any step.
						val, fail = saveFromHTTP([]byte(doc), varName, sp)
					} else {
						val, fail = saveFromMCP(res.Raw, varName, sp)
					}
					if fail != "" {
						out := report.StepResult{Status: "failed", Observed: fail}
						if sp.From != scenario.SaveFromExtracted {
							out.MCPEnvelope = res.Raw
						}
						return out, false
					}
					recordCapture(captured, varName, sp, val)
				}
				st.Captured = captured
			}
			switch {
			case v.Pass:
				st.Status = "passed"
			case notAnswer && !v.RateLimited:
				// r3: no response from the SUT (endpoint unreachable / transport-handshake failure)
				// is an EXECUTION error, NOT a negative response — mirror the standalone MCP path
				// (mcp_scenario.go mcpRequestOutcome) so stepOutcome maps it to OutcomeError, keeping
				// the 3-way request classification consistent across the standalone and chained paths.
				st.Status = report.StatusError
			}
			return st, !v.Pass && !notAnswer
		}

		if opt.Poll == nil {
			st, _ := attempt()
			return st
		}
		timeout, interval := opt.Poll.Timeout, opt.Poll.Interval
		deadline := time.Now().Add(timeout)
		attempts := 0
		for {
			attempts++
			st, retry := attempt()
			if st.Status == "passed" || !retry {
				return st
			}
			if time.Now().After(deadline) || (!chainDeadline.IsZero() && time.Now().After(chainDeadline)) {
				// the last outcome stands: `error` if it was still unavailable, `failed` if a claim missed
				if st.Status == report.StatusError {
					st.Observed = fmt.Sprintf("poll timed out after %s (%d attempt(s)) and the answer was still unavailable: %s", timeout, attempts, st.Observed)
				} else {
					st.Observed = fmt.Sprintf("poll timed out after %s (%d attempt(s)); last observed: %s", timeout, attempts, st.Observed)
				}
				return st
			}
			time.Sleep(interval)
		}
	}
	st.Run = func(cid string, vars map[string]string) report.StepResult { return run(cid, vars, time.Time{}) }
	if opt.Poll != nil {
		st.RunUntil = run
	}
	return st
}

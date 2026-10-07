// Package chain is the M2.5 D3.6 runner-sequenced multi-step executor ("deliverable
// 6"). A chained scenario (MCP → DB → UI) cannot live inside one JMeter template — a
// runner-native MCP step has to be sequenced in Go alongside JMeter DB/UI steps. The
// executor threads ONE correlation id (the suite's tr-<hex>; an MCP step sends it as the prefix of
// its own per-call _meta.request_id) across every step, records PER-STEP status, and on a mid-chain
// break emits a partial-failure verdict that names the failing step and never reports
// a later step as green (VR-K1..K5).
package chain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// stepOutcome maps a per-step verdict to a request-response outcome for the per-request
// distribution panel (r3): passed→success, error(StatusError)→error (no response, e.g. a UI
// harness failure), anything else (failed)→failed (a negative response). Skipped steps fire no
// request and are never passed here.
func stepOutcome(status string) string {
	switch status {
	case "passed", report.StepRanAfterFailureOK:
		// VR12-CH1 rule 4: a step that ran after the break still FIRED a request, and the request
		// got a positive response. The panel counts REQUESTS, not verdicts — conflating them here
		// would make the request distribution lie about what the SUT actually answered.
		return report.OutcomeSuccess
	case report.StatusError:
		return report.OutcomeError
	default:
		return report.OutcomeFailed
	}
}

// Step is one step of a chained scenario. Run does the step's work keyed on the
// shared correlation id and returns its per-step result.
//
// vars is the accumulated CAPTURE STORE (capture.go): everything earlier steps saved, so this step
// can late-bind ${saved.<var>} in its args. A step publishes into it by returning StepResult.
// Captured. Before this existed, all args were resolved once up front and a chain could never use
// a server-generated id — see capture.go for what that cost.
type Step struct {
	Name string
	Run  func(cid string, vars map[string]string) report.StepResult
	// Needs reports whether this step CAN be given its inputs from the capture store, WITHOUT
	// calling the SUT (VR12-CH1 rule 3). A non-nil error means an earlier step never produced
	// something this one references, so the step is `not-measured` and is never fired — a step
	// that cannot be given its inputs was not tested, and must not be reported as if it were.
	//
	// nil = nothing to check (a step with no `${saved.…}` reference, or a step type that has no
	// inputs of this kind).
	Needs func(vars map[string]string) error
	// Always marks a CLEANUP step (AC-D20 `"always": true`). It fires even after VR12-CH2 has
	// stopped the chain: a resource an earlier step created must not be left on a shared SUT because
	// a later step's connection dropped. It is recorded as ran-after-failure, never as the named cause.
	Always bool
	// RequiresEarlierPass marks a step whose verdict is only meaningful when the SUT demonstrably
	// answered earlier in the SAME chain (, the `unreachable` claim): it is judged only
	// if an earlier step PASSED, else it is `not-measured` and never fired — a dead cluster must not
	// pass every negative check.
	RequiresEarlierPass bool
}

// Run executes the steps IN ORDER, threading ONE correlation id across all of them.
//
// ⛔ VR12-CH1 (V30-001) — IT NO LONGER STOPS AT THE FIRST FAILURE.
//
// It used to `return` from inside the step loop on the first non-passing step, synthesising every
// remaining step as `skipped`, "not run (a prior step failed)". The rule was POSITIONAL, not
// CAUSAL — a step ran only if every earlier step PASSED, which is stricter than a chain needs —
// and the steps it discarded are usually the ones that UNDO what the test created, which usually
// need nothing from the step that failed. RACE-003's `delete-namespace` names its target
// `race-s3-${cid}` from the correlation id, known before the chain starts.
//
// THE LIVE EVIDENCE: the same pack, six runs. The four completed runs left nothing; the TWO runs
// whose chain broke each left a `race-s3-…` namespace with doc_count 2 — and only those. The
// confirming negative control after the scenario was fixed: RACE-003 passes, `delete-namespace`
// runs, an authenticated read afterwards shows zero `race-*` namespaces. Same code, same server,
// opposite outcome: the mechanism is the break, not the scenario.
//
// THE SEVEN RULES:
//
//  1. any non-passing step still fails the whole scenario (unchanged);
//  2. on a step failure the executor CONTINUES to the next step;
//  3. a step whose `${saved.<var>}` was never captured is `not-measured` and is NEVER FIRED —
//     a step that cannot be given its inputs was not tested and must not be reported as if it were;
//  4. ⛔ no step that runs after the first failure may be recorded `passed` — it is
//     `ran-after-failure: ok` or `ran-after-failure: failed` (see report.StepRanAfterFailureOK);
//  5. the first step the SUT would not answer stops the scenario dead (VR12-CH2, below);
//  6. the report names the FIRST failing step as the cause; later failures are subordinate ECHOES;
//  7. a run that ends with something possibly still on the SUT says so in ONE line (res.Residue).
//
// ⛔ WHAT MUST NOT CHANGE: the capture merge stays BEFORE the outcome is judged — a cleanup step
// needs the ids captured by the step that failed, and moving it after would silently gut rule 3.
// A chain that PASSES is byte-identical in the report to before this change.
//
// ⛔ AND WHAT IS DELIBERATELY NOT DONE: the executor does NOT infer which steps are "independent"
// from the `${saved.…}` graph. Measured: RACE-002 declares NONE at all and RACE-001 exactly one —
// the real coupling is through SUT STATE, which no scenario declares. An inferred graph would look
// authoritative and be wrong, which is the same class of defect as the row itself.
// budget is VR12-TO-RUN's optional scenario-level deadline (the declared `## TIMEOUT`, timeoutCeiling
// caps a chain at 120s — sections.go). It is variadic ONLY so every existing call site (production
// and the ~35 in this package's own tests) keeps compiling unchanged; pass at most one value.
// Omitted or <= 0 means "no deadline", today's behaviour byte-for-byte.
//
// This bounds the WALL CLOCK across steps — once it elapses, no FURTHER step is fired (a step
// already in flight when the deadline passes may finish inside stepGrace; past that it is
// abandoned, see below; each step type also bounds its own single call: chain/http.go's
// httpClientTimeout, an mcp step's Client.Timeout, an AMQP step's own dial/consume timeout). It
// does not shorten any individual step's own budget, so a chain within its declared TIMEOUT is
// unaffected.
//
// ⛔ — A STEP IN FLIGHT IS CAPPED TOO. The paragraph above used to end "a step already in
// flight when the deadline passes still finishes", and that was the defect: the deadline was read
// only BETWEEN steps, so one driver call that never returned (an amqp publish into a broker holding
// a memory alarm) held a 30s chain for 7 min 50 s and every other run of the instance behind it.
// Now, with a budget, each step call runs under the time the chain has LEFT plus stepGrace; a call
// still going then is abandoned (see callStep), its step fails by name, the chain ends, and Run returns.
func Run(cid string, steps []Step, budget ...time.Duration) report.ScenarioResult {
	res := report.ScenarioResult{CorrelationID: cid, Status: "passed"}
	vars := map[string]string{} // the capture store, threaded forward (capture.go)
	firstFailure := ""          // rule 6: the named cause; "" until something fails
	echoes := 0                 // rule 6: later non-passes, subordinate to it
	stopReason := ""            // VR12-CH2: set by the first step the SUT would not answer
	var deadline time.Time
	if len(budget) > 0 && budget[0] > 0 {
		deadline = time.Now().Add(budget[0])
	}
	earlierPassed := false // some step so far PASSED (the gate of an `unreachable` claim)
	timedOut := false      // VR12-TO-RUN: the deadline above elapsed before every step had run
	abandoned := ""        // the step whose call never returned; the chain ended there

	for _, st := range steps {
		// ── A STEP'S CALL WAS ABANDONED. The chain ends. Nothing more is fired, cleanup steps
		// included: the abandoned call's connection is in an unknown state, and a cleanup step aimed
		// at the same SUT would only wait again. residueLine says what may remain.
		if abandoned != "" {
			res.Steps = append(res.Steps, report.StepResult{
				Name: st.Name, Status: report.StepNotMeasured, CorrelationID: cid,
				Observed: fmt.Sprintf("step %q did not return and was abandoned, so the chain ended there; this step was not run", abandoned),
			})
			continue
		}
		if !deadline.IsZero() && !timedOut && stopReason == "" && !time.Now().Before(deadline) {
			timedOut = true
		}
		// ── VR12-TO-RUN — THE CHAIN'S OWN DEADLINE PASSED. Everything from here is not-measured,
		// and NOT FIRED — the SAME shape as VR12-CH2 below (a cleanup step still fires), but the
		// scenario's own verdict is `failed`, never `error`: this is the chain not answering inside
		// the budget IT declared, not "nothing about this run is evidence" (that word stays reserved
		// for a dead/refusing SUT, VR12-CH2's own case, a couple of branches down).
		if timedOut && st.Always {
			atMs := time.Now().UnixMilli()
			sr, gone := callStep(st, cid, vars, deadline)
			sr.Name = st.Name
			sr.CorrelationID = cid
			sr.RanAfterFailure = true
			if sr.Status == "passed" {
				sr.Status = report.StepRanAfterFailureOK
			} else {
				sr.Status = report.StepRanAfterFailureFailed
			}
			if gone {
				abandoned = st.Name
			}
			res.Steps = append(res.Steps, sr)
			res.AddRequest(atMs, stepOutcome(sr.Status))
			continue
		}
		if timedOut {
			res.Steps = append(res.Steps, report.StepResult{
				Name: st.Name, Status: report.StepNotMeasured, CorrelationID: cid,
				Observed: fmt.Sprintf("chain exceeded its declared ## TIMEOUT of %s before this step could run", budget[0]),
			})
			if res.Status != "failed" {
				res.Status = "failed"
				res.Failure = &report.Failure{Observed: fmt.Sprintf(
					"chain exceeded its declared ## TIMEOUT of %s (stopped before step %q)", budget[0], st.Name)}
			}
			continue
		}
		// ── VR12-CH2 — WE STOPPED. Everything from here is not-measured, and NOT FIRED. ────────
		if stopReason != "" && st.Always {
			// AC-D20: the cleanup step still fires — trying it against a SUT that dropped one
			// connection costs one request; skipping it leaves what the chain created behind.
			atMs := time.Now().UnixMilli()
			sr, gone := callStep(st, cid, vars, deadline)
			sr.Name = st.Name
			sr.CorrelationID = cid
			sr.RanAfterFailure = true
			if sr.Status == "passed" {
				sr.Status = report.StepRanAfterFailureOK
			} else {
				sr.Status = report.StepRanAfterFailureFailed
			}
			if gone {
				abandoned = st.Name
			}
			res.Steps = append(res.Steps, sr)
			res.AddRequest(atMs, stepOutcome(sr.Status))
			continue
		}
		if stopReason != "" {
			res.Steps = append(res.Steps, report.StepResult{
				Name: st.Name, Status: report.StepNotMeasured, CorrelationID: cid,
				Observed: stopReason,
			})
			continue
		}

		// ── — THE FALSE-GREEN GUARD of an `unreachable` claim. Nothing earlier passed,
		// so a transport failure here would prove nothing (the whole target may be down).
		if st.RequiresEarlierPass && !earlierPassed {
			reason := "no earlier step of this chain passed, so a transport failure here would prove nothing " +
				"(a dead target would pass every negative check); this step was not run"
			res.Steps = append(res.Steps, report.StepResult{
				Name: st.Name, Status: report.StepNotMeasured, CorrelationID: cid, Observed: reason,
			})
			if res.Status == "passed" {
				res.Status = "failed"
			}
			if res.Failure == nil {
				res.Failure = &report.Failure{Observed: "step '" + st.Name + "' not measured: " + reason}
			}
			if firstFailure != "" {
				echoes++
			}
			continue
		}

		// ── RULE 3 — its input was never produced, so it is NOT FIRED. ────────────────────────
		//
		// The check has to happen HERE rather than inside Run: bindSaved already produces exactly
		// this error text, but inside the step it arrives as a step FAILURE, after the decision to
		// call has been made. A step that cannot be given its inputs was not tested.
		if st.Needs != nil {
			if err := st.Needs(vars); err != nil {
				res.Steps = append(res.Steps, report.StepResult{
					Name: st.Name, Status: report.StepNotMeasured, CorrelationID: cid,
					Observed: err.Error(),
				})
				// Rule 1: a not-measured step still means the scenario did not pass. It is not a
				// SUT failure though — nothing was measured — so it never becomes the named cause.
				if res.Status == "passed" {
					res.Status = "failed"
				}
				if firstFailure != "" {
					echoes++
				}
				continue
			}
		}

		atMs := time.Now().UnixMilli() // the step's real fire-time (it runs natively, in order)
		sr, gone := callStep(st, cid, vars, deadline)
		if gone {
			abandoned = st.Name
		}
		sr.Name = st.Name
		sr.CorrelationID = cid
		// publish this step's captures for the steps after it. Merged BEFORE anything below reads
		// the outcome, so a step that saved successfully and then failed its assertion still hands
		// on what it got — which is what makes a later cleanup step able to run at all.
		for k, v := range sr.Captured {
			vars[k] = v
		}

		// ── VR12-CH2 — the SUT would not answer: unreachable, or refusing us because we are
		// going too fast. STOP. Three reasons, in order of weight:
		//
		//  1. nothing after it would be measured either — continuing fires more requests into a
		//     wall and records more not-measured steps. No information is gained.
		//  2. ⚠ THE CORRELATION-ID COLLISION. A rate-limited chain is retried as a WHOLE from step
		//     1 with the SAME correlation id, and scenario object names are built from that id. If
		//     the first attempt CREATED a library and was then throttled, the retry re-runs
		//     `create-library` with a name that already exists and fails on a collision that has
		//     nothing to do with the SUT under test.
		//  3. every further call is another effect fired into a SUT that is already refusing.
		if sr.RateLimited || isUnanswered(sr) {
			stopReason = notMeasuredReason(sr)
			res.Steps = append(res.Steps, report.StepResult{
				Name: st.Name, Status: report.StepNotMeasured, CorrelationID: cid,
				Observed: stopReason, MCPEnvelope: sr.MCPEnvelope,
			})
			// ⚠ THIS step DID fire a request — it asked and got no usable answer. The request
			// panel counts REQUESTS, not verdicts, so it is recorded (as an error outcome: the
			// test could not get a response). Only the steps AFTER it fire nothing.
			res.AddRequest(atMs, report.OutcomeError)
			// NOT MEASURED, not failed: "server is unreachable" is not a failure, it means we
			// cannot run tests. The rate-limit marker rides on the SCENARIO because the run loop
			// retries the whole chain, not a step in isolation (VR10-R1 / SA §0.14 R1-d).
			//
			// ⛔ BUT "NOT MEASURED" NEVER OVERRIDES "MEASURED AND WRONG". If an earlier step had
			// already failed, this run found a real defect, and re-labelling the scenario `error`
			// would bury it behind an infrastructure excuse — the same mis-attribution this round
			// exists to remove, pointed the other way.
			if firstFailure == "" {
				res.Status = report.StatusError
			}
			if sr.RateLimited {
				res.RateLimited = true
				res.RetryAfterMs = sr.RetryAfterMs
			}
			continue
		}

		// ── RULE 4 — nothing after the first failure may be recorded `passed`. ────────────────
		if firstFailure != "" {
			sr.RanAfterFailure = true
			if sr.Status == "passed" {
				sr.Status = report.StepRanAfterFailureOK
			} else {
				sr.Status = report.StepRanAfterFailureFailed
				echoes++
			}
		}

		if sr.Status == "passed" && !st.RequiresEarlierPass {
			// only a POSITIVE step opens the guard: a passed `unreachable` step proves nothing about liveness
			earlierPassed = true
		}
		res.Steps = append(res.Steps, sr)
		// Test-requests panel (r3): one request per EXECUTED step, recorded by its outcome. A
		// not-measured step fired none and is never passed here.
		res.AddRequest(atMs, stepOutcome(sr.Status))

		if sr.Status != "passed" && firstFailure == "" {
			// Rule 6: THE named cause. Everything after it is an echo.
			firstFailure = sr.Name
			res.Status = "failed"
			// ...unless nothing ever got through. A chain that broke on its FIRST step because the
			// SUT was unreachable is not a failed chain, it is an unrun one.
			if res.NeverReachedSUT() {
				res.Status = report.StatusError
			}
			res.Failure = &report.Failure{Observed: "step '" + sr.Name + "' failed: " + sr.Observed}
		}
	}

	// Rule 6, said out loud. Four red steps and one bug is an ACCEPTED COST of continuing; what
	// makes it payable is that the echoes are LABELLED as echoes, in the report, rather than left
	// for the reader to work out.
	if res.Failure != nil && echoes > 0 {
		res.Failure.Observed += " — " + strconv.Itoa(echoes) + " later step(s) also did not pass; " +
			"they ran after the break and are echoes of it, not independent findings"
	}
	res.Residue = residueLine(res)
	return res
}

// stepGrace is how long past the chain's deadline a step's call may still run before callStep
// abandons it. It is a var only so a test can shorten it.
var stepGrace = 5 * time.Second

// callStep runs st.Run and waits for it only as long as the chain has left (deadline) plus stepGrace.
// A zero deadline means the chain declared no budget: the call runs inline, unbounded, exactly as
// before. abandoned reports that the call did not return in time: sr is then a failed step naming
// the cap, and the goroutine still running st.Run is left behind.
//
// What the abandoned call can no longer touch: it got its OWN COPY of the capture store, so a late
// write cannot race the chain that went on; and its result travels through a buffered channel
// nobody reads, so it can never reach the finished run's results. A panic in st.Run is carried back
// and re-raised on the caller's goroutine, where it always surfaced.
func callStep(st Step, cid string, vars map[string]string, deadline time.Time) (sr report.StepResult, abandoned bool) {
	if deadline.IsZero() {
		return st.Run(cid, vars), false
	}
	left := max(time.Until(deadline), 0)
	limit := left + stepGrace
	own := make(map[string]string, len(vars))
	for k, v := range vars {
		own[k] = v
	}
	type done struct {
		sr       report.StepResult
		panicked any
		did      bool
	}
	ch := make(chan done, 1)
	go func() {
		d := done{}
		defer func() {
			if r := recover(); r != nil {
				d.panicked = r
			}
			ch <- d
		}()
		d.sr = st.Run(cid, own)
		d.did = true
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case d := <-ch:
		if !d.did {
			panic(d.panicked)
		}
		return d.sr, false
	case <-timer.C:
		return report.StepResult{Status: "failed", Observed: fmt.Sprintf(
			"the step did not return within %s (the chain's remaining declared ## TIMEOUT of %s plus a %s grace): "+
				"the call was abandoned and its result discarded, and the chain ended here",
			limit.Round(time.Millisecond), left.Round(time.Millisecond), stepGrace)}, true
	}
}

// isUnanswered reports whether a step got NO answer from the SUT, as opposed to a wrong one. The
// step statuses that mean this are set by MCPStep from the judge's plane.
func isUnanswered(sr report.StepResult) bool {
	return sr.Status == report.StatusError && !sr.RateLimited
}

// notMeasuredReason renders VR12-CH2's stop reason. It describes REALITY only — never the
// scenario's expected value — because a not-measured step is reported to both hats.
func notMeasuredReason(sr report.StepResult) string {
	if sr.RateLimited {
		wait := ""
		if sr.RetryAfterMs > 0 {
			wait = " (retry-after " + strconv.Itoa(sr.RetryAfterMs/1000) + "s)"
		}
		return "SUT rate-limited" + wait + " — the chain stopped here; this step and every later one were not measured"
	}
	return "SUT unreachable — the chain stopped here; this step and every later one were not measured"
}

// residueLine is VR12-CH1 rule 7: ONE line when the run may have left something behind.
//
// ⛔ It says MAY remain. The runner knows which steps did not complete; it does not know what the
// SUT actually holds, and a confident sentence it cannot support would be the same defect as the
// row it fixes.
func residueLine(res report.ScenarioResult) string {
	if res.Status == "passed" {
		return ""
	}
	var incomplete []string
	for _, st := range res.Steps {
		switch st.Status {
		case report.StepNotMeasured, report.StepRanAfterFailureFailed:
			incomplete = append(incomplete, st.Name)
		}
	}
	if len(incomplete) == 0 {
		return "" // every step ran to a real verdict; nothing was left un-attempted
	}
	return "failed before its cleanup — step(s) " + strings.Join(incomplete, ", ") +
		" did not complete, so anything this scenario created may remain on the SUT"
}

// MCPCleanup marks an mcp step as the chain's CLEANUP action (AC-D20 `"always": true`) — the mcp
// counterpart of HTTPStep's always argument ( until this existed, the field parsed on
// an mcp step and was dropped). It fires after a VR12-CH2 stop, and rule 3's gate is lifted so it is
// attempted even when a ${saved.…} it binds was never captured; the step's Run then fails cleanly,
// naming the variable, rather than sending a literal placeholder to the SUT.
func MCPCleanup(s Step) Step {
	s.Always = true
	s.Needs = nil
	return s
}

// MCPStep builds the native-MCP step (judged on the two planes in Go; reality-only).
// Its request id (_meta.request_id) is unique per call and PREFIXED by the correlation id, so
// downstream DB/UI steps and log lookups still key on the correlation id by prefix (VR-K2/K6).
// argsJSON is the step's args as RAW JSON, not a decoded value: any ${saved.<var>} inside it is
// bound at RUN time from the accumulated capture store, which is the whole point — the value is
// not known when the chain is built. save maps a capture variable to a path into THIS step's
// response (capture.go); nil means this step saves nothing.
func MCPStep(name string, cl *mcp.Client, tool, argsJSON string, expect mcp.Expect, save map[string]scenario.SaveSpec) Step {
	return Step{Name: name,
		// VR12-CH1 rule 3: can this step be given its inputs AT ALL? Asked before the call, using
		// the SAME bindSaved the args and the assertion use, so the answer cannot drift from what
		// the step would actually do. An unresolved ${saved.…} means the step that produces it did
		// not pass — this step was never tested, and firing it would only add an echo.
		Needs: func(vars map[string]string) error {
			if _, err := bindSaved(argsJSON, vars); err != nil {
				return err
			}
			return needsBodyAsserts(expect.Body, vars)
		},
		Run: func(cid string, vars map[string]string) report.StepResult {
			bound, err := bindSaved(argsJSON, vars)
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
			// ONE request id per call, prefixed by the chain's correlation id (mcp.PerCallRequestID). It
			// used to be `cid` itself on every step, and a SUT enforcing request-id uniqueness refused
			// every step after the first as "request_id reused".
			res := cl.Call(mcp.CallInput{Tool: tool, Args: args, RequestID: mcp.PerCallRequestID(cid)})
			// VR10-R1 (owner D12): the chain path converges on the SAME judge, so it gets the same fix —
			// the SUT's declared throttle rides on the client, which is the endpoint this step calls.
			v := mcp.JudgeWithRateLimit(res, want, cl.RateLimit)
			st := report.StepResult{Status: "failed", Observed: v.Observed}
			if !v.Pass && v.Plane == mcp.PlaneBody {
				// a content-claim miss. The text points at the step's failed_claims —
				// the per-claim record, built here from what THIS call's answer showed — instead of a
				// failure record that does not exist. `expect` is the claims as written, `want` the
				// bound copy the judge compared.
				st.Observed = chainBodyNote(v.Observed)
				st.FailedClaims = failedBodyClaims(mcp.BodyClaimText(res, want), expect.Body, want.Body, vars, nil)
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
				// a numeric threshold bound from the store is shown AS WRITTEN
				// (${saved.n}), never as the measured value. Every other claim keeps its long-pinned
				// BOUND rendering (TestMCPStep_SavedValueBindsIntoTheBodyAssertion).
				st.AssertionsEnforced = enforcedAssertions(keepSavedThresholdsAsWritten(expect, want))
				st.AssertionsEnforcedCount = len(st.AssertionsEnforced)
			}
			if !v.Pass {
				// PROB-1: carry the SUT's own envelope on a non-passing step. `Observed` alone
				// ("responder returned result.isError:true") is the same string for every tool-plane
				// failure, so the actual error text — the only thing that separates TEST_BUG from
				// CODE_BUG — was unreachable in-env for every chained scenario. Failures only: a
				// passing step's envelope is transcript, and report.json is already ~120KB for 45
				// scenarios.
				st.MCPEnvelope = res.Raw
			}
			// CAPTURE: extract what this step declared to save. Done AFTER judging (so the status is
			// known) but reported as a failure of THIS step when a path does not resolve — the break
			// belongs where the authoring mistake is, not two steps later when something else receives
			// an empty id. Only attempted on a passing step: a failed response has no payload worth
			// threading, and reporting a save error over a real assertion failure would bury it.
			if v.Pass && len(save) > 0 {
				captured := make(map[string]string, len(save))
				for name, sp := range save {
					val, fail := saveFromMCP(res.Raw, name, sp)
					if fail != "" {
						return report.StepResult{Status: "failed", MCPEnvelope: res.Raw, Observed: fail}
					}
					captured[name] = val
				}
				st.Captured = captured
			}
			switch {
			case v.Pass:
				st.Status = "passed"
			case v.Plane == "unreachable" || v.Plane == "transport":
				// r3: no response from the SUT (endpoint unreachable / transport-handshake failure)
				// is an EXECUTION error, NOT a negative response — mirror the standalone MCP path
				// (mcp_scenario.go mcpRequestOutcome) so stepOutcome maps it to OutcomeError, keeping
				// the 3-way request classification consistent across the standalone and chained paths.
				st.Status = report.StatusError
			}
			return st
		}}
}

// bindExpect returns a per-execution COPY of expect with ${saved.<var>} bound into its content
// assertions by bindSaved — the args' grammar, the args' error (it names every unbound variable).
// The input is never touched (VR10-S2-11: a re-run never sees the previous run's id).
func bindExpect(expect mcp.Expect, vars map[string]string) (mcp.Expect, error) {
	want := expect
	// VR12-E8: bind EVERY assertion, into a fresh slice. ⛔ The copy is load-bearing: `want :=
	// expect` copies the slice HEADER, so writing through it would mutate the caller's assertions
	// and a re-run would see the previous run's bound ids — exactly what VR10-S2-11 forbids.
	// the shared binder (claimbind.go) — also refuses a numeric comparison whose
	// saved threshold is not a number, by name.
	body, err := bindBodyAsserts(expect.Body, vars)
	if err != nil {
		return mcp.Expect{}, err
	}
	want.Body = body
	return want, nil
}

// enforcedAssertions renders the content assertions the judge evaluated, with their BOUND values —
// the text behind report.StepResult.AssertionsEnforced (test hat; the product hat keeps the count).
// VR12-E8: renders EVERY assertion the judge evaluated, not the first of each kind, and names the
// FIELD when one was declared. Before this, a step declaring two content assertions recorded one —
// so `assertions_enforced` under-reported exactly as the judging did, and the report agreed with
// the bug instead of exposing it.
func enforcedAssertions(want mcp.Expect) []string {
	var out []string
	for _, a := range want.Body {
		where := "content"
		if a.Scoped() {
			where = "field " + a.Field
		}
		switch a.Op {
		case mcp.BodyMatchesOp:
			out = append(out, where+" matches /"+a.Value+"/")
		case mcp.BodyExistsOp:
			out = append(out, where+" exists")
		case mcp.BodyEqualsOp:
			out = append(out, where+" equals "+strconv.Quote(a.Value))
		// item 25 — numeric comparisons render with their symbol, the same "text carries the
		// scenario's expected value, so it is test-hat only" contract every other op already has
		// (redactExpected nils this whole slice for the product hat; VR12-E14).
		case mcp.BodyGTOp:
			out = append(out, where+" > "+a.Value)
		case mcp.BodyGTEOp:
			out = append(out, where+" >= "+a.Value)
		case mcp.BodyLTOp:
			out = append(out, where+" < "+a.Value)
		case mcp.BodyLTEOp:
			out = append(out, where+" <= "+a.Value)
		default:
			out = append(out, where+" contains "+strconv.Quote(a.Value))
		}
	}
	return out
}

// EnforcedAssertions is enforcedAssertions for the other engines: the single-call mcp scenario and
// the http engine render their enforced body checks in the same words a chain step uses.
func EnforcedAssertions(want mcp.Expect) []string { return enforcedAssertions(want) }

package argus

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/avroschema"
	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/ui"
)

// savedRefRe matches a ${saved.<var>} reference in an http step's url (AC-D20) — the SAME grammar
// chain.savedRefRe binds at run time (V31-003: it compiles scenario.SavedRefPattern, never its own
// copy), so the preflight "is this url still unresolved" check can tell a legitimate, not-yet-bound
// saved ref apart from a genuinely missing env var.
var savedRefRe = regexp.MustCompile(scenario.SavedRefPattern)

// anyPlaceholderRe finds every `${...}` still standing in a resolved text, whatever is inside —
// resolveVars' own pattern only matches a well-formed NAME, and a malformed one must be reported too.
var anyPlaceholderRe = regexp.MustCompile(`\$\{[^}]*\}`)

// unresolvedPlaceholders names every `${...}` left in s once the legitimate, run-time-bound
// ${saved.<var>} references are set aside — each once, in first-seen order.
func unresolvedPlaceholders(s string) []string {
	var out []string
	seenName := map[string]bool{}
	for _, m := range anyPlaceholderRe.FindAllString(savedRefRe.ReplaceAllString(s, ""), -1) {
		if !seenName[m] {
			seenName[m] = true
			out = append(out, m)
		}
	}
	return out
}

// resolveChainHTTPURL resolves a chain `http` step's url. A url
// that STARTS with ${INGESTION_URL} gets the configured HTTP target's host and port in its place — the
// same base a plain HTTP check of the scenario would call (config.HTTPTarget.Origin; the path of
// base_url is NOT prepended). ${INGESTION_URL} is a leading marker, not an
// environment variable, so it wins over any environment variable of that name, as it does for a plain
// check (config.PathFromURL). The rest resolves as every chain field does: the correlation id, then the
// environment. It returns the url, or the reason the step cannot run — naming every variable that is
// still unresolved. ${saved.<var>} is left for run time.
func resolveChainHTTPURL(c *config.Config, st scenario.ChainStep, corr string) (string, string) {
	u := strings.TrimSpace(st.URL)
	if strings.HasPrefix(u, scenario.IngestionURLToken) {
		if strings.TrimSpace(st.Target) != "" {
			return "", "sets \"target\", which an http step does not read: ${INGESTION_URL} is the plain targets.http base and a named http target cannot be selected from a chain — write the full url for that host"
		}
		var origin string
		var ok bool
		if c != nil {
			origin, ok = c.Targets.HTTP.Origin()
		}
		if !ok {
			return "", "uses ${INGESTION_URL} but argus-config.yaml declares no targets.http.base_url to stand in for it — declare it, or write the full url"
		}
		u = origin + strings.TrimPrefix(u, scenario.IngestionURLToken)
	}
	// The marker is never read from the environment, here or in the payload (parseChainSpec): a second
	// ${INGESTION_URL} that is not the leading token stays standing and is refused below, whatever the
	// environment holds.
	u, _ = substituteVars(u, corr, false, true)
	if left := unresolvedPlaceholders(u); len(left) > 0 {
		return "", "url has unresolved variable(s) " + strings.Join(left, ", ") + " — set each in the executor's environment (check_env in argus-config.yaml makes onboarding DELIVER a name to the executor; any name present there is substituted)"
	}
	return u, ""
}

// dialAMQPBroker opens an amqp step's broker connection (AC-D18b). A package var ONLY so tests can
// put a fake broker behind the whole chain path; production never reassigns it.
var dialAMQPBroker chain.BrokerDialer = chain.DialAMQP

// mayRefire reports whether the run loop may re-fire this scenario after the SUT rate-limited it
// (pacer.handle, argus.go). ⛔ AC-D18b: NOT a chain carrying an amqp step. Re-firing re-runs the
// chain from step 1, so its publish would reach a real agent's inbox a second time. The one-publish
// rule holds for the WHOLE run, not just inside chain.Run. A payload that does not parse is left to
// today's behaviour: the chain stopped at preflight and published nothing.
func mayRefire(s *scenario.Scenario) bool {
	if !contains(s.Tags, ChainTag) {
		return true
	}
	steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
	if err != nil {
		return true
	}
	for _, st := range steps {
		if st.Type == "amqp" {
			return false
		}
	}
	return true
}

// ChainTag marks a multi-step chained scenario (D3.6 / UC-32/63): native-MCP and UI
// steps are SEQUENCED in Go (no single JMeter template can span them), threading ONE
// correlation id (the suite tr-<hex>; each MCP step sends it as the prefix of its own per-call
// _meta.request_id, mcp.PerCallRequestID) across every step.
// The chain executor records per-step status and, on a mid-chain break, emits a
// partial-failure verdict that names the failing step and never reports a later step
// green (VR-K1..K5). It rides the SAME RunAll pipeline, tag-selectable by `chain`.
const ChainTag = scenario.ChainTag

// chainStepSpec is scenario.ChainStep. ⛔ ALIASED, NOT COPIED (VR12-E10): tier 2 has to read the
// steps to enforce the orphan / empty-step / one-home guards, and internal/scenario cannot import
// internal/argus. Two decoders of one wire format is how a validator ends up policing a shape the
// runner does not run.
type chainStepSpec = scenario.ChainStep

// parseChainSpec reads the chain scenario's TRIGGER payload {steps:[...]} with ${cid}/${correlation_id}/${VAR}
// resolved.
//
// ⚠ V31-003 made it re-read a step's deprecated `expect` from the RAW text, so an environment value could not be
// filled into a CHECK (a check is printed in the report). V31-002 then REMOVED that key entirely, so the carve-out
// is gone with it: everything left in this payload is a REQUEST, and a request may carry an environment value.
//
// /-15 (amended): the substitution is done for the JSON TEXT (substituteVars asJSON), so a
// value is data: it cannot end its string, add a key or break the document. ${INGESTION_URL} is left
// standing for resolveChainHTTPURL, so the marker wins over an environment variable of that name. If the
// text still cannot be parsed after a value was filled in, the error says so WITHOUT the parser's own
// words, which quote a character of the offending text (`invalid character 's' after ...`) and so can be a
// character of a secret.
func parseChainSpec(payload, corr string) ([]chainStepSpec, error) {
	resolved, substituted := substituteVars(payload, corr, true, true)
	steps, err := scenario.ParseChainSteps(resolved)
	if err != nil {
		if !substituted {
			return nil, err // no environment value is in this text: the parser's message is safe and more helpful
		}
		where := ""
		var se *json.SyntaxError
		if errors.As(err, &se) {
			where = fmt.Sprintf(" (byte offset %d)", se.Offset)
		}
		return nil, fmt.Errorf("the chain spec is not valid JSON once the environment values are filled in%s: a value sits where JSON does not allow it, for example text in a number position — the value is not shown", where)
	}
	return steps, nil
}

// uiStep builds a chain Step that runs a vendored Playwright spec keyed on the shared
// correlation id (passed as ARGUS_CORRELATION_ID via uiRun). exit 0 -> passed;
// exit!=0 with an artifact -> failed; exit!=0 without -> error. The chain executor stops
// the chain on any non-pass.
func uiStep(name, spec, appURL, vendorDir string) chain.Step {
	return chain.Step{Name: name, Run: func(cid string, _ map[string]string) report.StepResult {
		if appURL == "" {
			appURL = os.Getenv("APP_URL")
		}
		exitCode, hasResults, stderr := uiRun(vendorDir, []string{spec}, appURL, cid, nil, "")
		status, observed := ui.Classify(exitCode, hasResults, stderr)
		return report.StepResult{Status: status, Observed: observed}
	}}
}

// runChainScenario builds the ordered steps from the scenario and runs them through the
// chain executor, returning a ScenarioResult with per-step Steps[] (DEC-07) and a
// reality-only partial-failure verdict on a mid-chain break (UC-63).
//
// A THIN WRAPPER around runChainScenarioWithMoneyWrites (nil allow, nil ledger) so every existing
// caller — every test that builds a chain scenario directly, without a whole RunAll around it —
// keeps compiling unchanged: a nil allowlist behaves exactly as before money_writes existed.
// runOneScenario (the only production caller) calls the money_writes-aware form directly, because
// only it can supply the ONE ledger a run's scenarios must share (item 3, "across all scenarios").
func runChainScenario(c *config.Config, s *scenario.Scenario, corr, vendorDir string) report.ScenarioResult {
	return runChainScenarioWithMoneyWrites(c, s, corr, vendorDir, nil, nil)
}

func runChainScenarioWithMoneyWrites(c *config.Config, s *scenario.Scenario, corr, vendorDir string,
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger) report.ScenarioResult {
	return runChainScenarioWithOutputs(c, s, corr, vendorDir, moneyAllow, moneyLedger, nil)
}

// runChainScenarioWithOutputs is runChainScenarioWithMoneyWrites plus the compare-mode recorder (oc nil
// = records nothing, byte-identical to before ARGUS-CMP-3).
func runChainScenarioWithOutputs(c *config.Config, s *scenario.Scenario, corr, vendorDir string,
	moneyAllow scenario.MoneyWriteAllowlist, moneyLedger *scenario.MoneySpendLedger, oc *outputCtx) report.ScenarioResult {
	// VR10-S3: a chain selects its MCP target PER STEP; a Metadata **Target** is refused, not ignored.
	if _, terr := scenario.TargetKind(s); terr != nil {
		return targetRefused(s, corr, terr)
	}
	// VR12-E10: the claims now live in `## EXPECT` as `- step <name>: <assertion>`. Parse them ONCE
	// here; each step below asks for its own. A claim naming no step, or a step with no claim, is
	// refused at authoring time — this path only has to READ.
	claims, claimErrs := scenario.ParseChainClaims(s.RunnableExpect())
	if len(claimErrs) > 0 {
		return report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: "failed",
			Failure: &report.Failure{Observed: "chain claim parse error: " + claimErrs[0].Error()}}
	}
	// ⛔ SECOND DOOR (matches the P3 #24c pattern below: declaredQueues/declaredExchanges are also
	// re-checked here, not trusted from Validate alone) — a claim's text is copied into the report
	// (assertions_enforced, Failure.Expected), so a `${…}` that is not the run's own id or, in a
	// chain, `${saved.<var>}` must never be filled in: doing so would publish an environment
	// variable's value. scenario.Validate (checkPlaceholderErrors) already refuses this at AUTHORING
	// time, for every scenario — but a file can reach a run without ever passing through the
	// validator (same reasoning as the amqp write guard, P3 #24c), so it is refused again here, BY
	// NAME. scenario.UnfilledPlaceholders only ever extracts the placeholder TOKEN (a regex over the
	// literal `${…}` text) — os.Getenv is never called on it, so the variable's value never exists in
	// any variable this function touches, let alone a report field.
	claimSteps := make([]string, 0, len(claims))
	for step := range claims {
		claimSteps = append(claimSteps, step)
	}
	sort.Strings(claimSteps) // deterministic: which refusal fires first must not depend on map order
	for _, step := range claimSteps {
		for _, b := range claims[step] {
			if ph := scenario.UnfilledPlaceholders(b, true); len(ph) > 0 {
				return preflightChainFail(s, corr, step, scenario.UnfilledPlaceholderError(ph[0]))
			}
		}
	}
	specs, err := parseChainSpec(s.Trigger.Payload, corr)
	if err != nil {
		return report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: "failed",
			Failure: &report.Failure{Observed: "chain scenario spec parse error: " + err.Error()}}
	}
	// pristineSteps mirrors specs one-for-one but comes from the UNSUBSTITUTED payload text — the
	// ONLY safe source for an amqp step's `record` (see buildAMQPRecordTemplate's doc comment: a
	// secret-looking ${VAR} must be refused BY NAME before resolveVars ever calls os.Getenv on it,
	// and resolveVars already ran, on the whole payload text, to produce `specs` above). Any parse
	// error here is unreachable in practice — the identical text just parsed successfully above —
	// so pristineSteps is simply left nil (every amqp step's record lookup then safely finds nothing
	// and reports its own "not valid JSON"/missing-record refusal instead of panicking).
	pristineSteps, _ := scenario.ParseChainSteps(s.Trigger.Payload)
	if len(specs) == 0 {
		return report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: "failed",
			Failure: &report.Failure{Observed: `chain scenario: no steps — a chain (tag:chain) TRIGGER must carry a JSON payload {"steps":[{"type":"mcp"|"ui",…},…]}; see the scenario-author chain format — preflight`}}
	}
	// P3 #24c, second door: the SAME "did this chain declare it" sets scenario.Validate computes
	// (amqpStepErrors), so a queue_delete/queue_unbind/exchange_delete that reaches a run without
	// passing through the validator is refused here too, exactly as the one-publish rule is.
	declaredQueues := map[string]bool{}
	declaredExchanges := map[string]bool{}
	for _, st := range specs {
		if st.Type != "amqp" {
			continue
		}
		switch st.Op {
		case "declare_queue":
			if st.Queue != "" {
				declaredQueues[st.Queue] = true
			}
		case "declare_exchange":
			if st.Exchange != "" {
				declaredExchanges[st.Exchange] = true
			}
		}
	}
	var steps []chain.Step
	hasHTTPStep := false
	for i, st := range specs {
		// P4, second door (scenario.Validate is the first): a `save` regex must compile and carry
		// exactly one capture group — refused BY NAME before anything runs.
		if why := scenario.SaveProblems(st.Save); len(why) > 0 {
			return preflightChainFail(s, corr, st.Name, why[0])
		}
		switch st.Type {
		case "mcp":
			mcpT := c.Targets.MCP
			serverURL := resolveVars(st.ServerURL, corr)
			if st.Target != "" {
				// VR10-S3: "target" selects targets.mcp_targets.<name> — endpoint, transport, timeout
				// and token. Refused by name when missing; never replaced by the plain slot. A
				// resolved server_url beside it is two answers to one question.
				if serverURL != "" && !strings.Contains(serverURL, "${") {
					return preflightChainFail(s, corr, st.Name, `sets both "target" and "server_url" — two answers to one question; keep "target" ("server_url" is the override for a SUT with one endpoint)`)
				}
				named, nerr := c.MCPTargetNamed(st.Target)
				if nerr != nil {
					return preflightChainFail(s, corr, st.Name, nerr.Error())
				}
				mcpT, serverURL = named, named.URL()
			} else if serverURL == "" || strings.Contains(serverURL, "${") {
				// CHANGE-1: an explicit, RESOLVED step server_url wins; otherwise fall back to
				// the argus-config targets.mcp.base_url. A leftover ${VAR} is "not provided".
				serverURL = c.MCPBaseURL()
			}
			if serverURL == "" || strings.Contains(serverURL, "${") {
				return preflightChainFail(s, corr, st.Name, "no mcp endpoint resolved — declare targets.mcp.base_url in argus-config.yaml (or set a step target / server_url)")
			}
			// pass the args as RAW JSON: any ${saved.<var>} inside is bound at RUN time from the
			// capture store, so it must survive un-decoded until then (chain/capture.go).
			argsJSON := string(st.Args)
			if strings.TrimSpace(argsJSON) == "" {
				argsJSON = "{}"
			}
			// VR12-TO-RUN (chain, per-step): the scenario's declared `## TIMEOUT` caps THIS step's own
			// call the same way runMCPScenario caps a single-call mcp scenario's callTimeout — tighter
			// only, never looser, so a SUT tuned slow-but-healthy at the target level keeps that
			// protection. Before this, a chain step's own wait was NEVER bounded by the scenario's own
			// TIMEOUT at all (only chain.Run's now-generous whole-chain backstop was) — see chain.Run's
			// budget comment (below) for why a chain of several LEGITIMATELY-slow steps must not have
			// their real, per-step waits summed against one flat ceiling.
			stepTimeout := mcpT.TimeoutOrDefault()
			if budget := s.TimeoutDuration(); budget > 0 && (stepTimeout <= 0 || budget < stepTimeout) {
				stepTimeout = budget
			}
			cl := &mcp.Client{ServerURL: serverURL, Transport: mcp.Transport(orDefault(st.Transport, mcpT.TransportOrDefault())), Token: resolveVars(mcpT.Token(), corr), Timeout: stepTimeout, RateLimit: rateLimitSpec(c),
				// VR12-T3 path 3: the scenario's headers are the default for every step.
				ExtraHeaders: resolvedHeaders(s, corr)}
			// VR10-S2 / CR-1: a step `expect` the parser cannot classify (or a `;`-joined string)
			// fails the chain at PREFLIGHT, naming the step and quoting the bullet — nothing is called.
			want, err := chainStepExpect(st, claims[st.Name])
			if err != nil {
				return preflightChainFail(s, corr, st.Name, err.Error())
			}
			// V31-003, after classification (see runMCPScenario). Covers the `## EXPECT` claims AND
			// the deprecated step `expect`; `${saved.…}` is still bound later, at run time, by
			// bindExpect — this fills only the run's own id.
			want = fillCorrelationIDInChecks(want, corr)
			step := chain.MCPStep(st.Name, cl, st.Tool, argsJSON, want, st.Save)
			if st.Always {
				step = chain.MCPCleanup(step) // AC-D20 — every cleanup step the suite ships is an mcp step
			}
			steps = append(steps, step)
		case "ui":
			spec := resolveVars(st.Spec, corr)
			if spec == "" || strings.Contains(spec, "${") {
				return preflightChainFail(s, corr, st.Name, "missing/unresolved ui spec path")
			}
			steps = append(steps, uiStep(st.Name, spec, resolveVars(st.AppURL, corr), vendorDir))
		case "http":
			method := strings.ToUpper(strings.TrimSpace(resolveVars(st.Method, corr)))
			if method == "" || strings.TrimSpace(st.URL) == "" {
				return preflightChainFail(s, corr, st.Name, "an http step needs both a method and a url")
			}
			if strings.Contains(method, "${") {
				return preflightChainFail(s, corr, st.Name, "method has unresolved variable(s) "+strings.Join(unresolvedPlaceholders(method), ", "))
			}
			// A `${saved.<var>}` in the url is legitimate and deliberately NOT resolved here — it
			// binds at RUN time (chain.HTTPStep/bindSaved), exactly as an mcp step's args do.
			// ${INGESTION_URL} stands for the configured http target (resolveChainHTTPURL); any other
			// `${NAME}` left over is an env var that was never set, and is named.
			url, why := resolveChainHTTPURL(c, st, corr)
			if why != "" {
				return preflightChainFail(s, corr, st.Name, why)
			}
			// VR12-T3 path 3, same as mcp: the scenario's own TRIGGER headers are the default for
			// every step; a step's own `headers` are layered on top and win on a name clash.
			//
			// st.Headers decodes leniently (scenario.ChainStep.Headers, shared with the amqp
			// `publish` op's custom headers, P3 #24b) so a non-string value is refused BY NAME here
			// — the second door; scenario.Validate already refuses it at authoring time.
			stepHeaders, badHeaders := scenario.HeaderValues(st.Headers)
			if len(badHeaders) > 0 {
				return preflightChainFail(s, corr, st.Name, "header "+strconv.Quote(badHeaders[0])+" must be a string value")
			}
			headers := map[string]string{}
			for k, v := range resolvedHeaders(s, corr) {
				headers[k] = v
			}
			for k, v := range stepHeaders {
				headers[k] = resolveVars(v, corr)
			}
			// #606 / the correlation id the report prints for this step is the one
			// the SUT sees, on every request the step sends (a poll re-attempt reuses this map). The
			// header is reserved (scenario/headers.go, refused on a step's own headers at validate time
			// too). The map is keyed as the author wrote it and applied with Header.Set in map order, so a
			// case variant (`x-correlation-id`) would be a second key that could win at random: drop any
			// key that names this header before setting the runner's.
			for k := range headers {
				if http.CanonicalHeaderKey(k) == "X-Correlation-Id" {
					delete(headers, k)
				}
			}
			headers["X-Correlation-Id"] = corr
			bodyTmpl := ""
			if len(st.Body) > 0 && string(st.Body) != "null" {
				// a body is JSON text: a value that is still to be filled in here is escaped like the rest
				// of the spec (parseChainSpec), so it cannot break the body either.
				bodyTmpl, _ = substituteVars(string(st.Body), corr, true, false)
			}
			wantStatus, bodyWant, err := httpStepExpect(claims[st.Name])
			if err != nil {
				return preflightChainFail(s, corr, st.Name, err.Error())
			}
			// `- step <name>: unreachable` — a step whose EXPECTED answer is a
			// transport failure. The second door for what scenario.Validate refuses (poll/always).
			if scenario.StepClaimsUnreachable(claims[st.Name]) {
				if st.Poll != nil || st.Always {
					return preflightChainFail(s, corr, st.Name, "a step that claims `unreachable` may not carry `poll` or `always` — it is judged once")
				}
				steps = append(steps, chain.UnreachableHTTPStep(st.Name, method, url, headers, bodyTmpl, moneyAllow, moneyLedger))
				continue
			}
			// Fix for the cid-in-chain-claims defect: the mcp step's claims have carried the run's
			// ${cid}/${cid8}/${correlation_id} since V31-003 (fillCorrelationIDInChecks, the mcp case
			// above); the http step's never did — a `body contains "...${cid8}"` claim reached
			// chain.HTTPStep with the placeholder LITERAL. Same rule, same helper.
			bodyWant = resolveBodyAssertsCorrelationID(bodyWant, corr)
			var poll *chain.Poll
			if st.Poll != nil {
				timeout, terr := time.ParseDuration(st.Poll.Timeout)
				if terr != nil {
					return preflightChainFail(s, corr, st.Name, "poll.timeout is not a valid duration: "+terr.Error())
				}
				interval, ierr := time.ParseDuration(st.Poll.Interval)
				if ierr != nil {
					return preflightChainFail(s, corr, st.Name, "poll.interval is not a valid duration: "+ierr.Error())
				}
				poll = &chain.Poll{Timeout: timeout, Interval: interval}
			}
			hasHTTPStep = true
			steps = append(steps, chain.HTTPStepWithOutput(st.Name, method, url, headers, bodyTmpl, wantStatus, bodyWant, st.Save, poll, st.Always, moneyAllow, moneyLedger,
				oc.chainCapture(c, s, st)))
		case "amqp":
			// AC-D18b. Validation refuses every one of these at seed time; the runner refuses them
			// AGAIN, because a file can reach a run without passing through the validator, and what
			// is at stake is a publish into a real agent's inbox (or a delete of a live queue/exchange,
			// P3 #24c). Nothing is dialed on a refusal.
			var pristineRecord json.RawMessage
			if i < len(pristineSteps) {
				pristineRecord = pristineSteps[i].Record
			}
			spec, want, why := amqpStepSpec(st, pristineRecord, claims[st.Name], declaredQueues, declaredExchanges, corr, c)
			if why != "" {
				return preflightChainFail(s, corr, st.Name, why)
			}
			steps = append(steps, chain.AMQPStep(st.Name, spec, want, dialAMQPBroker))
		default:
			return preflightChainFail(s, corr, st.Name, "unknown chain step type "+strconv.Quote(st.Type))
		}
	}
	// Patch #3: record the chain's end-to-end duration so it feeds
	// argus_scenario_duration_seconds (the Slowest-scenarios panel) like any scenario.
	start := time.Now()
	// ⛔ REVISED (P1 #12 follow-up): chain.Run's budget is now a GENEROUS PER-STEP-SCALED backstop,
	// not the raw declared ## TIMEOUT. The first cut of VR12-TO-RUN passed s.TimeoutDuration()
	// straight through as the WHOLE CHAIN's wall-clock deadline — but that value is capped at a FLAT
	// ceiling regardless of step count (120s for every `chain`-tagged scenario, sections.go's
	// timeoutCeiling), while each mcp step's own wait is now bounded by that SAME TIMEOUT individually
	// (the stepTimeout capping just above). A chain of several steps EACH legitimately taking close
	// to the full per-step TIMEOUT (a slow-but-healthy SUT) would sum past that flat ceiling and be
	// cut off mid-chain — measured: RACE-003 declares 9 steps at TIMEOUT=120s, i.e. ~13s/step on
	// average, while the target's own default per-call timeout is 30s; four steps anywhere near that
	// would already exceed 120s summed, though none of them individually breached anything. The chain
	// used to have NO deadline at all (unbounded), so this would be "a chain that used to always pass
	// now sometimes fails for a reason its author never chose" — the exact JMeter-process regression,
	// one layer up. The backstop is sized the same way jmeterProcessBackstop is: TIMEOUT × step count
	// (each step, worst case, legitimately taking the full per-step budget) — no separate JVM grace
	// (a chain step is a native Go call, not a subprocess).
	backstop := s.TimeoutDuration() * time.Duration(max(len(steps), 1))
	res := chain.Run(corr, steps, backstop)
	res.DurationMs = int(time.Since(start).Milliseconds())
	res.ID = s.ID
	oc.attachChainOutputs(s, &res, hasHTTPStep)
	// Test-requests panel (r3): chain.Run records one timestamped RequestSample per EXECUTED
	// step (by that step's RESPONSE outcome, 3-way) and skips the post-break "skipped" steps —
	// so the request carrier is already populated here; no extra counting needed.
	//
	// VR10-S2: the content assertions of the step that broke the chain go to failure.expected —
	// test hat only (redactExpected strips it for the product hat), as a single-call scenario's
	// EXPECT does. Observed stays reality-only (VR-C8).
	if res.Failure != nil && res.Failure.Expected == nil {
		for _, st := range res.Steps {
			// VR12-CH1: `skipped` is retired; a step that was NOT FIRED is `not-measured` and has
			// no enforced assertions to report anyway. A step that ran after the break and failed
			// DOES carry them, and it is a real (if subordinate) content failure.
			if st.Status != "passed" && st.Status != report.StepRanAfterFailureOK &&
				st.Status != report.StepNotMeasured && len(st.AssertionsEnforced) > 0 {
				exp := strings.Join(st.AssertionsEnforced, "; ")
				res.Failure.Expected = &exp
				break
			}
		}
	}
	return res
}

// chainStepExpect turns a step's `## EXPECT` claims into the mcp.Expect the judge applies — the
// chain-side twin of the single-call site, so both go through the one classifier.
//
// ⛔ V31-002 (R2): its LEGACY branch is gone. It used to fall back to the step's in-JSON `expect`
// when `## EXPECT` carried no claim for the step, so an un-migrated pack kept running. The key no
// longer exists — it is refused by name when a scenario is written — so there is nothing to fall
// back to, and a step with no claim is caught by the validator's empty-step guard instead.
func chainStepExpect(st chainStepSpec, claims []string) (mcp.Expect, error) {
	want, err := parseExpectPlane(claims)
	if err != nil {
		return mcp.Expect{}, fmt.Errorf("EXPECT %w", err)
	}
	return want, nil
}

// httpStepExpect turns a chain http step's `## EXPECT` claims into (wantStatus, bodyWant) — the
// http-side twin of chainStepExpect, and it goes through the SAME two grammars every other engine
// does: scenario.DeclaredStatuses (the `status=<code>` form, VR12-E7) and scenario.ParseBodyAsserts
// (the `body …` form, VR12-E8/V29-017) — no parallel assertion language, no third parser. wantStatus
// 0 means no status claim was declared (the body assertions alone decide the step).
//
// A claim that is neither form is refused BY NAME, exactly as an mcp step's unrecognised `expect`
// bullet is (CR-1) — never silently read as "expect success".
func httpStepExpect(claims []string) (wantStatus int, bodyWant []mcp.BodyAssert, err error) {
	// ONE parser, shared with scenario.Validate's write-time check of an http step,
	// so a claim refused here is refused at write time with the same reason text.
	return scenario.HTTPStepClaims(claims)
}

// amqpStepSpec turns a chain amqp step (its strings already resolved for ${cid}/${cid8}/
// ${correlation_id}/env by parseChainSpec) and its `## EXPECT` claims into the runner's spec and its
// AMQPStepWant — or a preflight refusal reason. A refusal never echoes url_env's value.
//
// declaredQueues/declaredExchanges are the names THIS chain declared with a declare_queue/
// declare_exchange step (chain_scenario.go, computed once per scenario) — the second door for P3
// #24c's guard: queue_delete/queue_unbind/exchange_delete refuse a name the chain never declared,
// unless the step sets `force: true`. scenario.Validate (amqpStepErrors) is the first door.
//
// corr is this run's correlation id, threaded through ONLY so a `consume` step's `body …` claim gets
// the same ${cid}/${cid8}/${correlation_id} resolution the mcp and http steps get (fix for the
// cid-in-chain-claims defect — a `content contains "...${cid8}"` claim used to reach chain.AMQPStep
// with the placeholder LITERAL, so it never matched a real delivered message).
// pristineRecord is the step's own `record` field exactly as authored — read from a SEPARATE,
// never-textually-substituted parse of the TRIGGER payload (runChainScenarioWithMoneyWrites builds
// it once, alongside the normal resolved `specs`, and matches steps by index). Every other amqp
// field is safe to pre-resolve with resolveVars (cid/cid8/correlation_id/env, all non-secret by the
// time a scenario is checked); `record` is not, because design §6 requires refusing a secret-looking
// ${VAR} BY NAME before its value ever exists in a variable this process touches — resolveVars would
// have already called os.Getenv on it. See buildAMQPRecordTemplate.
func amqpStepSpec(st chainStepSpec, pristineRecord json.RawMessage, claims []string, declaredQueues, declaredExchanges map[string]bool, corr string, c *config.Config) (chain.AMQPSpec, scenario.AMQPStepWant, string) {
	var none chain.AMQPSpec
	if st.Poll != nil {
		return none, scenario.AMQPStepWant{}, "an amqp step may not carry `poll` — it fires exactly once and is never retried"
	}
	if st.Always {
		return none, scenario.AMQPStepWant{}, "an amqp step may not carry `always` — it fires exactly once and is never re-fired as a cleanup"
	}
	if !scenario.ValidURLEnv(st.URLEnv) {
		return none, scenario.AMQPStepWant{}, "`url_env` must be an environment variable NAME matching " + scenario.URLEnvPattern +
			" (never a URL or a literal)"
	}
	if !contains(scenario.AMQPOps(), st.Op) {
		return none, scenario.AMQPStepWant{}, "unknown amqp `op` " + strconv.Quote(st.Op)
	}
	want, err := scenario.AMQPStepClaims(st.Op, claims)
	if err != nil {
		return none, scenario.AMQPStepWant{}, "EXPECT " + err.Error()
	}
	want.Body = resolveBodyAssertsCorrelationID(want.Body, corr)

	hasBody := len(st.Body) > 0 && strings.TrimSpace(string(st.Body)) != "null"
	hasSchema := st.Schema != ""
	spec := chain.AMQPSpec{Op: st.Op, URLEnv: st.URLEnv, Exchange: st.Exchange, RoutingKey: st.RoutingKey,
		Queue: st.Queue, ExchangeKind: st.ExchangeKind, UserID: st.UserID, Force: st.Force}
	if st.Op == "consume" && st.Schema != "" {
		schema, serr := c.MessageSchema(st.Schema)
		if serr != nil {
			return none, scenario.AMQPStepWant{}, serr.Error()
		}
		spec.ConsumeSchema = schema
	}
	if st.Op == "publish" {
		forms := 0
		if st.Envelope != nil {
			forms++
		}
		if hasBody {
			forms++
		}
		if hasSchema {
			forms++
		}
		if forms != 1 {
			return none, scenario.AMQPStepWant{}, "an amqp publish must carry exactly one of `envelope`, `body` or `schema`+`record`"
		}
		if hasSchema {
			schema, serr := c.MessageSchema(st.Schema)
			if serr != nil {
				return none, scenario.AMQPStepWant{}, serr.Error()
			}
			template, why := buildAMQPRecordTemplate(schema, pristineRecord, true)
			if why != "" {
				return none, scenario.AMQPStepWant{}, why
			}
			spec.Schema = schema
			spec.RecordTemplate = template
		} else if len(pristineRecord) > 0 && strings.TrimSpace(string(pristineRecord)) != "null" && st.Envelope == nil {
			// ⛔ `record` with neither `schema` nor `envelope` (e.g. `body` + `record`): refused, never
			// dropped. A real run loads scenarios with Parse, not Validate, so scenario/amqpstep.go's
			// authoring check never saw this step — this is the only gate it meets. Without it the
			// record was silently discarded and the literal `body` published (holdout H10).
			return none, scenario.AMQPStepWant{}, "an amqp publish declares `record` without `schema` — `schema` and `record` are required together (or `envelope` + `record`, which overrides the preset's fields)"
		} else if len(pristineRecord) > 0 && strings.TrimSpace(string(pristineRecord)) != "null" {
			// design §7: `envelope` + `record` — the override merges over the preset's defaults at
			// RUN time (internal/chain/amqp.go's publishEnvelope). It is only a PARTIAL record (just
			// the fields being overridden), so the full typed check against the whole schema is not
			// meaningful here (a field the override doesn't mention would refuse as "missing") — the
			// secret scan still runs (design §6 applies to every record, preset or not), and the
			// run-time check after merging over the defaults is the real gate.
			template, why := buildAMQPRecordTemplate(nil, pristineRecord, false)
			if why != "" {
				return none, scenario.AMQPStepWant{}, why
			}
			spec.RecordTemplate = template
		}
		if st.Envelope != nil {
			e := st.Envelope
			if e.Intent != "" && !contains(scenario.AMQPIntents, e.Intent) {
				return none, scenario.AMQPStepWant{}, "`envelope.intent` " + strconv.Quote(e.Intent) + " is not one of " +
					strings.Join(scenario.AMQPIntents, ", ")
			}
			spec.Envelope = &chain.AMQPEnvelope{ToInbox: e.ToInbox, FromAgentID: e.FromAgentID, Intent: e.Intent,
				Prompt: e.Prompt, CorrelationID: e.CorrelationID}
		} else {
			// A JSON string is sent unquoted, as text; any other JSON value as its JSON text.
			var text string
			if json.Unmarshal(st.Body, &text) == nil {
				spec.Body, spec.BodyContentType = text, "text/plain"
			} else {
				spec.Body, spec.BodyContentType = string(st.Body), "application/json"
			}
			spec.HasBody = true
		}
		// P3 #24b — custom publish headers, string values only; the second door (scenario.Validate
		// / amqpStepErrors is the first).
		hdrs, bad := scenario.HeaderValues(st.Headers)
		if len(bad) > 0 {
			return none, scenario.AMQPStepWant{}, "publish header " + strconv.Quote(bad[0]) + " must be a string value"
		}
		for k := range hdrs {
			if strings.EqualFold(k, scenario.AMQPSchemaHeader) {
				return none, scenario.AMQPStepWant{}, "publish header " + strconv.Quote(k) + " is reserved — the " +
					"msgbus envelope encoder sets it itself"
			}
		}
		if len(hdrs) > 0 {
			spec.Headers = hdrs
		}
	}
	if st.Op == "consume" && st.Wait != "" {
		w, werr := time.ParseDuration(st.Wait)
		if werr != nil || w <= 0 || w > scenario.AMQPConsumeWaitMax {
			return none, scenario.AMQPStepWant{}, "`wait` must be a positive Go duration of at most " +
				scenario.AMQPConsumeWaitMax.String()
		}
		spec.Wait = w
	}
	// P3 #24c — the write guard: refuse a delete/unbind of a name this chain never declared, unless
	// force says so explicitly. A delete is a write, same as a publish; unlike the write-vs-read
	// distinction moneyguard.go draws, this guard applies UNCONDITIONALLY (not only against a
	// money-handling SUT) — a check must not be able to delete a live queue by typo, ever.
	if !st.Force {
		switch st.Op {
		case "queue_delete":
			if !declaredQueues[st.Queue] {
				return none, scenario.AMQPStepWant{}, "would delete queue " + strconv.Quote(st.Queue) +
					", which no `declare_queue` step in this chain declares — set `force: true` to delete it anyway"
			}
		case "queue_unbind":
			if !declaredQueues[st.Queue] {
				return none, scenario.AMQPStepWant{}, "would unbind queue " + strconv.Quote(st.Queue) +
					", which no `declare_queue` step in this chain declares — set `force: true` to unbind it anyway"
			}
			if !declaredExchanges[st.Exchange] {
				return none, scenario.AMQPStepWant{}, "would unbind from exchange " + strconv.Quote(st.Exchange) +
					", which no `declare_exchange` step in this chain declares — set `force: true` to unbind it anyway"
			}
		case "exchange_delete":
			if !declaredExchanges[st.Exchange] {
				return none, scenario.AMQPStepWant{}, "would delete exchange " + strconv.Quote(st.Exchange) +
					", which no `declare_exchange` step in this chain declares — set `force: true` to delete it anyway"
			}
		}
	}
	// A leftover ${VAR} is an env var that was never set: refuse it rather than send the literal. A
	// ${saved.<var>} is legitimate and binds at run time (chain.AMQPStep), so it is stripped first.
	for name, v := range map[string]string{"exchange": spec.Exchange, "routing_key": spec.RoutingKey, "queue": spec.Queue,
		"exchange_kind": spec.ExchangeKind, "user_id": spec.UserID, "body": spec.Body} {
		if strings.Contains(savedRefRe.ReplaceAllString(v, ""), "${") {
			return none, scenario.AMQPStepWant{}, "unresolved placeholder in `" + name + "`"
		}
	}
	for k, v := range spec.Headers {
		if strings.Contains(savedRefRe.ReplaceAllString(v, ""), "${") {
			return none, scenario.AMQPStepWant{}, "unresolved placeholder in publish header " + strconv.Quote(k)
		}
	}
	if e := spec.Envelope; e != nil {
		for name, v := range map[string]string{"to_inbox": e.ToInbox, "from_agent_id": e.FromAgentID, "intent": e.Intent,
			"prompt": e.Prompt, "correlation_id": e.CorrelationID} {
			if strings.Contains(savedRefRe.ReplaceAllString(v, ""), "${") {
				return none, scenario.AMQPStepWant{}, "unresolved placeholder in `envelope." + name + "`"
			}
		}
	}
	return spec, want, ""
}

// msgbusEnvelopeSchemaName is the schema ref name the `envelope` preset is built over (design §7).
const msgbusEnvelopeSchemaName = "msgbus-envelope-v2"

// buildAMQPRecordTemplate is the authoring-time-like check a `schema`+`record` publish gets EVERY
// run (design §2/§8: the control plane only checks shape; this, with the app's real config in
// hand, checks the record against the declared schema in full) — on the PRISTINE, never
// textually-substituted record, so the secret scan (design §6) runs before any ${VAR} is ever
// resolved. It returns the marked template (avroschema.MarkWholeValuePlaceholders) ready for
// chain.AMQPSpec.RecordTemplate, or a refusal naming the field path.
func buildAMQPRecordTemplate(schema *avroschema.Schema, pristineRecord json.RawMessage, required bool) (any, string) {
	if len(pristineRecord) == 0 || strings.TrimSpace(string(pristineRecord)) == "null" {
		if required {
			return nil, "`schema` requires `record`"
		}
		return nil, ""
	}
	if bad := scenario.SecretLikeRecordVars(pristineRecord); len(bad) > 0 {
		return nil, "`record` references ${" + bad[0] + "}, which looks like a credential — a record may never carry a secret; put it in a header from the executor's own Secret instead"
	}
	var raw any
	if err := json.Unmarshal(pristineRecord, &raw); err != nil {
		return nil, "`record` is not valid JSON: " + err.Error()
	}
	marked := avroschema.MarkWholeValuePlaceholders(raw)
	if schema != nil {
		if _, ferr := avroschema.BuildNative(schema, marked, avroschema.Authoring); ferr != nil {
			return nil, ferr.Error()
		}
	}
	return marked, ""
}

// preflightChainFail builds a RED result for a chain that can't even be assembled.
func preflightChainFail(s *scenario.Scenario, corr, step, why string) report.ScenarioResult {
	return report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: "failed",
		Failure: &report.Failure{Observed: "chain step '" + step + "': " + why + " — preflight"}}
}

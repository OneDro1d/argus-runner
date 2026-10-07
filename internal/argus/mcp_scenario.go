package argus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// MCPTag marks a runner-native MCP-call scenario (D3): the runner-core calls the
// SUT's MCP tool itself and judges the two error planes in Go — NO JMeter. The
// scenario declares its server/transport/tool/args in the TRIGGER payload and its
// expected plane in EXPECT; it runs through the SAME RunAll pipeline as any scenario
// (VR-J9 / UC-80), tag-selectable by `mcp` and layer-selectable by `HTTP Ingestion`.
const MCPTag = scenario.MCPTag

var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
var rpcCodeRe = regexp.MustCompile(`-3\d{4}`)

// resolveVars expands ${cid} and ${correlation_id} (the scenario's correlation id — fillCorrelationID) and ${VAR} (env). An
// unresolved ${VAR} is left literal so the caller can fail it CLEARLY at preflight (never call a literal ${...} —
// UC-82). ⛔ Never for a CHECK: a check gets fillCorrelationID alone (V31-003).
func resolveVars(s, corr string) string {
	out, _ := substituteVars(s, corr, false, false)
	return out
}

// ingestionMarker is the NAME of the leading marker a chain http url may start with.
// It is not an environment variable, whatever the environment says.
const ingestionMarker = "INGESTION_URL"

// substituteVars is resolveVars with two switches, for text that is JSON or a chain url:
//
//   - asJSON: each environment value is escaped for the inside of a JSON string (`"`, `\`, control
//     characters), so a value is DATA to the parser: it cannot end the string it sits in, add a key or
//     break the document. A plain value (letters, digits) is unchanged, so a placeholder written outside a
//     string (`"count": ${N}`) still yields the same bytes it always did.
//   - skipMarker: ${INGESTION_URL} is left standing, never read from the environment, so the marker wins
//     over an environment variable of that name ( amended).
//
// The second result says whether any environment value was substituted: the caller uses it to keep a parse
// error from quoting a character of one (parseChainSpec).
func substituteVars(s, corr string, asJSON, skipMarker bool) (string, bool) {
	s = fillCorrelationID(s, corr)
	substituted := false
	out := varRe.ReplaceAllStringFunc(s, func(m string) string {
		name := varRe.FindStringSubmatch(m)[1]
		if skipMarker && name == ingestionMarker {
			return m
		}
		if v := os.Getenv(name); v != "" {
			substituted = true
			if asJSON {
				return jsonStringEscape(v)
			}
			return v
		}
		return m
	})
	return out, substituted
}

// jsonStringEscape escapes v for the inside of a JSON string literal and nothing more: every other byte,
// non-ASCII included, is kept as it is, so a plain value is byte-identical.
func jsonStringEscape(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(fmt.Sprintf(`\u%04x`, c))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

type mcpSpec struct {
	Transport string
	Tool      string
	RequestID string
	ServerURL string
	Args      any
}

// parseMCPSpec reads the mcp scenario's TRIGGER payload JSON
// ({transport, tool, args, request_id, server_url?}), with ${cid}/${correlation_id}/${VAR} resolved.
func parseMCPSpec(payload, corr string) (mcpSpec, error) {
	var raw struct {
		Transport string          `json:"transport"`
		Tool      string          `json:"tool"`
		RequestID string          `json:"request_id"`
		ServerURL string          `json:"server_url"`
		Args      json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal([]byte(resolveVars(payload, corr)), &raw); err != nil {
		return mcpSpec{}, err
	}
	var args any = map[string]any{}
	if len(raw.Args) > 0 {
		_ = json.Unmarshal(raw.Args, &args)
	}
	return mcpSpec{Transport: raw.Transport, Tool: raw.Tool, RequestID: raw.RequestID, ServerURL: raw.ServerURL, Args: args}, nil
}

// ── EXPECT bullet classification (VR10-S2 / V28-014, CR-1) ───────────────────────────────────
//
// Every `### Runnable` EXPECT bullet of an mcp scenario (a `### Non-runnable` bullet is never
// classified — VR12-E1, V31-006), and every bullet of a chain step's `expect`, is one of:
//
//	1. a PLANE bullet   — a row of expectPlaneTable (the phrases the parser matched before this
//	                      build, unchanged); sets ErrorPlane / ErrorCode
//	2. a BODY bullet    — ANY bullet opening with `body`, parsed by the ONE grammar in
//	                      internal/scenario/bodyassert.go (VR12-E8 R5), shared verbatim with the
//	                      HTTP path; fills Expect.Body with EVERY assertion it declares
//	3. PROSE            — a bullet that does not LOOK like an assertion: kept as documentation
//	4. NOT UNDERSTOOD   — a bullet that looks like an assertion (assertionShaped) but is neither
//	                      1 nor 2 → an error quoting the bullet and naming the accepted forms
//
// Before this build 3 and 4 shared one default branch ("expect success"), so an authored content
// assertion vanished in silence — measured 2026-09-04: 2 of 21 green RACE steps proved their own
// claim. The owner's lock (reg:1515-1518): ONLY assertion-shaped bullets are policed; everything
// else stays prose. CR-1 makes the policing a refusal, not a warning.

// expectPlanePhrase is one row of the plane table: a bullet whose lower-cased text contains EVERY
// phrase in `all` is that plane. A PlaneNone row is a KNOWN success bullet — it changes nothing
// (success is the default), it only stops the bullet from being reported as not understood.
type expectPlanePhrase struct {
	all   []string
	plane mcp.ErrPlane
	form  string // the canonical spelling, shown in refusal messages
}

// expectPlaneTable — the exact phrases the parser matched before this build, in the same order
// (a bullet naming both `jsonrpc error` and `isError` is the protocol plane, as it always was).
var expectPlaneTable = []expectPlanePhrase{
	{[]string{"jsonrpc", "error"}, mcp.PlaneProtocol, "jsonrpc error == <code>"},
	{[]string{"protocol error"}, mcp.PlaneProtocol, "protocol error <code>"},
	{[]string{"error code"}, mcp.PlaneProtocol, "error code <code>"},
	{[]string{"iserror", "true"}, mcp.PlaneTool, "result.isError == true"},
	{[]string{"iserror", "false"}, mcp.PlaneNone, "result.isError == false"},
}

// The assertion SHAPE (the owner's lock): a bullet beginning `body has`, or carrying an assertion
// operator/verb next to a field-ish token — a dotted or indexed path (result.isError,
// content[0].text) or a snake_case name (error_code). Prose has neither.
var (
	assertVerbRe = regexp.MustCompile(`(?i)==|!=|\bmust\b|\bcontains?\b|\bcontaining\b|\bmatch(?:es|ing)?\b`)
	fieldishRe   = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(?:[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+|\[[0-9]+\])+|[A-Za-z0-9]+_[A-Za-z0-9_]+)(?:[^A-Za-z0-9_]|$)`)
)

func assertionShaped(clause string) bool {
	return scenario.ClaimsToBeBodyAssert(clause) || (assertVerbRe.MatchString(clause) && fieldishRe.MatchString(clause))
}

// expectAcceptedForms is the accepted-values half of a CR-1 refusal.
const expectAcceptedForms = "`result.isError == false`, `result.isError == true`, `jsonrpc error == <code>`, " +
	"`body has <field>`, `body has <field> containing <value>`, `body has <field> matching <regex>`, " +
	"`body contains <value>`, `body matching <regex>`"

type expectKind int

const (
	expectProse expectKind = iota // documentation — changes nothing
	expectPlane                   // sets the plane (+ code)
	expectBody                    // a body assertion (extractBodyAsserts fills the fields)
)

type expectBullet struct {
	kind  expectKind
	plane mcp.ErrPlane // kind == expectPlane
	code  int          // kind == expectPlane, protocol only; 0 = any
}

// classifyExpectBullet is the ONE place a bullet's meaning is decided (the run at preflight and
// both validators go through it). The error is the CR-1 refusal: it quotes the bullet (in plain
// double quotes, never %q-escaped, so the author can grep for it), names the accepted forms and,
// where cheap, the nearest one.
func classifyExpectBullet(raw string) (expectBullet, error) {
	clause := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "-"))
	low := strings.ToLower(clause)
	// Gate-2 F1 (VR10-S2-6): ONE assertion per bullet. The chain path refuses a `;`-joined string by name; the
	// single-call path used to match the plane phrase and silently DROP whatever followed it — a content
	// assertion the author wrote became "did not error". Refuse the join, and refuse a plane phrase that is
	// followed by a second assertion, naming the fix.
	// Prose is never policed (the owner's lock) — only a bullet that carries an assertion shape.
	if strings.Contains(clause, ";") && (assertionShaped(clause) || matchesAnyPlane(low)) {
		return expectBullet{}, fmt.Errorf("bullet not understood: \"%s\" — it joins several assertions with \";\"; write one bullet per assertion, e.g. `- result.isError == false` and `- body contains <value>`", clause)
	}
	// VR12-E8 R5 — ONE grammar. A bullet that OPENS with "body" is CLAIMING to be a body assertion,
	// so scenario.ParseBodyAsserts decides whether it IS one. The classifier no longer carries a
	// second, narrower copy of the regex: this path used to accept only `body has …`, so
	// `body contains <value>` was classified as PROSE here while the HTTP path executed it — the
	// same sentence meaning two different things depending on the scenario's tag.
	//
	// ⛔ V31-004 fix 2a: THIS RUNS BEFORE THE PLANE TABLE. A bullet that opens with `body` is always a
	// content check, never a plane — `- body contains error code 7` used to match the protocol row and
	// become a plane expectation (measured on 0.3.31). And because the body branch is now first, a
	// plane phrase riding on a body bullet is the half that would be dropped, so it is refused here
	// with the same message the plane branch uses for the mirror case.
	//
	// VR10-S2-7 survives unchanged (an uncompilable `matching` regex is an authoring error, never a
	// red step against the SUT): compilability is checked inside the shared parser and surfaces here
	// as the refusal it always was.
	if scenario.ClaimsToBeBodyAssert(clause) {
		if carriesPlaneAssertion(low) {
			return expectBullet{}, fmt.Errorf("bullet not understood: \"%s\" — it carries a content assertion beside the plane phrase; write one bullet per assertion (`- body contains <value>` and the plane bullet, e.g. `- result.isError == false`)", clause)
		}
		if _, errs := scenario.ParseBodyAsserts([]string{clause}); len(errs) > 0 {
			return expectBullet{}, fmt.Errorf("bullet not understood: \"%s\" — %v", clause, errs[0])
		}
		return expectBullet{kind: expectBody}, nil
	}
	for _, row := range expectPlaneTable {
		if containsAll(low, row.all) {
			if carriesSecondAssertion(low) {
				return expectBullet{}, fmt.Errorf("bullet not understood: \"%s\" — it carries a content assertion beside the plane phrase; write one bullet per assertion (`- %s` and `- body contains <value>`, or `- body has <field> containing <value>` for a field of a JSON answer)", clause, row.form)
			}
			b := expectBullet{kind: expectPlane, plane: row.plane}
			if row.plane == mcp.PlaneProtocol {
				if m := rpcCodeRe.FindString(clause); m != "" {
					b.code, _ = strconv.Atoi(m)
				}
			}
			return b, nil
		}
	}
	if assertionShaped(clause) {
		return expectBullet{}, expectNotUnderstood(clause, nearestExpectForm(low))
	}
	return expectBullet{kind: expectProse}, nil
}

// carriesPlaneAssertion is fix 2a's mirror guard. The body branch now runs BEFORE the plane table, so
// a plane half riding on a body bullet would be the silently dropped one — but the test must be the
// EXPLICIT plane assertion, never matchesAnyPlane: the loose phrase "error code" is a legitimate part
// of a body VALUE, and `- body contains error code 7` is exactly the content check this row exists to
// stop being read as a protocol-plane expectation.
func carriesPlaneAssertion(low string) bool {
	return planeAssertionRe.MatchString(low)
}

var planeAssertionRe = regexp.MustCompile(`(?i)(result\.)?iserror\s*(==|=|:)|jsonrpc\s+error\s*==`)

func containsAll(low string, phrases []string) bool {
	for _, p := range phrases {
		if !strings.Contains(low, p) {
			return false
		}
	}
	return true
}

func expectNotUnderstood(clause, nearest string) error {
	msg := "bullet not understood: \"" + clause + "\" — accepted forms: " + expectAcceptedForms +
		"; a prose bullet must not carry an assertion operator (==, must, contains, matching) next to a field name"
	if nearest != "" {
		msg += " (did you mean `" + nearest + "`?)"
	}
	return errors.New(msg)
}

// nearestExpectForm is the cheap nearest-match hint for the shapes authors actually wrote
// (measured in examples/ 2026-09-05: `content[0].text == "…"` ×7, `content[0].text matches …` ×9).
func nearestExpectForm(low string) string {
	switch {
	case strings.Contains(low, "match"):
		return "body matching <regex>"
	case strings.Contains(low, "content") || strings.Contains(low, "text") || strings.Contains(low, "=="):
		return "body contains <value>"
	}
	return ""
}

// parseExpectPlane turns EXPECT bullets into the mcp.Expect the judge applies — all four fields:
//
//	"result.isError == false"           -> none (success)
//	"result.isError == true"            -> tool plane
//	"jsonrpc error == -32601" etc.      -> protocol plane (+ code)
//	"body has <f> containing <v>"       -> BodyContains (bare `body has <f>` → the JSON field <f> is present)
//	"body has <f> matching <re>"        -> BodyMatches
//
// It returns an error for the FIRST bullet it cannot classify (CR-1); both call sites turn that
// into a preflight `failed` with the bullet quoted, and ExpectProblems reports all of them at
// validate time. The body fields are filled by scenario.ParseBodyAsserts — the SAME parser the HTTP
// path uses, and it keeps EVERY assertion rather than the first of each kind (VR12-E8 R1).
func parseExpectPlane(expect []string) (mcp.Expect, error) {
	want := mcp.Expect{ErrorPlane: mcp.PlaneNone}
	for _, e := range expect {
		b, err := classifyExpectBullet(e)
		if err != nil {
			return mcp.Expect{}, err
		}
		if b.kind == expectPlane && b.plane != mcp.PlaneNone {
			want.ErrorPlane = b.plane
			if b.code != 0 {
				want.ErrorCode = b.code
			}
		}
	}
	// VR12-E8 R1/R5: EVERY body assertion, ANDed — on the single-MCP path AND on every chain step,
	// which both reach this function. a tester scoped his row to the status layer; the defect was
	// shared, so a chain step declaring two content assertions was dropping the second too.
	asserts, berrs := ParseBodyAsserts(expect)
	if len(berrs) > 0 {
		// R3: a bullet that claims to be a body assertion and parses as none is an ERROR here, not a
		// silent skip. This is the very `continue` V29-020 identified as the moment the product
		// decides it does not understand a line and says nothing.
		return want, berrs[0]
	}
	want.Body = asserts
	return want, nil
}

// ExpectProblem is one EXPECT bullet a validator must report (VR10-S2-8): the bullet, the chain
// step it belongs to ("" for a single-call mcp scenario) and the message the run would fail with.
type ExpectProblem struct {
	Step    string
	Bullet  string
	Message string
}

// ExpectProblems is the validate-time surface of the classifier — the same rules the run applies
// at preflight, applied to EVERY bullet so the author sees all of them at once. `argus
// validate-config --scenarios` and `author__validate_scenario` both call it (through toolcore).
// Only mcp and chain scenarios are this parser's business: an HTTP scenario's `body has` goes to
// the JSR223 path, and a chain scenario's own EXPECT section is documentation — the runner judges
// its steps' `expect` (specs/17-chain-scenarios.md).
func ExpectProblems(s *scenario.Scenario) []ExpectProblem {
	var out []ExpectProblem
	out = append(out, unreadablePropertyProblems(s)...)
	out = append(out, secondRequestProblems(s)...)
	switch {
	case contains(s.Tags, MCPTag):
		// ⛔ RUNNABLE ONLY (VR12-E1). It used to walk s.Expect, which now includes `### Non-runnable`
		// — prose the author deliberately marked as NOT a claim. Policing it would refuse a scenario
		// for a sentence it explicitly said was documentation, which is the exact inversion of the
		// split's purpose: POSITION decides whether a bullet is a claim, never its wording.
		for _, b := range s.RunnableExpect() {
			if _, err := classifyExpectBullet(b); err != nil {
				out = append(out, ExpectProblem{Bullet: strings.TrimSpace(b), Message: "EXPECT " + err.Error()})
			}
		}
	case contains(s.Tags, ChainTag):
		steps, err := parseChainSpec(s.Trigger.Payload, "validate")
		if err != nil {
			return []ExpectProblem{{Message: "chain scenario spec parse error: " + err.Error()}}
		}
		// VR12-E10: the claims live in `## EXPECT`, and since V31-002 that is the ONLY home — the
		// legacy in-JSON `expect` was read here too, so an un-migrated pack was validated as it was
		// written. The key is now refused by name (validate.go's W1), so there is nothing to read.
		claims, claimErrs := scenario.ParseChainClaims(s.RunnableExpect())
		for _, e := range claimErrs {
			out = append(out, ExpectProblem{Message: "EXPECT " + e.Error()})
		}
		for _, st := range steps {
			if st.Type != "mcp" {
				continue
			}
			for _, b := range claims[st.Name] {
				if _, err := classifyExpectBullet(b); err != nil {
					out = append(out, ExpectProblem{Step: st.Name, Bullet: b, Message: "step \"" + st.Name + "\" EXPECT " + err.Error()})
				}
			}
		}
	}
	return out
}

// mcpRequestOutcome classifies the MCP tool CALL's response for the per-request distribution
// panel (r3, 3-way). It splits the old two-way "error" into a real-response `failed` vs a
// no-response `error`:
//   - error  = the test couldn't get a response: the endpoint was unreachable or the
//     transport/handshake failed (mirrors HTTP conn-fail / a dead rig).
//   - failed = a negative RESPONSE the SUT actually returned: a top-level JSON-RPC protocol
//     error, or result.isError:true.
//   - success = a clean isError:false reply with no protocol error.
//
// This is the REQUEST's outcome, NOT the scenario's verdict (mcp.Judge) — a deliberate
// error-plane scenario PASSES while its call is recorded here as a `failed` response.
func mcpRequestOutcome(r mcp.CallResult) string {
	switch {
	case r.Unreachable || r.TransportErr != "":
		return report.OutcomeError
	case r.JSONRPCError != nil || r.IsError:
		return report.OutcomeFailed
	default:
		return report.OutcomeSuccess
	}
}

// runMCPScenario executes one mcp-typed scenario natively and builds a standard
// ScenarioResult. The MCP planes decide the error state and the scenario's own `body …` checks are then
// judged — on success and on an expected error, V31-005; Observed is reality-only
// (VR-C8). The judged envelope is captured in MCPEnvelope (VR-J10). Unreachable /
// transport / unresolved-var are reported with a distinct, gate-infra observed.
func runMCPScenario(c *config.Config, s *scenario.Scenario, corr string) report.ScenarioResult {
	res := report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: "failed"}
	spec, err := parseMCPSpec(s.Trigger.Payload, corr)
	if err != nil {
		res.Failure = &report.Failure{Observed: "mcp scenario spec parse error: " + err.Error()}
		return res
	}
	// VR10-S2 / CR-1 (S2-b): a `### Runnable` bullet the parser cannot classify is refused at PREFLIGHT,
	// quoting it — never silently read as "expect success". No request is fired: nothing was
	// measured, so nothing can have failed or errored at the SUT.
	//
	// ⛔ V31-006: RUNNABLE ONLY. This read s.Expect, so a `### Non-runnable` line — which the author
	// marked as documentation — could fail the scenario, flip the expected error plane, or stop it at
	// preflight. POSITION decides whether a bullet is a claim (VR12-E1), on every engine.
	want, perr := parseExpectPlane(s.RunnableExpect())
	if perr != nil {
		res.Failure = &report.Failure{Observed: "EXPECT " + perr.Error() + " — preflight"}
		return res
	}
	// ⛔ V31-003: AFTER classification, never before. The correlation id embeds the scenario id, an
	// id may contain `_`, and the classifier reads any `a_b` token as a field name — so filling in
	// first could turn a prose bullet into a "bullet not understood" refusal that the validator,
	// which reads the RAW text, never saw. The meaning of a bullet must not depend on the run's id.
	want = fillCorrelationIDInChecks(want, corr)
	// VR10-S3: `**Target**: <name>` selects targets.mcp_targets.<name> — its base_url, transport,
	// timeout and token — and a missing or wrong-kind name is refused by name, never replaced by
	// the plain slot. A payload server_url beside the word is two answers to one question (a
	// leftover ${VAR} placeholder is not an answer) and is refused too.
	sel, serr := c.SelectTarget(s)
	if serr != nil {
		res.Failure = &report.Failure{Observed: serr.Error() + " — preflight"}
		return res
	}
	mcpT := c.Targets.MCP
	var serverURL string
	if sel != nil {
		if spec.ServerURL != "" && !strings.Contains(spec.ServerURL, "${") {
			res.Failure = &report.Failure{Observed: "**Target**: " + s.Target + " and a payload server_url are two answers to one question — keep **Target** (server_url is the per-scenario override for a SUT with one endpoint) — preflight"}
			return res
		}
		mcpT = sel.MCP
		serverURL = mcpT.URL()
	} else {
		// CHANGE-1: endpoint resolution order — an explicit, RESOLVED per-scenario override
		// (TRIGGER url, then a server_url payload field) wins; otherwise fall back to the
		// argus-config targets.mcp.base_url (the single onboarding artifact). A leftover
		// ${VAR} placeholder is "not provided" and falls through to config, so the in-tree
		// `POST ${MCP_URL}` scenarios resolve from config with NO MCP_URL env set.
		serverURL = resolveVars(strings.Trim(s.Trigger.URL, "`"), corr)
		if serverURL == "" || strings.Contains(serverURL, "${") {
			serverURL = resolveVars(spec.ServerURL, corr)
		}
		if serverURL == "" || strings.Contains(serverURL, "${") {
			serverURL = c.MCPBaseURL()
		}
	}
	if serverURL == "" || strings.Contains(serverURL, "${") {
		res.Failure = &report.Failure{Observed: "no mcp endpoint resolved — declare targets.mcp.base_url in argus-config.yaml (or set a TRIGGER/server_url) — preflight"}
		return res
	}
	requestID := singleCallRequestID(spec.RequestID, corr)
	// VR12-TO-RUN: the scenario's own declared `## TIMEOUT` caps the per-call deadline — it can
	// only make the target's configured timeout (GAP-3, `mcpT.TimeoutOrDefault()`) TIGHTER, never
	// looser, so a SUT tuned slow-but-healthy at the target level keeps that protection; the
	// scenario's own promise is the one enforced when it is the smaller number. Every shipped
	// example kit declares TIMEOUT >= its target's configured timeout today (checked 2026-09-27),
	// so this is not expected to newly redden anything in tree.
	budget := s.TimeoutDuration()
	callTimeout := mcpT.TimeoutOrDefault()
	if budget > 0 && (callTimeout <= 0 || budget < callTimeout) {
		callTimeout = budget
	}
	// CHANGE-1/5: the config token MAY be a ${VAR} so a real bearer is supplied at runtime
	// (never committed to argus-config). resolveVars expands it; a literal/empty token is
	// returned unchanged.
	cl := &mcp.Client{ServerURL: serverURL, Transport: mcp.Transport(orDefault(spec.Transport, mcpT.TransportOrDefault())), Token: resolveVars(mcpT.Token(), corr), Timeout: callTimeout, RateLimit: rateLimitSpec(c),
		// VR12-T3 path 2: the author's own TRIGGER headers. resolveVars is already applied to the
		// token on this path, so ${VAR}/${cid}/${correlation_id} resolution is the same mechanism, not a new one.
		ExtraHeaders: resolvedHeaders(s, corr)}
	// Patch #3: record the end-to-end call duration so the runner-native MCP path feeds
	// argus_scenario_duration_seconds (the HTTP/JMeter path already does) — otherwise the
	// "Slowest scenarios" dashboard panel is empty for every MCP SUT.
	start := time.Now()
	callRes := cl.Call(mcp.CallInput{Tool: spec.Tool, Args: spec.Args, RequestID: requestID})
	res.DurationMs = int(time.Since(start).Milliseconds())
	v := mcp.JudgeWithRateLimit(callRes, want, cl.RateLimit)
	// The content checks the judge evaluated, rendered as a chain step renders them: on a pass, and
	// when the failing plane was the body (the expected plane matched, the content did not). The
	// error plane itself (`result.isError …`, an error code) is not a content check and is not
	// counted, so 0 still means only the no-error / error plane was checked.
	if v.Pass || v.Plane == mcp.PlaneBody {
		res.AssertionsEnforced = chain.EnforcedAssertions(want)
		res.AssertionsEnforcedCount = len(res.AssertionsEnforced)
	}
	// Test-requests panel (r3): exactly ONE request (the single tool call), recorded as a
	// timestamped sample by its RESPONSE outcome — NOT the scenario's verdict (an error-plane
	// scenario that EXPECTs isError:true PASSES, yet its call is recorded here as a `failed`
	// response). Early returns above (spec-parse / no endpoint resolved) fired NO request →
	// no sample. The fire-time is the call start (the runner fires it natively, in real time).
	//
	// VR10-S2 (owner lock D): a content miss IS a negative response — the SUT answered and the
	// answer was wrong — so it is tallied `failed`, exactly as a chain step's is (chain.stepOutcome).
	// It is never `error`: a measurement was obtained.
	outcome := mcpRequestOutcome(callRes)
	if !v.Pass && v.Plane == mcp.PlaneBody {
		outcome = report.OutcomeFailed
	}
	res.AddRequest(start.UnixMilli(), outcome)
	if len(callRes.Raw) > 0 {
		res.MCPEnvelope = callRes.Raw
	}
	// VR12-TO-RUN: the call was cut off by the deadline above, not merely unreachable. This is the
	// SAME event chain.go's Poll timeout names ("poll timed out after …", report.StepResult{Status:
	// "failed"}) — the SUT did not answer inside the budget IT was given, which is a claim about the
	// SUT, not "nothing was measured" (StatusError, the dead-rig/never-reached word). Checked before
	// v.Pass so a slow SUT that happened to answer just past the deadline is never scored a fluke pass.
	if callRes.TimedOut {
		res.Status = "failed"
		res.Failure = &report.Failure{Observed: fmt.Sprintf(
			"mcp call timed out after %s against a declared ## TIMEOUT of %s", callTimeout, strings.TrimSpace(s.Timeout))}
		return res
	}
	if v.Pass {
		res.Status = "passed"
		return res
	}
	// VR10-R1 (owner D7): the SUT REFUSED to answer because we were going too fast — it matched the
	// signature the SUT itself declared. Nothing was measured, so this is `errored` (grey) with no
	// Expected, exactly like the never-reached case: `failed` would say the SUT did the wrong thing,
	// and it did not. The run loop reads RateLimited/RetryAfterMs and pauses BETWEEN scenarios.
	if v.RateLimited {
		res.Status = report.StatusError
		res.RateLimited = true
		res.RetryAfterMs = int(v.RetryAfter.Milliseconds())
		res.Failure = &report.Failure{Observed: v.Observed}
		return res
	}
	observed := v.Observed
	if v.SoftWarning != "" {
		observed += " [" + v.SoftWarning + "]"
	}
	// The SUT never answered — unreachable, or the transport failed. mcpRequestOutcome already
	// classified the call that way one level down; without this the scenario could only say
	// "failed", i.e. report an absent server as a product defect.
	if res.NeverReachedSUT() {
		res.Status = report.StatusError
		res.Failure = &report.Failure{Observed: observed} // no Expected: nothing was measured
		return res
	}
	exp := strings.Join(s.Expect, "; ")
	res.Failure = &report.Failure{Observed: observed, Expected: &exp}
	return res
}

// singleCallRequestID is the `_meta.request_id` a single-call mcp scenario sends. An id DERIVED from
// the correlation id — none given, or one built from ${cid} — is made unique per call
// (mcp.PerCallRequestID), because the run loop re-fires a throttled scenario ONCE with the same
// correlation id and a SUT enforcing uniqueness would refuse the re-fire as "request_id reused". A
// literal the author wrote with no ${cid} in it is theirs, and is sent exactly as written.
func singleCallRequestID(authored, corr string) string {
	switch {
	case authored == "":
		return mcp.PerCallRequestID(corr)
	case corr != "" && strings.Contains(authored, corr):
		return mcp.PerCallRequestID(authored)
	default:
		return authored
	}
}

// matchesAnyPlane reports whether a lower-cased bullet carries any plane phrase of the table.
func matchesAnyPlane(low string) bool {
	for _, row := range expectPlaneTable {
		if containsAll(low, row.all) {
			return true
		}
	}
	return false
}

// carriesSecondAssertion reports a content assertion riding on a plane bullet — a `body has`, a `content[…]`
// reference or a second `==` — the shapes gate 2 measured being swallowed (VR10-S2-6).
func carriesSecondAssertion(low string) bool {
	return secondBodyFormRe.MatchString(low) || strings.Contains(low, "content[") || strings.Count(low, "==") >= 2 || strings.Contains(low, " matches ")
}

// secondBodyFormRe is fix 2a (V31-004): ANY `body …` form, not only `body has`. The moment this row
// teaches `body contains …` and `body matching …`, the form every hint recommends is the one gate 2's
// narrower check still swallowed — `- result.isError == false, body contains X` was accepted as a
// plain success bullet with its content check silently dropped (VR10-S2-6, measured on 0.3.31).
var secondBodyFormRe = regexp.MustCompile(`\bbody\s+(has|contains?|containing|match(es|ing)?)\b`)

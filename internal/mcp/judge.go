// Package mcp is the M2.5 D3 MCP-call testing path — the SOLE no-JMeter,
// runner-native executor. It makes an MCP tool call (both transports) and judges
// the response on the two MCP-spec error planes IN GO. This file is the judge; the
// client is in client.go.
//
// The two planes decide the error state (per a_mcp-testing-approach.md); the scenario's own body checks are then
// judged against the answer:
//   - protocol plane: a top-level JSON-RPC `error` (unknown tool -32601, bad params -32602)
//   - tool plane:     a JSON-RPC `result` with `isError:true`
//
// The advisory content scan (softWarn) is NEVER binding. A scenario's own `body …` checks ARE
// binding — on a success answer and, since V31-005, on an expected error. The judge is
// reality-only (VR-J12 / VR-C8): `Observed` describes the responder's actual behaviour and NEVER
// echoes the test's expected value.
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrPlane is the error plane an mcp scenario expects.
type ErrPlane string

const (
	PlaneNone     ErrPlane = "none"     // success expected (isError:false, no JSON-RPC error)
	PlaneProtocol ErrPlane = "protocol" // a top-level JSON-RPC error expected
	PlaneTool     ErrPlane = "tool"     // result.isError:true expected
)

// Expect is what an mcp scenario asserts: the error plane it expects, and the body checks judged
// once that plane matched — on success and on an expected error alike (V31-005).
type Expect struct {
	ErrorPlane ErrPlane // none | protocol | tool
	ErrorCode  int      // optional: for the protocol plane (-32601 / -32602); 0 = any
	// VR12-E8 (V29-017 R1/R5): EVERY declared body assertion, in order, ANDed. This replaces the
	// two scalars that kept only the FIRST `containing` and the FIRST `matching` — which is why a
	// scenario declaring two content assertions had exactly one of them judged, on the single-MCP
	// path AND on every chain step. a tester scoped his row to the status layer; the defect was shared.
	Body []BodyAssert
	// BodyContains / BodyMatches are the pre-VR12-E8 scalars, kept ONLY so an out-of-tree caller
	// still compiles. Judging reads Body. ⛔ Do not add new uses.
	BodyContains string
	BodyMatches  string
}

// Body assertion operators (VR12-E8). BodyEqualsOp (P3 #24a) is an EXACT match, added for the amqp
// consume step's body claim (`body has <field> equals <value>` / `body equals <value>`) — the
// existing `contains`/`matches`/`exists` forms cannot express "the value IS exactly this", which a
// JSON-path check on a message body routinely needs (a status field that must read `"ok"` and
// nothing else). It is exposed here, not amqp-only, because VR12-E8's rule is ONE parser for every
// path — http and mcp scenarios may write `body equals …` too, not just amqp.
const (
	BodyContainsOp = "contains"
	BodyMatchesOp  = "matches"
	BodyExistsOp   = "exists"
	BodyEqualsOp   = "equals"
	// BodyGTOp/BodyGTEOp/BodyLTOp/BodyLTEOp (item 25, http-step gap) are the numeric comparison
	// operators: the target (a JSON path in the body, ALWAYS field-scoped — a numeric comparison
	// against the raw whole-response text is not a form this grammar offers) is parsed as a
	// float64 and compared to Value, also parsed as a float64. A non-numeric observed value is a
	// MISS, never a pass and never a skip — the same R4 degradation bodyAssertMiss already applies
	// to an absent field, so a numeric check on a text/prose answer fails honestly instead of
	// silently passing vacuously.
	BodyGTOp  = "gt"
	BodyGTEOp = "gte"
	BodyLTOp  = "lt"
	BodyLTEOp = "lte"
)

// isNumericOp reports whether op is one of the four numeric comparison operators.
func isNumericOp(op string) bool {
	switch op {
	case BodyGTOp, BodyGTEOp, BodyLTOp, BodyLTEOp:
		return true
	}
	return false
}

// BodyAssert is ONE parsed body assertion. Field is the whole point of the grammar: empty means
// "anywhere in the answer" (the `body contains …` form); non-empty pins it to a named, possibly
// dotted field (`body has data.token containing …`).
//
// ⚠ It lives HERE, not in internal/argus, because argus imports mcp and the reverse would cycle.
// ⛔ It is NOT mcp.RateLimitSpec.BodyContains (ratelimit.go) — that is a DIFFERENT field describing
// the SUT's throttle signature. Converting every `BodyContains` in this package would break
// throttle detection, which is why V29-017 names the trap explicitly.
type BodyAssert struct {
	Field string
	Op    string
	Value string
}

// Scoped reports whether this assertion is pinned to a named field.
func (b BodyAssert) Scoped() bool { return b.Field != "" }

// PlaneBody is the Verdict.Plane of a content-assertion miss (VR10-S2, V31-005):
// the expected plane matched — success, or the expected error — but the text checked
// did not satisfy the scenario's `body …`. It is never an Expect.ErrorPlane — a scenario cannot "expect" the body plane.
const PlaneBody = "body"

// BodyAssertObserved is the REALITY-ONLY observed message for a content-assertion miss. VR-C8:
// observed is shown to BOTH hats, so it must never echo the asserted value (that lives only in
// failure.expected, test hat). It states what happened without reproducing what was asserted.
const BodyAssertObserved = "responder returned result.isError:false, no JSON-RPC error, but the result.content text did not satisfy the scenario's content assertion " + ClaimsHeldOutNote

// ClaimsHeldOutNote closes every reality-only Observed text of a content-assertion miss. On a
// single-call scenario the asserted value is in failure.expected (test hat). A CHAIN step replaces it
// with its own pointer (the step's failed_claims, chain.chainClaimsNote), because there the per-claim
// record is that field and failure.expected only joins the claims.
const ClaimsHeldOutNote = "(the asserted value is held out; the test hat reads it in the failure record)"

// BodyAssertObservedToolError / BodyAssertObservedProtocolError are the REALITY-ONLY observed messages for a check
// that missed on an EXPECTED error (V31-005). Like BodyAssertObserved they never echo the asserted value (VR-C8),
// and they never say what was expected (VR-J12): Observed reaches the product hat too.
//
// ⛔ BodyAssertObserved itself is NOT reused: it says "result.isError:false, no JSON-RPC error", which is exactly
// what did not happen on an error answer.
const BodyAssertObservedToolError = "responder returned result.isError:true, but the result.content text did not satisfy the scenario's content assertion " + ClaimsHeldOutNote
const BodyAssertObservedProtocolError = "responder returned a JSON-RPC error, but the error object (code, message, data) did not satisfy the scenario's content assertion " + ClaimsHeldOutNote

// RPCError is a top-level JSON-RPC error (the protocol plane).
//
// V31-005: `Data` carries the error's `data` member, so a check on an expected protocol error can reach it
// (`body has data.<path> …`). client.go:311 already decodes "error" into *RPCError, so it arrives once the field
// exists; mcp-call's JSON output and the preflight decode gain it too, both of them the SUT's own text.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ContentBlock is one result.content entry.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CallResult is the client's parsed view of a tools/call response (client.go fills it).
type CallResult struct {
	JSONRPCError *RPCError       // top-level error (protocol plane), nil if absent
	IsError      bool            // result.isError (tool plane)
	Content      []ContentBlock  // result.content
	Raw          json.RawMessage // the full raw envelope (captured for report.MCPEnvelope)
	Unreachable  bool            // preflight: endpoint not reachable (distinct from tool-failed)
	TransportErr string          // handshake / declared-transport mismatch (a requirements failure)
	// TimedOut (VR12-TO-RUN) is set when the client's own per-call deadline elapsed before the SUT
	// answered — the caller's http.Client.Timeout, capped by the scenario's declared `## TIMEOUT`
	// (see mcp_scenario.go's mcpCallTimeout). DISTINCT from an ordinary Unreachable/TransportErr: a
	// connection genuinely refused is "never reached", but a deadline that elapsed after we started
	// waiting is a SUT that did not answer IN TIME, which the runner reports `failed`, never
	// `error` — the same distinction chain.go's Poll timeout already makes for a chained http step.
	TimedOut bool
}

// Verdict is the judge's reality-only result.
type Verdict struct {
	Pass        bool           `json:"pass"`
	Plane       string         `json:"plane"` // ok | protocol | tool | unreachable | transport | body | rate-limited
	Observed    string         `json:"observed"`
	SoftWarning string         `json:"soft_warning,omitempty"`
	Detail      map[string]any `json:"detail,omitempty"`
	// RateLimited (VR10-R1) says the SUT REFUSED to answer because we were going too fast — it
	// matched the signature the SUT itself declared. Nothing was measured, so the scenario is
	// `errored` (grey), never `failed`: `failed` means the SUT did the wrong thing, and it did not.
	RateLimited bool `json:"rate_limited,omitempty"`
	// RetryAfter is how long the SUT asked us to wait — its own retry_after when it gave one,
	// otherwise the declared window. The run pauses for it BETWEEN scenarios (never inside a
	// scenario's timeout clock, which would just manufacture a timeout red).
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

// Judge applies the two-plane rules, then the scenario's body checks. Reachability / transport failures are reported
// DISTINCTLY (not as a tool pass/fail). The binding pass/fail is the two planes only;
// the content soft-warning is advisory and never changes Pass.
func Judge(r CallResult, want Expect) Verdict { return JudgeWithRateLimit(r, want, nil) }

// PlaneRateLimited is the Verdict.Plane of a declared throttle (VR10-R1). Like PlaneBody it is
// never an Expect.ErrorPlane — a scenario cannot "expect" to be rate-limited.
const PlaneRateLimited = "rate-limited"

// JudgeWithRateLimit is Judge plus the SUT's DECLARED rate-limit signature (VR10-R1 / V28-009).
// With a nil spec (or an undeclared signature) it is byte-for-byte the old judge — VR10-R1-3: with
// no rate_limit block, behaviour is identical to today.
//
// The throttle check runs BEFORE the plane switch on purpose. A scenario that expects the tool
// plane (`result.isError == true`) would otherwise go GREEN on a rate-limit refusal, which is a
// false pass over a scenario that was never measured — the worst direction for a testing product.
func JudgeWithRateLimit(r CallResult, want Expect, rl *RateLimitSpec) Verdict {
	if r.Unreachable {
		return Verdict{Pass: false, Plane: "unreachable", Observed: "MCP endpoint unreachable (preflight) — gate-infra, not a SUT failure"}
	}
	if r.TransportErr != "" {
		return Verdict{Pass: false, Plane: "transport", Observed: "transport/handshake failure: " + r.TransportErr}
	}
	if hit, after := rateLimited(r, rl); hit {
		return Verdict{Pass: false, Plane: PlaneRateLimited, Observed: RateLimitedObserved(after), RateLimited: true, RetryAfter: after}
	}
	soft := softWarn(r)
	switch want.ErrorPlane {
	case PlaneProtocol:
		if r.JSONRPCError != nil && (want.ErrorCode == 0 || r.JSONRPCError.Code == want.ErrorCode) {
			// V31-005: the expected error came back — now its checks, against the whole error object.
			if bodyMiss(errorObjectText(r.JSONRPCError), want) {
				return Verdict{Pass: false, Plane: PlaneBody, Observed: BodyAssertObservedProtocolError, SoftWarning: soft}
			}
			return Verdict{Pass: true, Plane: "protocol", Observed: fmt.Sprintf("responder returned JSON-RPC error %d", r.JSONRPCError.Code), SoftWarning: soft}
		}
		return Verdict{Pass: false, Plane: "protocol", Observed: observedReality(r), SoftWarning: soft}
	case PlaneTool:
		if r.JSONRPCError == nil && r.IsError {
			// V31-005: the expected error came back — now its checks, against result.content[*].text.
			if bodyMiss(ContentText(r), want) {
				return Verdict{Pass: false, Plane: PlaneBody, Observed: BodyAssertObservedToolError, SoftWarning: soft}
			}
			return Verdict{Pass: true, Plane: "tool", Observed: "responder returned result.isError:true", SoftWarning: soft}
		}
		return Verdict{Pass: false, Plane: "tool", Observed: observedReality(r), SoftWarning: soft}
	default: // PlaneNone: success expected
		if r.JSONRPCError == nil && !r.IsError {
			// VR10-S2 (V28-014): both planes said success — now the CONTENT assertion, if the
			// scenario made one. Judged against the concatenated result.content[*].text ONLY, never
			// Raw: the envelope echoes the request's id / _meta.request_id, and a scenario must not
			// pass on its own correlation id. A miss is a distinct plane with a reality-only observed.
			if bodyMiss(ContentText(r), want) {
				return Verdict{Pass: false, Plane: PlaneBody, Observed: BodyAssertObserved, SoftWarning: soft}
			}
			return Verdict{Pass: true, Plane: "ok", Observed: "responder returned result.isError:false, no JSON-RPC error", SoftWarning: soft}
		}
		return Verdict{Pass: false, Plane: "ok", Observed: observedReality(r), SoftWarning: soft}
	}
}

// BodyAssertsMiss reports whether text fails to satisfy every one of asserts (ANDed) — the SAME
// grammar and the SAME judgement bodyMiss/bodyAssertMiss apply to an MCP step's
// `result.content[*].text`, exported for an engine outside the JSON-RPC envelope (the chain `http`
// step judges its own response body against this identical grammar; VR10-S2-owner: reuse the
// existing judge, never a parallel assertion language).
func BodyAssertsMiss(text string, asserts []BodyAssert) bool {
	return bodyMiss(text, Expect{Body: asserts})
}

// BodyAssertsMissReason is BodyAssertsMiss plus WHY, for an engine (the chain `http` step) that
// wants to distinguish "a numeric comparison found a non-numeric observed value" from an ordinary
// content mismatch in its own reality-only Observed line. Reality-only: it says WHAT kind of thing
// went wrong (a number was expected and the field held something else), never the threshold or the
// field's actual value (VR-C8 holds exactly as it does for BodyAssertObserved).
func BodyAssertsMissReason(text string, asserts []BodyAssert) (miss bool, nonNumeric bool) {
	for _, a := range asserts {
		if !bodyAssertMiss(text, a) {
			continue
		}
		miss = true
		if !isNumericOp(a.Op) {
			continue
		}
		target := text
		ok := true
		if a.Scoped() {
			target, ok = jsonField(text, a.Field)
		}
		if ok {
			if _, perr := strconv.ParseFloat(strings.TrimSpace(target), 64); perr != nil {
				nonNumeric = true
			}
		}
	}
	return miss, nonNumeric
}

// BodyClaimText is the text a step's content claims are judged against for this call: the error object
// of an expected protocol error, result.content[*].text otherwise — exactly what the judge matched.
func BodyClaimText(r CallResult, want Expect) string {
	if want.ErrorPlane == PlaneProtocol && r.JSONRPCError != nil {
		return errorObjectText(r.JSONRPCError)
	}
	return ContentText(r)
}

// BodyAssertObservation judges ONE claim against text, the way the judge does, and says what it saw:
// miss reports whether the claim did not hold; observed is, for a claim scoped to a field, that field's
// value as text (FieldAbsentObserved when the answer is not JSON or has no such field), and for an
// unscoped claim the whole text. Author-side evidence: the caller bounds it and keeps it off every
// builder path.
func BodyAssertObservation(text string, a BodyAssert) (miss bool, observed string) {
	if !bodyAssertMiss(text, a) {
		return false, ""
	}
	if !a.Scoped() {
		return true, text
	}
	v, ok := jsonField(text, a.Field)
	if !ok {
		return true, FieldAbsentObserved
	}
	return true, v
}

// FieldAbsentObserved is the observed text of a field-scoped claim whose field the answer does not
// have (or whose answer is not JSON) — a miss by rule R4, not a value.
const FieldAbsentObserved = "(field absent, or the answer is not JSON)"

// ContentText is the text a content assertion is matched against on a success answer and on an expected tool
// error: result.content[*].text, concatenated in order (one block per line). Never the raw envelope
// (VR10-S2-3, owner-locked). An expected protocol error is matched against errorObjectText instead.
func ContentText(r CallResult) string {
	parts := make([]string, 0, len(r.Content))
	for _, c := range r.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

// errorObjectText is what a body check on an EXPECTED protocol error is matched against (V31-005, the owner
// 2026-09-11: "the whole error object"): the JSON-RPC error as {"code":…,"message":…,"data":…}, `data` omitted
// when the SUT sent none (a JSON null stays null). HTML escaping is off, so a message is matched as the SUT wrote it.
func errorObjectText(e *RPCError) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e)
	return strings.TrimSpace(b.String())
}

// bodyMiss reports whether the scenario's content assertions (if any) are NOT satisfied by text —
// result.content[*].text, or the error object of an expected protocol error (V31-005). A regex that does not
// compile cannot match: the scenario parser refuses it at validate/preflight time before a call is ever made, so
// here it is simply a miss.
func bodyMiss(text string, want Expect) bool {
	if len(want.Body) == 0 {
		return false
	}
	// VR12-E8 R1: ALL of them must hold. Any single miss fails the assertion — no bullet is
	// discarded, and none is allowed to pass merely because an earlier one did.
	for _, a := range want.Body {
		if bodyAssertMiss(text, a) {
			return true
		}
	}
	return false
}

// bodyAssertMiss evaluates ONE assertion against the answer text.
//
// VR12-E8 R4 — a FIELD-SCOPED assertion that cannot be evaluated is a MISS, never a pass and never a
// skip. On the MCP plane the answer is `result.content[*].text`
// (or, for an expected protocol error, the error object), which is frequently prose rather
// than JSON, so a field-scoped assertion has to degrade honestly: if the text is not JSON, or the
// field is absent, the assertion has not been satisfied. That is what steers an author to
// `body contains …` for a text answer instead of letting a field assertion pass vacuously on prose.
func bodyAssertMiss(text string, a BodyAssert) bool {
	target := text
	if a.Scoped() {
		v, ok := jsonField(text, a.Field)
		if !ok {
			return true // R4: field absent, or the answer is not JSON → FAIL
		}
		target = v
	}
	switch a.Op {
	case BodyExistsOp:
		return false // reaching here means the field resolved, which IS the assertion
	case BodyMatchesOp:
		re, err := regexp.Compile(a.Value)
		// A regex that does not compile is refused at validate/preflight before any call is made,
		// so here it can only be a miss — never a silent pass.
		return err != nil || !re.MatchString(target)
	case BodyGTOp, BodyGTEOp, BodyLTOp, BodyLTEOp:
		miss, _ := numericAssertMiss(target, a.Value, a.Op)
		return miss
	case BodyEqualsOp:
		return target != a.Value // EXACT match — unlike BodyContainsOp, extra text either side is a miss
	default:
		return !strings.Contains(target, a.Value)
	}
}

// numericAssertMiss evaluates ONE numeric comparison. nonNumeric reports whether the MISS was
// caused by an observed value that could not be parsed as a number — item 25's "a non-numeric
// observed value fails with a reason that says so" — distinct from an ordinary threshold miss, so
// a caller building Observed can name WHAT went wrong without ever echoing the threshold (Value).
// Value itself is validated numeric at authoring time (scenario.ParseBodyAsserts), so a parse
// failure on it here would be a defect in that guard, not a runtime input — treated as a miss
// either way, never a panic and never a silent pass.
func numericAssertMiss(observed, thresholdStr, op string) (miss bool, nonNumeric bool) {
	ov, oerr := strconv.ParseFloat(strings.TrimSpace(observed), 64)
	if oerr != nil {
		return true, true
	}
	tv, terr := strconv.ParseFloat(strings.TrimSpace(thresholdStr), 64)
	if terr != nil {
		return true, false
	}
	switch op {
	case BodyGTOp:
		return !(ov > tv), false
	case BodyGTEOp:
		return !(ov >= tv), false
	case BodyLTOp:
		return !(ov < tv), false
	case BodyLTEOp:
		return !(ov <= tv), false
	}
	return true, false
}

// jsonField navigates a dotted path through a JSON document and returns the value as text.
// Reports ok=false when the text is not JSON or the path does not resolve — R4 makes both a FAIL.
func jsonField(text, path string) (string, bool) {
	var doc any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &doc); err != nil {
		return "", false
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			nxt, ok := v[seg]
			if !ok {
				return "", false
			}
			cur = nxt
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(v) {
				return "", false
			}
			cur = v[i]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

// observedReality describes ONLY what the responder actually did — never the
// expectation (VR-J12 / VR-C8). It names the plane + code, not any test value.
func observedReality(r CallResult) string {
	if r.JSONRPCError != nil {
		return fmt.Sprintf("responder returned JSON-RPC error %d", r.JSONRPCError.Code)
	}
	if r.IsError {
		return "responder returned result.isError:true"
	}
	return "responder returned result.isError:false, no JSON-RPC error"
}

// softWarn scans result.content for a buried error marker when the structured
// planes say success — an ADVISORY warning that the server may be non-conformant
// (the pre-rebuild Social pattern). It NEVER changes the verdict (VR-J4 / UC-26).
func softWarn(r CallResult) string {
	if r.JSONRPCError != nil || r.IsError {
		return "" // the error is already on a binding plane; nothing buried
	}
	for _, c := range r.Content {
		t := strings.ToLower(c.Text)
		if strings.Contains(t, "error_code") || strings.Contains(t, "\"error\"") || strings.Contains(t, "iserror") {
			return "possible non-conformant error buried in result.content (advisory; verdict follows the two planes only)"
		}
	}
	return ""
}

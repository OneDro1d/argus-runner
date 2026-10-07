package scenario

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// VR12-E10 (V30-002) — A CHAIN'S CLAIMS HAVE EXACTLY ONE HOME, AND IT IS `## EXPECT`.
//
// A chain used to carry its real assertions INSIDE the TRIGGER's JSON, one `expect` per step, while
// its `## EXPECT` was documentation by design (skills/scenario-author/SKILL.md:152 said so outright).
// So a scenario had two places that LOOK like they hold its claims and only one did — and authors
// filled the decorative one with correctly-formed assertions. Measured: chain scenarios carry 127
// EXPECT bullets, 38 of them `result.isError == …`. Those 38 parse, are recognised syntax, and were
// never executed by anything.
//
// ⛔ THE REJECTED VARIANT, recorded so it is never re-proposed: bullets in EXPECT ALONGSIDE the
// step's own `expect`. That creates two homes for one claim and forces a precedence rule where every
// possible answer is worse than today — EXPECT wins ⇒ the step's expect becomes decorative (the same
// defect relocated); the step wins ⇒ EXPECT is decorative (today's bug with ceremony); both must
// agree ⇒ the author maintains identical text twice and drift is guaranteed.
//
// ⛔ THE NEW FAILURE MODE THIS INTRODUCES, AND ITS GUARD. An assertion now references its step BY
// NAME, so renaming a step silently orphans its claim — a new way to produce a scenario that asserts
// nothing, which is the exact defect class this round closes. The orphan guard is part of the
// design, not a follow-up (E10), and so is the empty-step guard.

// IngestionURLToken is the leading marker a chain http step's url (and a plain check's trigger url) uses
// for "the configured base URL". It is not an environment variable.
const IngestionURLToken = "${INGESTION_URL}"

// ChainStep is one step of a chain scenario's TRIGGER payload.
//
// ⛔ IT LIVES HERE, NOT IN internal/argus, for the same reason the body grammar and the status
// reader do: tier 2 has to read the steps to enforce E10, and internal/scenario cannot import
// internal/argus. argus aliases this type rather than keeping a second copy — two decoders of one
// wire format is how a validator ends up policing a shape the runner does not run.
type ChainStep struct {
	Type string `json:"type"` // "mcp" | "ui" | "http" | "amqp"
	Name string `json:"name"`
	// mcp step:
	Transport string `json:"transport"`
	// Target selects targets.mcp_targets.<name> for this step (VR10-S3) — the documented form.
	// ServerURL stays the per-step override for a SUT with ONE endpoint; a step that sets both is
	// refused (two answers to one question).
	Target    string          `json:"target"`
	ServerURL string          `json:"server_url"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	// ⛔ `Expect` — the LEGACY in-JSON home of a step's claims — is REMOVED (V31-002). V30-002 moved
	// the claims to `## EXPECT` and kept this key readable so no catalogue stopped running mid-
	// migration; the owner then ruled old-format scenarios unsupported, so the key is gone from the
	// struct, gone from the runner, and REFUSED BY NAME when a scenario is written (validate.go's
	// W1, which decodes the steps a second time as raw JSON precisely because this field no longer
	// exists). A claim has one home.
	// Save declares what this step publishes for LATER steps: variable name -> path into this
	// step's response (chain/capture.go). A later step references it as ${saved.<var>} in its args.
	// A value is a JSON dot-path string, or {"regex": "<one capture group>"} for a text body (SaveSpec).
	Save map[string]SaveSpec `json:"save"`
	// ui step:
	Spec   string `json:"spec"`
	AppURL string `json:"app_url"`
	// http step (AC-D20) — a native Go HTTP request/response lifecycle, for a REST resource no
	// mcp tool fronts (a Coder workspace's create/poll/stop/delete lifecycle is the concrete case).
	// Method/URL/Headers/Body resolve ${cid}/${cid8}/${correlation_id}/env exactly as an mcp step's
	// args do (chain_scenario.go resolveVars); ${saved.<var>} binds at RUN time, in url, headers AND
	// body, the same as an mcp step's args (chain/capture.go bindSaved). `save` is the SAME capture
	// store mcp steps publish into — a later step of EITHER type may read it back.
	// A URL that STARTS with IngestionURLToken (${INGESTION_URL}) takes the host and port of the plain
	// targets.http.base_url in its place — a marker, not an environment variable; a
	// chain cannot name another http target. Any other ${NAME} still unresolved fails the step, named.
	Method string `json:"method"`
	URL    string `json:"url"`
	// Headers is shared with the amqp `publish` op (P3 #24b, custom AMQP message headers) — it is
	// decoded LENIENTLY (raw JSON per value, not map[string]string) so a non-string value is refused
	// BY NAME (HeaderValues) rather than failing the WHOLE step's decode with a generic Go type error
	// that names the field but not which key was wrong.
	Headers map[string]json.RawMessage `json:"headers"`
	Body    json.RawMessage            `json:"body"`
	// Poll (optional): re-send the request until the step's claims all pass, or until Timeout.
	// nil = fire once. A poll with no claim is refused (validate.go) — there would be nothing to
	// decide "pass" with, and the step would just spin for the full timeout every run.
	Poll *ChainPoll `json:"poll"`
	// Always (optional, http and mcp; an amqp step refuses it): this step is the chain's CLEANUP action — it must run even when
	// an earlier step failed, so nothing created by this scenario is left on the SUT. The chain
	// executor already runs every step after a failure (internal/chain/chain.go rule 2 — see
	// continue_past_failure_test.go); Always does NOT reinvent that. What it changes is rule 3: an
	// ordinary step whose `${saved.<var>}` input was never captured is `not-measured` and is NEVER
	// FIRED (a step that cannot be given its inputs was not tested). An `always` step is exempted
	// from that gate — it fires regardless, because "attempt the cleanup, best-effort, even though
	// an earlier step never produced what it usually would have" is exactly the promise this field
	// makes. If the reference really is missing, binding it at RUN time still fails cleanly (naming
	// the variable) rather than sending a literal `${saved.x}` to the SUT.
	Always bool `json:"always"`
	// amqp step (AC-D18b) — one native AMQP operation against a broker, through internal/amqpengine.
	// ⛔ A real agent's inbox can be on the other end, so an amqp step fires ONCE: it may carry
	// neither `poll` nor `always` (validate.go refuses both; the runner refuses them again at
	// preflight) and nothing re-publishes it. Op is publish | consume | declare_queue |
	// declare_exchange | queue_delete | queue_unbind | exchange_delete (P3 #24c). URLEnv is an
	// environment variable NAME holding the broker URL — never the URL itself; the URL is read at
	// run time and never printed. Publish carries exactly one of Envelope (a msgbus v2 envelope,
	// encoded by amqpengine.EncodeEnvelope) or Body (raw bytes: a JSON string is sent unquoted, any
	// other JSON value as its JSON text), plus optional custom Headers (P3 #24b — string values
	// only). UserID sets the AMQP user-id property only when given. Wait bounds a consume (Go
	// duration, default 5s, max 30s). Force (P3 #24c) lets queue_delete/queue_unbind/exchange_delete
	// act on a queue/exchange this CHAIN never declared with a declare_queue/declare_exchange step —
	// refused by default (amqpStepErrors/amqpStepSpec): a check must not delete a live queue by typo.
	// The string fields resolve ${cid}/${cid8}/${correlation_id}/env exactly as an http step's do,
	// and ${saved.<var>} binds at RUN time.
	Op           string         `json:"op"`
	URLEnv       string         `json:"url_env"`
	Exchange     string         `json:"exchange"`
	RoutingKey   string         `json:"routing_key"`
	Queue        string         `json:"queue"`
	ExchangeKind string         `json:"exchange_kind"`
	UserID       string         `json:"user_id"`
	Envelope     *ChainEnvelope `json:"envelope"`
	Wait         string         `json:"wait"`
	Force        bool           `json:"force"`
	// Schema + Record (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28) are the generic,
	// app-declared alternative to Envelope: a publish carries exactly one of `envelope`, `body` or
	// `schema`+`record`; a consume's `schema` is optional and, when present, decodes the delivered
	// body so the body grammar can read its fields (design §5). Schema is a name declared under the
	// app's `message_schemas` (internal/config); Record is the JSON record TEMPLATE, kept as raw
	// JSON here because it is validated/resolved against the schema, not against this struct's shape
	// (internal/argus/chain_scenario.go, internal/chain/amqp.go). `envelope`'s own `record` (design
	// §7) reuses this same field to override the preset's default record — never both nil AND both
	// set for one step: the shape rules live in amqpStepErrors (validate.go's door) and
	// amqpStepSpec's second door (chain_scenario.go), not in the struct.
	Schema string          `json:"schema"`
	Record json.RawMessage `json:"record"`
}

// HeaderValues decodes headers's raw per-value JSON as strings — the shared shape an amqp `publish`
// step's custom headers and an http step's headers both carry (ChainStep.Headers). A header is
// always sent as text, so a value that is not a JSON string is refused BY NAME rather than
// coerced/stringified or left to fail the whole step's decode with no key attached: bad lists the
// offending key names (sorted, for a deterministic message), and ok holds every value that DID
// decode as a string.
func HeaderValues(headers map[string]json.RawMessage) (ok map[string]string, bad []string) {
	ok = make(map[string]string, len(headers))
	for k, v := range headers {
		var s string
		if json.Unmarshal(v, &s) != nil {
			bad = append(bad, k)
			continue
		}
		ok[k] = s
	}
	sort.Strings(bad)
	return ok, bad
}

// ChainEnvelope is an amqp publish step's msgbus v2 envelope (AC-D18b). Everything else the
// envelope carries is fixed by the runner: a fresh messageId, kind MESSAGE, source MCP, originTrust
// AGENT, reply NoReply, no refs, enqueuedAt = now (UTC). CorrelationID defaults to the run's
// correlation id. Intent is the Avro symbol: INFORM (the default when empty), ASK or DECISION_REQUEST.
type ChainEnvelope struct {
	ToInbox       string `json:"to_inbox"`
	FromAgentID   string `json:"from_agent_id"`
	Intent        string `json:"intent"`
	Prompt        string `json:"prompt"`
	CorrelationID string `json:"correlation_id"`
}

// ChainPoll is a chain http step's optional poll block: `{"timeout":"180s","interval":"5s"}`.
// Both fields are Go duration strings (time.ParseDuration): "180s", "3m", "500ms".
type ChainPoll struct {
	Timeout  string `json:"timeout"`
	Interval string `json:"interval"`
}

// ParseChainSteps reads a chain scenario's TRIGGER payload `{"steps":[…]}`.
//
// It does NOT resolve ${…} — an unresolved variable is an ordinary JSON string, so validation reads
// the same structure the runner will, without needing a correlation id it does not have.
func ParseChainSteps(payload string) ([]ChainStep, error) {
	var raw struct {
		Steps []ChainStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, err
	}
	return raw.Steps, nil
}

// chainClaimRe matches `step <name>: <assertion>`. The name runs to the FIRST colon, so an assertion
// may itself contain colons — `- step read: body has error containing "code":"missing"` is one claim
// about the step named `read`, which is the only reading that keeps the grammar usable.
var chainClaimRe = regexp.MustCompile(`(?i)^\s*step\s+([^:]+?)\s*:\s*(\S.*)$`)

// ClaimsToBeChainClaim reports whether a bullet OPENS with the word `step` — i.e. it is claiming to
// be a per-step assertion, whether or not it parses as one. Same contract as
// ClaimsToBeBodyAssert: this decides whose business the bullet is, the parser decides validity.
var chainClaimOpenRe = regexp.MustCompile(`(?i)^\s*step\b`)

func ClaimsToBeChainClaim(bullet string) bool {
	return chainClaimOpenRe.MatchString(bulletText(bullet))
}

// ParseChainClaims reads `- step <name>: <assertion>` bullets into step name -> assertions, in file
// order. Several bullets naming the same step are ANDed, exactly as VR12-E8 rules for body
// assertions.
//
// Names are matched EXACTLY and CASE-SENSITIVELY against the step's `name`. A bullet that opens with
// `step` and does not parse is an error, never a shrug — the whole point of the round.
func ParseChainClaims(bullets []string) (map[string][]string, []error) {
	claims := map[string][]string{}
	var errs []error
	for _, raw := range bullets {
		clause := bulletText(raw)
		if !chainClaimOpenRe.MatchString(clause) {
			continue // not claiming to be a per-step assertion — somebody else's bullet
		}
		m := chainClaimRe.FindStringSubmatch(clause)
		if m == nil {
			errs = append(errs, fmt.Errorf("EXPECT bullet %q looks like a chain claim but matches no known "+
				"form — write `step <name>: <assertion>`, where <name> is a step's `name` from the TRIGGER "+
				"JSON and <assertion> is any form the EXPECT grammar accepts", clause))
			continue
		}
		name := strings.TrimSpace(m[1])
		claims[name] = append(claims[name], strings.TrimSpace(m[2]))
	}
	return claims, errs
}

// SplitChainClaim reads ONE bullet as `step <name>: <assertion>` and reports whether it is one.
//
// ⭐ It exists so tier 3 can tell the TWO unexecuted shapes apart (V31-002 R5): a runnable bullet
// that names no step at all, and one that names a step which is not an `mcp` step. Both are legal in
// a file the validator accepts, and both are judged by nobody — which is exactly what the run report
// must say. It uses the SAME regexes ParseChainClaims does, so the two can never disagree about
// what a claim is.
func SplitChainClaim(bullet string) (name, assertion string, ok bool) {
	clause := bulletText(bullet)
	if !chainClaimOpenRe.MatchString(clause) {
		return "", "", false
	}
	m := chainClaimRe.FindStringSubmatch(clause)
	if m == nil {
		return "", "", false
	}
	return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), true
}

// ChainClaimNames returns the step names a claim map mentions, so a caller can report an orphan.
func ChainClaimNames(claims map[string][]string) []string {
	out := make([]string, 0, len(claims))
	for k := range claims {
		out = append(out, k)
	}
	return out
}

// bulletText strips the markdown list marker and surrounding whitespace from a bullet.
func bulletText(raw string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "-"))
}

// rawChainSteps decodes the TRIGGER payload's steps as raw JSON objects, so the validator can ask
// whether a step carries a key the STRUCT no longer has (V31-002's W1: `expect`). ParseChainSteps
// keeps its plain typed unmarshal; this is the only place that needs to see the wire shape.
//
// It answers nil for anything that does not decode — a payload that is not a chain spec is already
// refused by the caller, and a second copy of that error would double-report one fault.
func rawChainSteps(payload string) []map[string]json.RawMessage {
	var doc struct {
		Steps []map[string]json.RawMessage `json:"steps"`
	}
	if json.Unmarshal([]byte(payload), &doc) != nil {
		return nil
	}
	return doc.Steps
}

// chainStepName is the step's own name, or its 1-based position when it has none — so a refusal can
// always point at a specific step even in a file that forgot to name them.
func chainStepName(raw map[string]json.RawMessage, i int) string {
	var name string
	if v, ok := raw["name"]; ok && json.Unmarshal(v, &name) == nil && name != "" {
		return name
	}
	return fmt.Sprintf("#%d", i+1)
}

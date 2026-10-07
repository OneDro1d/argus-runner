package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AC-D18b — THE "amqp" CHAIN STEP: its claim grammar and its authoring-time shape rules.
//
// ⛔ SAFETY. A publish may land in a REAL agent's inbox. So an amqp step fires exactly once per run:
// it may carry neither `poll` nor `always`, and the runner never retries or re-publishes it. The
// rules below refuse what would break that at seed time, so it never reaches a run.

// URLEnvPattern is the only shape an amqp step's `url_env` may take: an environment variable NAME.
// Anything else — a URL, a `${VAR}`, a lower-case word — is refused, so a credentialed connection
// string can never be written into a scenario file.
const URLEnvPattern = `^[A-Z_][A-Z0-9_]*$`

var urlEnvRe = regexp.MustCompile(URLEnvPattern)

// ValidURLEnv reports whether name is an acceptable `url_env` (an environment variable NAME).
func ValidURLEnv(name string) bool { return urlEnvRe.MatchString(name) }

// AMQPConsumeWaitDefault and AMQPConsumeWaitMax bound an amqp consume step's `wait`.
const (
	AMQPConsumeWaitDefault = 5 * time.Second
	AMQPConsumeWaitMax     = 30 * time.Second
)

// AMQPExchangeKindDefault is the kind a declare_exchange step declares when it names none.
const AMQPExchangeKindDefault = "topic"

// AMQPSchemaHeader is the AMQP header name chain.publishPayload sets on every msgbus envelope
// publish (amqpengine.EncodeEnvelope: x-envelope-schema=2) — reserved out of a publish step's custom
// `headers` (P3 #24b) so an author can never collide with it by accident.
const AMQPSchemaHeader = "x-envelope-schema"

// MessageSchemaRefPattern is the shape a step's `schema:` reference must take — the same pattern a
// message_schemas declaration's own name must match (internal/config/schemas.go's
// MessageSchemaNamePattern; duplicated as a literal, not an import, because internal/scenario may
// not depend on internal/config — see chainStepName's neighbours for the same constraint). This is
// a SHAPE check only: whether the name is actually DECLARED can only be known where the app's
// argus-config is loaded (design §8 — "the control plane's checks are shape-only").
const MessageSchemaRefPattern = `^[a-z0-9][a-z0-9-]*$`

var messageSchemaRefRe = regexp.MustCompile(MessageSchemaRefPattern)

// secretVarNamePattern (design §6, R6) is the ${VAR} NAME shape a record may never carry, at
// authoring or run time, refused by NAME — the value is never read to make this decision.
const secretVarNamePattern = `(?i)(token|secret|password|passwd|key|credential|auth)`

var secretVarNameRe = regexp.MustCompile(secretVarNamePattern)

// recordEnvVarRe finds every `${VAR}` (an env-style placeholder name: letters/digits/underscore,
// starting with a letter or underscore) inside a record's raw, UNRESOLVED JSON text — the same
// token shape argus.resolveVars's varRe matches, duplicated here so a secret-looking name can be
// refused BY NAME before anything ever substitutes it (never call os.Getenv to make this decision).
var recordEnvVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// SecretLikeRecordVars scans a record's raw JSON text (still carrying its literal `${VAR}` tokens —
// call this BEFORE any substitution) and returns every referenced variable name that looks like a
// credential (design §6): `(?i)(token|secret|password|passwd|key|credential|auth)`. `${saved.…}`,
// `${cid}`, `${cid8}`, `${correlation_id}`, `${uuid}`, `${now}` and `${now_ms}` never match (none of
// those words appear in them), so this never has to special-case them.
func SecretLikeRecordVars(recordJSON []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range recordEnvVarRe.FindAllSubmatch(recordJSON, -1) {
		name := string(m[1])
		if secretVarNameRe.MatchString(name) && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// AMQPIntents are the envelope intents an amqp publish step may send (Avro enum symbols).
var AMQPIntents = []string{"INFORM", "ASK", "DECISION_REQUEST"}

var amqpExchangeKinds = map[string]bool{"direct": true, "fanout": true, "topic": true, "headers": true}

// amqpCommonFields are accepted on every amqp step; amqpOpFields adds what each op may carry.
// Every other key is refused by name (the ChainStep struct is shared across mcp/ui/http/amqp, so a
// stray field would otherwise be accepted and silently ignored).
var (
	amqpCommonFields = []string{"type", "name", "op", "url_env"}
	amqpOpFields     = map[string][]string{
		"publish":          {"exchange", "routing_key", "user_id", "envelope", "body", "headers", "schema", "record"},
		"consume":          {"queue", "wait", "schema"},
		"declare_queue":    {"queue"},
		"declare_exchange": {"exchange", "exchange_kind"},
		// P3 #24c — the writes a check needs to clean up what it declared. `force` on all three lets
		// a step act on a name this CHAIN never declared (see amqpStepErrors: refused by default —
		// "a check must not be able to delete a live queue by typo").
		"queue_delete":    {"queue", "force"},
		"queue_unbind":    {"queue", "exchange", "routing_key", "force"},
		"exchange_delete": {"exchange", "force"},
	}
)

// AMQPOps lists the ops an amqp step may name, in a stable order (for messages and docs).
func AMQPOps() []string {
	out := make([]string, 0, len(amqpOpFields))
	for k := range amqpOpFields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── THE CLAIM GRAMMAR — exactly two forms ───────────────────────────────────────────────────────

var (
	brokerClaimOpenRe = regexp.MustCompile(`(?i)^\s*broker\b`)
	brokerAcceptsRe   = regexp.MustCompile(`(?i)^\s*broker\s+accepts\s*$`)
	brokerRefusesRe   = regexp.MustCompile(`(?i)^\s*broker\s+refuses\s+with\s+([0-9]{3})\s*$`)
)

// BrokerClaim is a parsed amqp-step claim: `broker accepts` (Accepts) or `broker refuses with
// <code>` (Code, a 3-digit AMQP reply code such as 403, 404 or 406).
type BrokerClaim struct {
	Accepts bool
	Code    int
}

// String renders the claim in its canonical EXPECT spelling — what assertions_enforced records.
func (b BrokerClaim) String() string {
	if b.Accepts {
		return "broker accepts"
	}
	return "broker refuses with " + strconv.Itoa(b.Code)
}

// ClaimsToBeBrokerClaim reports whether a claim OPENS with the word `broker` — i.e. it is claiming
// to be an amqp-step claim, whether or not it parses as one. Same contract as ClaimsToBeStatus and
// ClaimsToBeBodyAssert: this decides whose business the claim is; ParseBrokerClaim decides validity.
func ClaimsToBeBrokerClaim(claim string) bool {
	return brokerClaimOpenRe.MatchString(claim)
}

// ParseBrokerClaim reads one claim as one of the two broker forms. ok=false for anything else,
// including `broker refuses with 4x6`.
func ParseBrokerClaim(claim string) (BrokerClaim, bool) {
	if brokerAcceptsRe.MatchString(claim) {
		return BrokerClaim{Accepts: true}, true
	}
	if m := brokerRefusesRe.FindStringSubmatch(claim); m != nil {
		code, _ := strconv.Atoi(m[1])
		return BrokerClaim{Code: code}, true
	}
	return BrokerClaim{}, false
}

// AMQPStepClaim turns an amqp step's `## EXPECT` claims into the ONE BrokerClaim the runner judges.
// Every claim must parse; several claims must all be the same claim (two different broker claims
// can never both pass). An empty list is an error too — the empty-step guard refuses it at seed
// time, and the runner refuses it again rather than judging nothing.
func AMQPStepClaim(claims []string) (BrokerClaim, error) {
	var got *BrokerClaim
	for _, c := range claims {
		bc, ok := ParseBrokerClaim(c)
		if !ok {
			return BrokerClaim{}, fmt.Errorf("claim %q is not an amqp-step claim — an amqp step takes exactly "+
				"`broker accepts` or `broker refuses with <code>` (a 3-digit AMQP reply code, e.g. 403, 404, 406)", c)
		}
		if got != nil && *got != bc {
			return BrokerClaim{}, fmt.Errorf("claims %q and %q contradict — an amqp step takes exactly one "+
				"`broker …` claim", got.String(), bc.String())
		}
		got = &bc
	}
	if got == nil {
		return BrokerClaim{}, fmt.Errorf("no claim — an amqp step needs `broker accepts` or `broker refuses with <code>`")
	}
	return *got, nil
}

// ── THE CONSUME-ONLY EXTENSIONS (P3 #24a/d) ─────────────────────────────────────────────────────
//
// A `broker …` claim judges only whether the OPERATION was accepted — it has never been able to say
// anything about what a `consume` actually received. Two gaps followed from that:
//   a. no way to assert on the message BODY once it arrived.
//   d. an empty queue (Consume times out, no message) FAILS `broker accepts` (no message arrived) AND
//      fails every `broker refuses with <code>` (there was no refusal, just silence) — so "the queue
//      is empty" could never be the scenario's PASSING expectation.
//
// Both are consume-only: `queue is empty` and a `body …` assertion describe a DELIVERY, and only
// consume delivers anything. Every other op keeps the exact original two-form grammar (AMQPStepClaim,
// unchanged) — this is additive, not a replacement.

var queueEmptyRe = regexp.MustCompile(`(?i)^\s*queue\s+is\s+empty\s*$`)

// ClaimsToBeQueueEmpty reports whether a claim is the exact `queue is empty` form (VR-AMQP-d).
func ClaimsToBeQueueEmpty(claim string) bool {
	return queueEmptyRe.MatchString(strings.TrimSpace(claim))
}

// AMQPStepWant is everything an amqp step's `## EXPECT` claims resolve to — what chain.AMQPStep
// judges. Broker is always meaningful; Empty and Body are consume-only and mutually exclusive with
// each other (AMQPStepClaims never returns both set).
type AMQPStepWant struct {
	Broker BrokerClaim
	// Empty means the step's WHOLE claim is `queue is empty`: consume must find NO message within
	// its wait. Broker is the zero BrokerClaim{} when this is true — it is not "broker accepts" in
	// disguise, because a genuine broker refusal (e.g. 403 on the queue itself) is a DIFFERENT
	// failure from an empty queue, and judgeBroker tells them apart.
	Empty bool
	// Body assertions on the delivered message (consume only), evaluated only once a message has
	// actually arrived. Reuses the identical grammar/judge http and mcp steps use (mcp.BodyAssert,
	// mcp.BodyAssertsMiss) — VR12-E8's rule is ONE parser, every path, and this is another path.
	Body []BodyAssert
}

// AMQPStepClaims turns an amqp step's `## EXPECT` claims into its AMQPStepWant, given its op.
//
// Every op but `consume` takes EXACTLY the original two `broker …` forms — unchanged, byte-for-byte
// the same messages AMQPStepClaim has always produced, so every existing scenario and test keeps
// meaning exactly what it did. Only `consume` additionally accepts `queue is empty` and `body …`
// assertions, because only consume ever has a delivery to judge.
func AMQPStepClaims(op string, claims []string) (AMQPStepWant, error) {
	if op != "consume" {
		bc, err := AMQPStepClaim(claims)
		if err != nil {
			return AMQPStepWant{}, err
		}
		return AMQPStepWant{Broker: bc}, nil
	}

	var broker *BrokerClaim
	var empty bool
	var body []BodyAssert
	for _, c := range claims {
		switch {
		case ClaimsToBeQueueEmpty(c):
			empty = true
		case ClaimsToBeBrokerClaim(c):
			bc, ok := ParseBrokerClaim(c)
			if !ok {
				return AMQPStepWant{}, fmt.Errorf("claim %q is not an amqp-step claim — a `consume` step takes "+
					"`broker accepts`, `broker refuses with <code>`, `queue is empty`, or a `body …` assertion", c)
			}
			if broker != nil && *broker != bc {
				return AMQPStepWant{}, fmt.Errorf("claims %q and %q contradict — an amqp step takes exactly one "+
					"`broker …` claim", broker.String(), bc.String())
			}
			broker = &bc
		case ClaimsToBeBodyAssert(c):
			asserts, errs := ParseBodyAsserts([]string{c})
			if len(errs) > 0 {
				return AMQPStepWant{}, errs[0]
			}
			body = append(body, asserts...)
		default:
			return AMQPStepWant{}, fmt.Errorf("claim %q is not an amqp-step claim — a `consume` step takes "+
				"`broker accepts`, `broker refuses with <code>`, `queue is empty`, or a `body …` assertion", c)
		}
	}
	switch {
	case empty && len(body) > 0:
		return AMQPStepWant{}, fmt.Errorf("`queue is empty` cannot be combined with a body assertion — an empty " +
			"queue has no message to check")
	case empty && broker != nil:
		return AMQPStepWant{}, fmt.Errorf("`queue is empty` cannot be combined with a `broker …` claim — pick one")
	case empty:
		return AMQPStepWant{Empty: true}, nil
	case len(body) > 0 && broker != nil && !broker.Accepts:
		return AMQPStepWant{}, fmt.Errorf("a body assertion requires the message to arrive; it cannot be combined " +
			"with `broker refuses with <code>`")
	case len(body) > 0:
		// A body assertion IMPLIES a message must arrive — `broker accepts` is the only sensible
		// pairing, and the default a consume-with-content-check needs no explicit broker claim for.
		if broker == nil {
			broker = &BrokerClaim{Accepts: true}
		}
		return AMQPStepWant{Broker: *broker, Body: body}, nil
	case broker != nil:
		return AMQPStepWant{Broker: *broker}, nil
	default:
		return AMQPStepWant{}, fmt.Errorf("no claim — a `consume` step needs `broker accepts`, `broker refuses " +
			"with <code>`, `queue is empty`, or a `body …` assertion")
	}
}

// amqpStepErrors is the AC-D18b block of Validate: the broker-claim rules across EVERY step, and
// the shape rules for each amqp step. trigLine/expLine are the lines the errors point at.
func amqpStepErrors(steps []ChainStep, raw []map[string]json.RawMessage, claims map[string][]string, trigLine, expLine int) []Error {
	var errs []Error
	trig := func(format string, a ...any) {
		errs = append(errs, Error{Line: trigLine, Message: fmt.Sprintf(format, a...) + " (AC-D18b)"})
	}

	// P3 #24c — what THIS chain declared, so a queue_delete/queue_unbind/exchange_delete can be
	// refused when it names something the chain never declared (a check must not delete a live
	// queue/exchange by typo). Any step's position counts, not just an earlier one: the whole chain
	// is known at validate time, unlike a runtime effect.
	declaredQueues := map[string]bool{}
	declaredExchanges := map[string]bool{}
	for _, st := range steps {
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

	for i, st := range steps {
		// A `broker …` claim on a step that is not amqp is judged by nothing that understands it.
		if st.Type != "amqp" {
			for _, c := range claims[st.Name] {
				if ClaimsToBeBrokerClaim(c) {
					errs = append(errs, Error{Line: expLine, Message: fmt.Sprintf("step %q is an `%s` step, but "+
						"its claim %q is a broker claim — a `broker …` claim belongs to an `amqp` step (AC-D18b)",
						st.Name, st.Type, c)})
				}
			}
			continue
		}

		// Claims: exactly the two broker forms (every op but consume), or — consume only — those
		// two PLUS `queue is empty` / a `body …` assertion (P3 #24a/d). (No claim at all is the
		// empty-step guard's message, in validate.go.)
		if len(claims[st.Name]) > 0 {
			if _, err := AMQPStepClaims(st.Op, claims[st.Name]); err != nil {
				errs = append(errs, Error{Line: expLine, Message: fmt.Sprintf("step %q: %s (AC-D18b)", st.Name, err.Error())})
			}
		}

		var keys map[string]json.RawMessage
		if i < len(raw) {
			keys = raw[i]
		}

		// ⛔ THE ONE-PUBLISH RULE. Refused on the key's PRESENCE, whatever its value.
		if _, ok := keys["poll"]; ok || st.Poll != nil {
			trig("step %q is an `amqp` step and may not carry `poll` — an amqp step fires exactly once per "+
				"run and is never retried, because a real agent's inbox may be on the other end", st.Name)
		}
		if _, ok := keys["always"]; ok || st.Always {
			trig("step %q is an `amqp` step and may not carry `always` — an amqp step fires exactly once, "+
				"in order, and is never re-fired as a cleanup", st.Name)
		}

		// url_env: required, and a NAME. The refusal never echoes the value (it may be a credential).
		switch {
		case strings.TrimSpace(st.URLEnv) == "":
			trig("step %q is an `amqp` step but declares no `url_env` — name the environment variable that "+
				"holds the broker URL (e.g. \"MSGBUS_ARGUS_TEST_URL\")", st.Name)
		case !ValidURLEnv(st.URLEnv):
			trig("step %q: `url_env` must be an environment variable NAME matching %s — never a URL or a "+
				"literal; the broker URL is read from that variable at run time and never written into a scenario",
				st.Name, URLEnvPattern)
		default:
			// A field that expands the broker URL would put it into a message or a report.
			ref := []byte("${" + st.URLEnv + "}")
			for k, v := range keys {
				if bytes.Contains(v, ref) {
					trig("step %q: the field %q references its own `url_env` (${%s}) — the broker URL must never "+
						"be sent or printed", st.Name, k, st.URLEnv)
				}
			}
		}

		opFields, known := amqpOpFields[st.Op]
		if !known {
			trig("step %q declares an unknown `op` %q — an amqp step's op is one of: %s", st.Name, st.Op,
				strings.Join(AMQPOps(), ", "))
			continue // the per-op rules below need a known op
		}

		switch st.Op {
		case "publish":
			hasEnv := st.Envelope != nil
			_, hasBody := keys["body"]
			if hasBody && strings.TrimSpace(string(keys["body"])) == "null" {
				hasBody = false
			}
			_, hasSchema := keys["schema"]
			if hasSchema && strings.TrimSpace(string(keys["schema"])) == "null" {
				hasSchema = false
			}
			_, hasRecord := keys["record"]
			if hasRecord && strings.TrimSpace(string(keys["record"])) == "null" {
				hasRecord = false
			}
			// design §2/§7: exactly one of `envelope`, `body`, `schema`+`record` — EXCEPT that
			// `envelope` may carry a `record` too (an override of the preset's default fields,
			// design §7), so envelope+record together is not a second-form violation.
			forms := 0
			if hasEnv {
				forms++
			}
			if hasBody {
				forms++
			}
			if hasSchema {
				forms++
			}
			if forms != 1 {
				trig("step %q is an amqp `publish` and must carry exactly one of `envelope`, `body` or `schema`+`record`", st.Name)
			}
			if !hasEnv && hasSchema != hasRecord {
				trig("step %q declares %s without %s — `schema` and `record` are required together", st.Name,
					map[bool]string{true: "`schema`", false: "`record`"}[hasSchema],
					map[bool]string{true: "`record`", false: "`schema`"}[hasSchema])
			}
			if hasSchema && !messageSchemaRefRe.MatchString(st.Schema) {
				trig("step %q: `schema` %q is not a valid schema reference (must match %s)", st.Name, st.Schema, MessageSchemaRefPattern)
			}
			if hasRecord {
				if bad := SecretLikeRecordVars(keys["record"]); len(bad) > 0 {
					trig("step %q: `record` references ${%s}, which looks like a credential — a record may never carry a secret; put it in a header from the executor's own Secret instead", st.Name, bad[0])
				}
			}
			if strings.TrimSpace(st.RoutingKey) == "" {
				trig("step %q is an amqp `publish` but declares no `routing_key`", st.Name)
			}
			if hasEnv {
				e := st.Envelope
				for _, f := range []struct{ name, val string }{
					{"to_inbox", e.ToInbox}, {"from_agent_id", e.FromAgentID}, {"prompt", e.Prompt},
				} {
					if strings.TrimSpace(f.val) == "" {
						trig("step %q: `envelope.%s` is required", st.Name, f.name)
					}
				}
				if e.Intent != "" && !contains(AMQPIntents, e.Intent) {
					trig("step %q: `envelope.intent` %q is not one of %s (empty means INFORM)", st.Name, e.Intent,
						strings.Join(AMQPIntents, ", "))
				}
			}
			// P3 #24b — custom publish headers, string values only (reject non-string types by
			// name): a broker header is always sent as text, so a JSON number/bool/object/array/null
			// is refused rather than coerced or silently stringified.
			hdrs, bad := HeaderValues(st.Headers)
			for _, k := range bad {
				trig("step %q: publish header %q must be a string value — a broker header is always sent as text", st.Name, k)
			}
			// x-envelope-schema is the wire marker chain.publishPayload sets on EVERY msgbus envelope
			// publish (amqpengine.EncodeEnvelope) — a custom header of the same name would collide
			// with it and corrupt how the message decodes on the other end.
			for k := range hdrs {
				if strings.EqualFold(k, AMQPSchemaHeader) {
					trig("step %q: publish header %q is reserved — the msgbus envelope encoder sets it "+
						"itself; a custom value would corrupt how the message decodes", st.Name, k)
				}
			}
		case "consume":
			if strings.TrimSpace(st.Queue) == "" {
				trig("step %q is an amqp `consume` but declares no `queue`", st.Name)
			}
			if st.Wait != "" {
				w, err := time.ParseDuration(st.Wait)
				switch {
				case err != nil:
					trig("step %q declares `wait` %q, which is not a valid duration (Go syntax: \"5s\")", st.Name, st.Wait)
				case w <= 0:
					trig("step %q declares `wait` %s, which must be positive", st.Name, w)
				case w > AMQPConsumeWaitMax:
					trig("step %q declares `wait` of %s, which is more than the %s bound on an amqp consume",
						st.Name, w, AMQPConsumeWaitMax)
				}
			}
			// design §5: `schema` is optional on consume (raw-bytes behaviour is unchanged
			// without it) — shape-only check here, the same as publish's.
			if _, ok := keys["schema"]; ok && strings.TrimSpace(string(keys["schema"])) != "null" {
				if !messageSchemaRefRe.MatchString(st.Schema) {
					trig("step %q: `schema` %q is not a valid schema reference (must match %s)", st.Name, st.Schema, MessageSchemaRefPattern)
				}
			}
		case "declare_queue":
			if strings.TrimSpace(st.Queue) == "" {
				trig("step %q is an amqp `declare_queue` but declares no `queue`", st.Name)
			}
		case "declare_exchange":
			if strings.TrimSpace(st.Exchange) == "" {
				trig("step %q is an amqp `declare_exchange` but declares no `exchange`", st.Name)
			}
			if st.ExchangeKind != "" && !amqpExchangeKinds[st.ExchangeKind] {
				trig("step %q: `exchange_kind` %q is not one of direct, fanout, headers, topic", st.Name, st.ExchangeKind)
			}
		// P3 #24c — cleanup ops for what a check declared. Each refuses, BY DEFAULT, a name this
		// chain never declared with its own declare_queue/declare_exchange step: a check must not be
		// able to delete a live queue/exchange by typo. `force: true` bypasses the guard explicitly.
		case "queue_delete":
			if strings.TrimSpace(st.Queue) == "" {
				trig("step %q is an amqp `queue_delete` but declares no `queue`", st.Name)
			} else if !st.Force && !declaredQueues[st.Queue] {
				trig("step %q would delete queue %q, which no `declare_queue` step in this chain declares — "+
					"a check must not be able to delete a live queue by typo; add a `declare_queue` step for it, "+
					"or set `force: true` on this step to delete it anyway", st.Name, st.Queue)
			}
		case "queue_unbind":
			if strings.TrimSpace(st.Queue) == "" {
				trig("step %q is an amqp `queue_unbind` but declares no `queue`", st.Name)
			}
			if strings.TrimSpace(st.Exchange) == "" {
				trig("step %q is an amqp `queue_unbind` but declares no `exchange`", st.Name)
			}
			if strings.TrimSpace(st.RoutingKey) == "" {
				trig("step %q is an amqp `queue_unbind` but declares no `routing_key`", st.Name)
			}
			if !st.Force {
				if st.Queue != "" && !declaredQueues[st.Queue] {
					trig("step %q would unbind queue %q, which no `declare_queue` step in this chain declares — "+
						"set `force: true` on this step to unbind it anyway", st.Name, st.Queue)
				}
				if st.Exchange != "" && !declaredExchanges[st.Exchange] {
					trig("step %q would unbind from exchange %q, which no `declare_exchange` step in this chain "+
						"declares — set `force: true` on this step to unbind it anyway", st.Name, st.Exchange)
				}
			}
		case "exchange_delete":
			if strings.TrimSpace(st.Exchange) == "" {
				trig("step %q is an amqp `exchange_delete` but declares no `exchange`", st.Name)
			} else if !st.Force && !declaredExchanges[st.Exchange] {
				trig("step %q would delete exchange %q, which no `declare_exchange` step in this chain declares — "+
					"a check must not be able to delete a live exchange by typo; add a `declare_exchange` step for "+
					"it, or set `force: true` on this step to delete it anyway", st.Name, st.Exchange)
			}
		}

		// Any key this op does not take is refused by name (`poll`/`always` already have their own,
		// more specific, message above).
		allowed := map[string]bool{"poll": true, "always": true}
		for _, k := range amqpCommonFields {
			allowed[k] = true
		}
		for _, k := range opFields {
			allowed[k] = true
		}
		var stray []string
		for k := range keys {
			if !allowed[k] {
				stray = append(stray, k)
			}
		}
		sort.Strings(stray)
		for _, k := range stray {
			trig("step %q is an amqp `%s` step and carries the field %q, which that op does not accept", st.Name, st.Op, k)
		}
	}
	return errs
}

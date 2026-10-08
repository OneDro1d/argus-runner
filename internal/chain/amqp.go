package chain

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
	"github.com/hamba/avro/v2"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/avroschema"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D18b — THE "amqp" CHAIN STEP: one native AMQP operation (publish / consume / declare_queue /
// declare_exchange / queue_delete / queue_unbind / exchange_delete, P3 #24c) against a real broker,
// through internal/amqpengine, judged by a scenario.AMQPStepWant: the one `broker accepts` /
// `broker refuses with <code>` claim every op takes, plus — consume only — `queue is empty` (P3
// #24d) or a body assertion on the delivered message (P3 #24a).
//
// ⛔ NO PARALLEL EXECUTOR. Like HTTPStep, this is a Step built for the SAME chain.Run: the same
// continue-past-failure, the same not-measured gate for an unresolved ${saved.<var>}, the same
// VR12-CH2 stop when the broker cannot be reached at all (a connect failure is StatusError).
//
// ⛔ THE ONE-PUBLISH RULE. A publish may land in a REAL agent's inbox. The step body has NO LOOP:
// it dials once, performs its one operation once, and returns — whatever the claim says. It is
// never `Always` (a cleanup re-fire), and it carries no poll. The Broker interface below does not
// even offer amqpengine's fire-and-forget Publish: a publish here is always PublishConfirmed, so a
// refusal the broker raises asynchronously is observed rather than lost.
//
// ⛔ THE URL NEVER SURFACES. The step holds only the environment variable NAME; the dialer reads
// the value. Every text that reaches a StepResult passes through scrubBroker first.

// Broker is the slice of amqpengine an amqp step drives. *amqpengine.Engine satisfies it; tests
// substitute a fake, so the runner is exercised without a RabbitMQ.
type Broker interface {
	PublishConfirmed(exchange, routingKey string, body []byte, props amqpengine.Props) amqpengine.Outcome
	Consume(queue string, timeout time.Duration) (amqpengine.Outcome, []byte)
	DeclareQueue(name string) amqpengine.Outcome
	DeclareExchange(name, kind string) amqpengine.Outcome
	// DeleteQueue / UnbindQueue / DeleteExchange (P3 #24c) — the cleanup ops for what a check
	// declared. Guarded the same way as every other amqp write: refused at preflight (P3 #24c's
	// declared-name guard) before any of these is ever called.
	DeleteQueue(name string) amqpengine.Outcome
	UnbindQueue(queue, exchange, routingKey string) amqpengine.Outcome
	DeleteExchange(name string) amqpengine.Outcome
	Close() error
}

// BrokerDialer opens a Broker from an environment variable NAME (never a URL).
type BrokerDialer func(urlEnv string) (Broker, error)

// DialAMQP is the production BrokerDialer: amqpengine.Connect, which reads the URL from urlEnv and
// keeps no copy of it.
func DialAMQP(urlEnv string) (Broker, error) {
	e, err := amqpengine.Connect(urlEnv)
	if err != nil {
		return nil, err // never a typed-nil *Engine inside a non-nil interface
	}
	return e, nil
}

// AMQPEnvelope is an amqp publish step's msgbus envelope, its strings already resolved for
// ${cid}/${cid8}/${correlation_id}/env (a ${saved.<var>} still binds at run time). CorrelationID ""
// means "the run's correlation id"; Intent "" means INFORM.
type AMQPEnvelope struct {
	ToInbox, FromAgentID, Intent, Prompt, CorrelationID string
}

// AMQPSpec is one amqp step as the runner resolved it (chain_scenario.go).
type AMQPSpec struct {
	Op, URLEnv, Exchange, RoutingKey, Queue, ExchangeKind, UserID string
	// Envelope, Body and (Schema+RecordTemplate) are the three (mutually exclusive, design §2)
	// publish payloads. HasBody tells an empty raw body apart from no body; BodyContentType is the
	// content-type sent with a raw body.
	Envelope        *AMQPEnvelope
	Body            string
	HasBody         bool
	BodyContentType string
	// Schema + RecordTemplate (ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28) are the generic
	// schema-driven publish/consume path (design §2/§5). RecordTemplate is the record TEMPLATE as a
	// generic JSON tree (map[string]any/[]any/…), with every whole-value `${…}` leaf already wrapped
	// by avroschema.Placeholder (internal/argus/chain_scenario.go: parsed from the step's PRISTINE
	// `record` — never textually pre-substituted, unlike every other amqp-step field, precisely so
	// this wrapping survives). ConsumeSchema is set independently (a consume step names `schema`
	// without ever carrying a `record`) — kept as a separate field so a publish and a consume never
	// have to agree on which one a given AMQPSpec means.
	Schema         *avroschema.Schema
	RecordTemplate any
	ConsumeSchema  *avroschema.Schema
	// Headers (P3 #24b) are custom AMQP message headers a publish sends alongside its payload —
	// string values only (scenario.HeaderValues refuses the rest by name). nil/empty means none.
	Headers map[string]string
	// Wait bounds a consume; 0 means scenario.AMQPConsumeWaitDefault.
	Wait time.Duration
	// Force (P3 #24c) lets queue_delete/queue_unbind/exchange_delete act on a name this chain never
	// declared with its own declare_queue/declare_exchange step — the guard argus.amqpStepSpec
	// applies BEFORE building this spec, so by the time Run sees Force==true the refusal has
	// already been decided; this field exists only so tests can build a spec directly.
	Force bool
}

// strings returns pointers to every string field a ${saved.<var>} may appear in — one list, so
// Needs (can it be bound?) and Run (bind it) can never disagree about which fields are bound.
func (s *AMQPSpec) strings() []*string {
	out := []*string{&s.Exchange, &s.RoutingKey, &s.Queue, &s.ExchangeKind, &s.UserID, &s.Body}
	if s.Envelope != nil {
		e := s.Envelope
		out = append(out, &e.ToInbox, &e.FromAgentID, &e.Intent, &e.Prompt, &e.CorrelationID)
	}
	return out
}

// bound returns a COPY of spec with every ${saved.<var>} bound — the input is never mutated, so a
// re-built chain never sees one execution's values. Headers is deep-copied (a fresh map) and each of
// its values is bound too, the same as every other string field.
func (s AMQPSpec) bound(vars map[string]string) (AMQPSpec, error) {
	out := s
	if s.Envelope != nil {
		env := *s.Envelope
		out.Envelope = &env
	}
	for _, p := range out.strings() {
		v, err := bindSaved(*p, vars)
		if err != nil {
			return AMQPSpec{}, err
		}
		*p = v
	}
	if len(s.Headers) > 0 {
		hdrs := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			bv, err := bindSaved(v, vars)
			if err != nil {
				return AMQPSpec{}, err
			}
			hdrs[k] = bv
		}
		out.Headers = hdrs
	}
	return out, nil
}

// AMQPStep builds the amqp step. want is the step's claim (scenario.AMQPStepClaims); dial opens the
// connection (DialAMQP in production). One connection per step, closed before the step returns.
func AMQPStep(name string, spec AMQPSpec, want scenario.AMQPStepWant, dial BrokerDialer) Step {
	return Step{
		Name: name,
		// Always is deliberately left false: an amqp step is never re-fired as a cleanup.
		Needs: func(vars map[string]string) error {
			if _, err := spec.bound(vars); err != nil {
				return err
			}
			// a consume's body claims may carry ${saved.<var>} too.
			return needsBodyAsserts(want.Body, vars)
		},
		Run: func(cid string, vars map[string]string) report.StepResult {
			sp, err := spec.bound(vars)
			if err != nil {
				// An authoring error — never send a literal ${saved.x} to a broker.
				return report.StepResult{Status: "failed", Observed: err.Error()}
			}
			// Bound BEFORE the broker is touched: nothing is published or consumed for a claim that
			// cannot be bound (unsaved variable, or a saved threshold that is not a number — by name).
			boundBody, cerr := bindBodyAsserts(want.Body, vars)
			if cerr != nil {
				return report.StepResult{Status: "failed", Observed: "content assertion: " + cerr.Error()}
			}
			br, derr := dial(sp.URLEnv)
			if derr != nil {
				// No broker to talk to: an EXECUTION error (VR12-CH2 stops the chain), never a refusal.
				return report.StepResult{Status: report.StatusError,
					Observed: "amqp connect (url from $" + sp.URLEnv + ") failed: " + scrubBroker(derr.Error(), sp.URLEnv)}
			}
			defer func() { _ = br.Close() }()

			out, body, what, perr := perform(br, sp, cid, vars)
			if perr != nil {
				// The step could not even be put on the wire (an envelope that will not encode):
				// nothing was sent, so nothing was checked.
				return report.StepResult{Status: "failed", Observed: scrubBroker(perr.Error(), sp.URLEnv)}
			}
			return judgeBroker(out, body, want, boundBody, vars, what, sp.URLEnv, sp.ConsumeSchema)
		},
	}
}

// perform does the step's ONE broker operation. No loop, no retry: whatever the broker answers is
// the answer. The second return is the delivered message body — populated only for `consume`, so
// judgeBroker can evaluate a body assertion (P3 #24a), confirm emptiness (P3 #24d), or decode it
// against sp.ConsumeSchema (design §5). vars is threaded through to publishPayload ONLY for a
// schema+record publish, which needs ${saved.<var>} at the SAME run-time resolution moment
// ${uuid}/${now}/${now_ms} are (design §4) — every other publish form never needed it here, because
// bound() (above) already resolved their plain string fields before perform was ever called.
func perform(br Broker, sp AMQPSpec, cid string, vars map[string]string) (amqpengine.Outcome, []byte, string, error) {
	switch sp.Op {
	case "publish":
		body, props, err := publishPayload(sp, cid, vars)
		if err != nil {
			return amqpengine.Outcome{}, nil, "", err
		}
		what := fmt.Sprintf("publish to exchange %q with routing key %q", sp.Exchange, sp.RoutingKey)
		return br.PublishConfirmed(sp.Exchange, sp.RoutingKey, body, props), nil, what, nil
	case "consume":
		wait := sp.Wait
		if wait <= 0 {
			wait = scenario.AMQPConsumeWaitDefault
		}
		out, body := br.Consume(sp.Queue, wait)
		return out, body, fmt.Sprintf("consume from queue %q", sp.Queue), nil
	case "declare_queue":
		return br.DeclareQueue(sp.Queue), nil, fmt.Sprintf("declare queue %q", sp.Queue), nil
	case "declare_exchange":
		kind := sp.ExchangeKind
		if kind == "" {
			kind = scenario.AMQPExchangeKindDefault
		}
		return br.DeclareExchange(sp.Exchange, kind), nil, fmt.Sprintf("declare %s exchange %q", kind, sp.Exchange), nil
	// P3 #24c — the cleanup ops. The declared-name guard already ran (argus.amqpStepSpec /
	// scenario.amqpStepErrors) before this spec was ever built; perform never re-decides it.
	case "queue_delete":
		return br.DeleteQueue(sp.Queue), nil, fmt.Sprintf("delete queue %q", sp.Queue), nil
	case "queue_unbind":
		return br.UnbindQueue(sp.Queue, sp.Exchange, sp.RoutingKey), nil,
			fmt.Sprintf("unbind queue %q from exchange %q (routing key %q)", sp.Queue, sp.Exchange, sp.RoutingKey), nil
	case "exchange_delete":
		return br.DeleteExchange(sp.Exchange), nil, fmt.Sprintf("delete exchange %q", sp.Exchange), nil
	}
	return amqpengine.Outcome{}, nil, "", fmt.Errorf("unknown amqp op %q", sp.Op)
}

// publishPayload builds the body and properties of a publish: a msgbus v2 envelope (fixed
// messageId/kind/source/trust/reply/refs/enqueuedAt, the run's correlation id unless the envelope
// names one — design §7's preset, with any `record` override merged over the defaults BEFORE
// resolution), a generic schema+record (design §2), or a raw body. user-id is set ONLY when the
// step gives one. Custom Headers (P3 #24b) are layered in last, on every form — scenario.Validate
// already refuses one named scenario.AMQPSchemaHeader (or a declared schema's own headers, design
// §1), so this never has to choose who wins.
func publishPayload(sp AMQPSpec, cid string, vars map[string]string) ([]byte, amqpengine.Props, error) {
	props := amqpengine.Props{UserID: sp.UserID}
	switch {
	case sp.Envelope != nil:
		return publishEnvelope(sp, cid, vars, props)
	case sp.Schema != nil:
		return publishSchemaRecord(sp, cid, vars, props)
	default:
		props.ContentType = sp.BodyContentType
		props.CorrelationID = cid
		if len(sp.Headers) > 0 {
			props.Headers = headersToAny(sp.Headers)
		}
		return []byte(sp.Body), props, nil
	}
}

// publishSchemaRecord is the generic path (design §2): resolve sp.RecordTemplate's placeholders,
// build+type-check the avro-native tree against sp.Schema, encode, and set content-type + the
// schema's own declaration headers (design §1) before layering the step's custom Headers on top.
func publishSchemaRecord(sp AMQPSpec, cid string, vars map[string]string, props amqpengine.Props) ([]byte, amqpengine.Props, error) {
	native, err := buildRecordForRun(sp.Schema, sp.RecordTemplate, cid, vars)
	if err != nil {
		return nil, amqpengine.Props{}, fmt.Errorf("record does not fit schema %q: %w", sp.Schema.Name, err)
	}
	body, err := avro.Marshal(sp.Schema.Avro(), native)
	if err != nil {
		return nil, amqpengine.Props{}, fmt.Errorf("record does not encode against schema %q: %w", sp.Schema.Name, err)
	}
	props.ContentType = sp.Schema.ContentType
	props.CorrelationID = cid
	props.Persistent = true
	if len(sp.Schema.Headers) > 0 {
		props.Headers = headersToAny(sp.Schema.Headers)
	}
	for k, v := range sp.Headers {
		if props.Headers == nil {
			props.Headers = map[string]any{}
		}
		props.Headers[k] = v // scenario.Validate already refuses a key colliding with a declaration header
	}
	return body, props, nil
}

// publishEnvelope is the `envelope` preset (design §7): schema:msgbus-envelope-v2 plus a default
// record built from the four legacy fields, with sp.RecordTemplate (the step's own `record`, if
// any) merged over the defaults at the TOP LEVEL before resolution — "every other field
// overridable". Still the one place that imports the in-repo msgbus format,
// internal/msgenvelope (design: "the only app-specific code left, and only the preset uses it").
func publishEnvelope(sp AMQPSpec, cid string, vars map[string]string, props amqpengine.Props) ([]byte, amqpengine.Props, error) {
	e := sp.Envelope
	corr := e.CorrelationID
	if corr == "" {
		corr = cid
	}
	intent := string(envelope.IntentInform)
	if e.Intent != "" {
		intent = e.Intent
	}
	defaults := map[string]any{
		"messageId":       newUUID(),
		"kind":            string(envelope.KindMessage),
		"source":          string(envelope.SourceMCP),
		"originTrust":     string(envelope.TrustAgent),
		"fromAgentId":     e.FromAgentID,
		"toInbox":         e.ToInbox,
		"prompt":          e.Prompt,
		"reply":           map[string]any{msgbusV2NS + ".NoReply": map[string]any{}},
		"refs":            []any{},
		"correlationId":   corr,
		"enqueuedAt":      time.Now().UTC().UnixMilli(),
		"expiredOriginal": nil,
		"intent":          intent,
	}
	merged := any(defaults)
	if sp.RecordTemplate != nil {
		override, ok := sp.RecordTemplate.(map[string]any)
		if !ok {
			return nil, amqpengine.Props{}, fmt.Errorf("envelope `record` override must be an object")
		}
		for k, v := range override {
			defaults[k] = v
		}
		merged = defaults
	}
	native, err := buildRecordForRun(msgbusEnvelopeSchema(), merged, cid, vars)
	if err != nil {
		return nil, amqpengine.Props{}, fmt.Errorf("envelope record does not fit the msgbus v2 schema: %w", err)
	}
	body, err := avro.Marshal(msgbusEnvelopeSchema().Avro(), native)
	if err != nil {
		return nil, amqpengine.Props{}, fmt.Errorf("the envelope does not encode as a msgbus v2 message: %w", err)
	}
	props.ContentType = "application/avro"
	props.Persistent = true
	props.Headers = map[string]any{envelope.HeaderSchema: envelope.SchemaVersion2}
	for k, v := range sp.Headers {
		props.Headers[k] = v // scenario.Validate already refuses a key colliding with the schema marker above
	}
	props.CorrelationID = corr
	return body, props, nil
}

// msgbusV2NS is the namespace SchemaJSONV2 declares (internal/msgenvelope) — needed to build
// the tagged union value for the `reply` field's default (design §3: a 2+-branch union's JSON form
// tags with the branch's FULL qualified name).
const msgbusV2NS = "argus.envelope.v2"

var msgbusSchemaOnce = struct {
	s   *avroschema.Schema
	err error
}{}

// msgbusEnvelopeSchema parses envelope.SchemaJSONV2, Argus's in-repo copy of the msgbus schema
// (design §7: "still taken from the msgbus package"; the copy replaced the import) exactly once — a package-level lazy singleton rather than a sync.Once/init
// panic, since a parse error here should surface as this STEP's failure, never a process-wide panic
// at import time.
func msgbusEnvelopeSchema() *avroschema.Schema {
	if msgbusSchemaOnce.s == nil && msgbusSchemaOnce.err == nil {
		msgbusSchemaOnce.s, msgbusSchemaOnce.err = avroschema.Parse("msgbus-envelope-v2", "application/avro",
			map[string]string{envelope.HeaderSchema: envelope.SchemaVersion2}, envelope.SchemaJSONV2)
		if msgbusSchemaOnce.err != nil {
			// envelope.go itself does avro.MustParse(SchemaJSONV2) at package init — if THAT ever
			// panics, this line is unreachable; this branch exists only so a future edit to
			// SchemaJSONV2 that somehow still passes hamba's own parse but not avroschema.Parse's
			// (currently identical) checks fails loud here rather than nil-dereferencing below.
			panic("msgbus envelope.SchemaJSONV2 failed to parse: " + msgbusSchemaOnce.err.Error())
		}
	}
	return msgbusSchemaOnce.s
}

// headersToAny widens a publish step's custom headers (string values, P3 #24b) to the
// map[string]any amqpengine.Props.Headers takes — amqp091-go's Table accepts any AMQP field type,
// but every header this step ever sends is text.
func headersToAny(h map[string]string) map[string]any {
	out := make(map[string]any, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// bodyPreviewMax bounds how much of a delivered message judgeBroker will show in Observed when a
// `queue is empty` claim fails because a message DID arrive (P3 #24d) — reality, not the test's
// asserted value (VR-C8), but still bounded: an unbounded echo of a live message is its own leak.
const bodyPreviewMax = 500

// judgeBroker applies the step's claim (scenario.AMQPStepClaims) to the broker's answer — and, for a
// consume, to what it delivered. assertions_enforced records the claim on pass AND fail — it was
// always evaluated. Observed is reality-only: what the broker did/delivered, never the asserted
// value (VR-C8) — a body-assertion miss says only that it missed, and a `queue is empty` miss shows
// what ARRIVED (that is reality, not the test's expectation) but never echoes the claim's own value.
//
// boundBody is want.Body with ${saved.<var>} bound (bindBodyAsserts): it is what the judge compares;
// want.Body — the claim as written — is what assertions_enforced shows, so a saved value is never printed.
//
// a step that failed on its claims also carries FailedClaims — each claim that did not
// hold, as written, with what the broker showed (author-only, see report.StepResult.FailedClaims).
func judgeBroker(out amqpengine.Outcome, body []byte, want scenario.AMQPStepWant, boundBody []mcp.BodyAssert, vars map[string]string, what, urlEnv string, consumeSchema *avroschema.Schema) report.StepResult {
	var observed string
	switch {
	case out.OK:
		observed = "broker accepted the " + what
	case out.Code != 0:
		observed = fmt.Sprintf("broker refused the %s with %d: %s", what, out.Code, scrubBroker(out.Text, urlEnv))
	default:
		observed = "broker did not accept the " + what + " (no AMQP reply code): " + scrubBroker(out.Text, urlEnv)
	}

	// P3 #24d — `queue is empty`: the ONLY claim form an empty consume can PASS. A timeout with no
	// AMQP reply code (out.Code == 0, !out.OK) IS emptiness; a genuine broker refusal (403 on the
	// queue itself) is a DIFFERENT failure and keeps the "broker refused" observed text above.
	if want.Empty {
		pass := !out.OK && out.Code == 0
		if out.OK {
			preview := scrubBroker(string(body), urlEnv)
			if len(preview) > bodyPreviewMax {
				preview = preview[:bodyPreviewMax] + "... (truncated)"
			}
			observed = "a message arrived on the queue, but the step's claim is `queue is empty`: " + preview
		}
		st := report.StepResult{Status: "failed", Observed: observed,
			AssertionsEnforced: []string{"queue is empty"}, AssertionsEnforcedCount: 1}
		if pass {
			st.Status, st.Observed = "passed", "no message arrived on the queue within the wait — queue is empty"
		} else if out.OK {
			st.FailedClaims = []report.FailedClaim{{Claim: "queue is empty",
				Observed: report.TruncateObserved(scrubBroker(string(body), urlEnv))}}
		} else {
			// a genuine broker refusal (out.Code != 0): the claim did not hold, and `observed` says why
			st.FailedClaims = []report.FailedClaim{{Claim: "queue is empty", Observed: report.TruncateObserved(observed)}}
		}
		return st
	}

	// design §5: `schema` on consume decodes the delivered body and renders it as the §3 JSON view,
	// so the EXISTING body grammar (below) runs on the DECODED fields — no new assertion language.
	// A body that fails to decode is its OWN failure, never reported as a content mismatch (design:
	// "delivered message does not decode as <schema>: <reason>"), whether or not the step even
	// declared a body assertion — `schema:` itself is the promise this decodes.
	if out.OK && consumeSchema != nil {
		var native map[string]any
		if derr := avro.Unmarshal(consumeSchema.Avro(), body, &native); derr != nil {
			return report.StepResult{Status: "failed",
				Observed:           fmt.Sprintf("delivered message does not decode as %s: %s", consumeSchema.Name, scrubBroker(derr.Error(), urlEnv)),
				AssertionsEnforced: []string{want.Broker.String()}, AssertionsEnforcedCount: 1}
		}
		if view, verr := avroschema.ToJSON(consumeSchema)(native); verr == nil {
			if j, jerr := json.Marshal(view); jerr == nil {
				body = j
			}
		}
	}

	pass := out.OK
	if !want.Broker.Accepts {
		pass = !out.OK && out.Code == want.Broker.Code
	}
	enforced := []string{want.Broker.String()}
	// P3 #24a — a body assertion is evaluated ONLY once the message has arrived (want.Body is never
	// non-empty alongside a `broker refuses` claim — scenario.AMQPStepClaims refuses that pairing at
	// authoring time), so `pass` here is exactly "did the message arrive".
	var failed []report.FailedClaim
	failedOmitted := 0 // claims past the list's backstop (author-only, never in observed)
	if !pass {
		// the broker claim itself did not hold (body claims are never evaluated then)
		failed = []report.FailedClaim{{Claim: want.Broker.String(), Observed: report.TruncateObserved(observed)}}
	}
	if pass && len(want.Body) > 0 {
		enforced = append(enforced, enforcedAssertions(mcp.Expect{Body: want.Body})...)
		if mcp.BodyAssertsMiss(string(body), boundBody) {
			pass = false
			observed = "broker accepted the " + what + ", but the delivered message did not satisfy the " +
				"scenario's content assertion " + chainClaimsNote
			failed, failedOmitted = failedBodyClaims(string(body), want.Body, boundBody, vars, func(s string) string { return scrubBroker(s, urlEnv) })
		}
	}
	st := report.StepResult{Status: "failed", Observed: observed,
		AssertionsEnforced: enforced, AssertionsEnforcedCount: len(enforced)}
	if pass {
		st.Status = "passed"
	} else {
		st.FailedClaims = failed
		st.FailedClaimsOmitted = failedOmitted
	}
	return st
}

// scrubBroker removes the broker URL — and its credentials — from any text before it can reach a
// step result, a report or a log: the whole value of $urlEnv, then its password on its own, then
// any remaining `scheme://userinfo@` (amqpengine.RedactURL).
func scrubBroker(text, urlEnv string) string {
	if raw := os.Getenv(urlEnv); raw != "" {
		text = strings.ReplaceAll(text, raw, "<$"+urlEnv+">")
		if u, err := url.Parse(raw); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				text = strings.ReplaceAll(text, pw, "REDACTED")
			}
		}
	}
	return amqpengine.RedactURL(text)
}

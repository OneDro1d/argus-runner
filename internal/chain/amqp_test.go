package chain

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D18b — the "amqp" chain step, driven against a FAKE broker (the Broker interface), so no
// RabbitMQ is needed. What is pinned here: the two claim forms judged exactly; the envelope that
// reaches the broker; user-id set only when given; ONE publish per step even when its claim fails;
// and the broker URL/credential never surfacing in a step result or the report.

type publishCall struct {
	exchange, routingKey string
	body                 []byte
	props                amqpengine.Props
}

type fakeBroker struct {
	mu            sync.Mutex
	outcome       amqpengine.Outcome
	consumeBody   []byte
	publishes     []publishCall
	consumes      []string
	queues        []string
	exchanges     [][2]string
	deletedQueues []string
	unbinds       [][3]string
	deletedExchgs []string
	closed        int
}

func (f *fakeBroker) PublishConfirmed(exchange, routingKey string, body []byte, props amqpengine.Props) amqpengine.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishes = append(f.publishes, publishCall{exchange, routingKey, body, props})
	return f.outcome
}

func (f *fakeBroker) Consume(queue string, _ time.Duration) (amqpengine.Outcome, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumes = append(f.consumes, queue)
	return f.outcome, f.consumeBody
}

func (f *fakeBroker) DeclareQueue(name string) amqpengine.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queues = append(f.queues, name)
	return f.outcome
}

func (f *fakeBroker) DeclareExchange(name, kind string) amqpengine.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges = append(f.exchanges, [2]string{name, kind})
	return f.outcome
}

func (f *fakeBroker) DeleteQueue(name string) amqpengine.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedQueues = append(f.deletedQueues, name)
	return f.outcome
}

func (f *fakeBroker) UnbindQueue(queue, exchange, routingKey string) amqpengine.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unbinds = append(f.unbinds, [3]string{queue, exchange, routingKey})
	return f.outcome
}

func (f *fakeBroker) DeleteExchange(name string) amqpengine.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedExchgs = append(f.deletedExchgs, name)
	return f.outcome
}

func (f *fakeBroker) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func dialerFor(f *fakeBroker) (BrokerDialer, *[]string) {
	var envs []string
	return func(urlEnv string) (Broker, error) {
		envs = append(envs, urlEnv)
		return f, nil
	}, &envs
}

func publishSpec() AMQPSpec {
	return AMQPSpec{
		Op: "publish", URLEnv: "MSGBUS_ARGUS_TEST_URL",
		Exchange: "msgbus.inbox", RoutingKey: "inbox.agent.hop",
		Envelope: &AMQPEnvelope{ToInbox: "agent.hop", FromAgentID: "argus-test", Intent: "ASK", Prompt: "argus-test: hello"},
	}
}

var (
	accepts    = scenario.BrokerClaim{Accepts: true}
	refuses403 = scenario.BrokerClaim{Code: 403}
	refuses406 = scenario.BrokerClaim{Code: 406}
)

// runOne takes a plain scenario.BrokerClaim — the shape every existing test in this file already
// builds — and wraps it as the Broker half of an AMQPStepWant. runOneWant is the extended form for
// the consume-only claims (P3 #24a/d: a body assertion or `queue is empty`).
func runOne(t *testing.T, spec AMQPSpec, want scenario.BrokerClaim, f *fakeBroker) report.StepResult {
	t.Helper()
	return runOneWant(t, spec, scenario.AMQPStepWant{Broker: want}, f)
}

func runOneWant(t *testing.T, spec AMQPSpec, want scenario.AMQPStepWant, f *fakeBroker) report.StepResult {
	t.Helper()
	dial, _ := dialerFor(f)
	res := Run("tr-amqp-1", []Step{AMQPStep("spoof", spec, want, dial)})
	if len(res.Steps) != 1 {
		t.Fatalf("want exactly one step result, got %+v", res.Steps)
	}
	return res.Steps[0]
}

func TestAMQPStep_AcceptedPublish(t *testing.T) {
	t.Run("passes `broker accepts`", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		sr := runOne(t, publishSpec(), accepts, f)
		if sr.Status != "passed" {
			t.Fatalf("an accepted publish must pass `broker accepts`: %+v", sr)
		}
		if len(sr.AssertionsEnforced) != 1 || sr.AssertionsEnforced[0] != "broker accepts" || sr.AssertionsEnforcedCount != 1 {
			t.Fatalf("assertions_enforced must record the claim: %+v", sr)
		}
	})
	t.Run("fails `broker refuses with 406`", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		sr := runOne(t, publishSpec(), refuses406, f)
		if sr.Status != "failed" {
			t.Fatalf("an accepted publish must fail `broker refuses with 406`: %+v", sr)
		}
		if !strings.Contains(sr.Observed, "accepted") {
			t.Fatalf("observed must say the broker accepted: %q", sr.Observed)
		}
	})
}

func TestAMQPStep_RefusedPublish406(t *testing.T) {
	out := amqpengine.Outcome{OK: false, Code: 406, Text: "PRECONDITION_FAILED - user_id property set to 'hop' but authenticated user was 'argus-test'"}
	t.Run("passes `broker refuses with 406`", func(t *testing.T) {
		sr := runOne(t, publishSpec(), refuses406, &fakeBroker{outcome: out})
		if sr.Status != "passed" {
			t.Fatalf("a 406 must pass `broker refuses with 406`: %+v", sr)
		}
		if len(sr.AssertionsEnforced) != 1 || sr.AssertionsEnforced[0] != "broker refuses with 406" {
			t.Fatalf("assertions_enforced: %+v", sr.AssertionsEnforced)
		}
	})
	t.Run("fails `broker refuses with 403`, showing the observed code and text", func(t *testing.T) {
		sr := runOne(t, publishSpec(), refuses403, &fakeBroker{outcome: out})
		if sr.Status != "failed" {
			t.Fatalf("a 406 must fail `broker refuses with 403`: %+v", sr)
		}
		if !strings.Contains(sr.Observed, "406") || !strings.Contains(sr.Observed, "PRECONDITION_FAILED") {
			t.Fatalf("observed must show the code and the (redacted) text: %q", sr.Observed)
		}
		if len(sr.AssertionsEnforced) != 1 || sr.AssertionsEnforced[0] != "broker refuses with 403" {
			t.Fatalf("assertions_enforced must record what was checked, pass or fail: %+v", sr.AssertionsEnforced)
		}
	})
	t.Run("fails `broker accepts`", func(t *testing.T) {
		sr := runOne(t, publishSpec(), accepts, &fakeBroker{outcome: out})
		if sr.Status != "failed" {
			t.Fatalf("a 406 must fail `broker accepts`: %+v", sr)
		}
	})
}

func TestAMQPStep_EnvelopeReachingTheBroker(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	runOne(t, publishSpec(), accepts, f)
	if len(f.publishes) != 1 {
		t.Fatalf("want one publish, got %d", len(f.publishes))
	}
	p := f.publishes[0]
	if p.exchange != "msgbus.inbox" || p.routingKey != "inbox.agent.hop" {
		t.Fatalf("exchange/routing key: %q %q", p.exchange, p.routingKey)
	}
	m, err := amqpengine.DecodeEnvelope(p.body, p.props.Headers)
	if err != nil {
		t.Fatalf("the body must decode as a msgbus envelope: %v", err)
	}
	if m.Prompt != "argus-test: hello" || m.ToInbox != "agent.hop" || m.FromAgentID != "argus-test" || m.Intent != envelope.IntentAsk {
		t.Fatalf("envelope fields: %+v", m)
	}
	if m.Kind != envelope.KindMessage || m.Source != envelope.SourceMCP || m.OriginTrust != envelope.TrustAgent ||
		m.Reply.Mode != envelope.ReplyNone || len(m.Refs) != 0 || m.MessageID == "" {
		t.Fatalf("fixed envelope fields: %+v", m)
	}
	if m.CorrelationID != "tr-amqp-1" || p.props.CorrelationID != "tr-amqp-1" {
		t.Fatalf("correlation id defaults to the run's (envelope %q, prop %q)", m.CorrelationID, p.props.CorrelationID)
	}
	if time.Since(m.EnqueuedAt) > time.Minute || m.EnqueuedAt.Location() != time.UTC {
		t.Fatalf("enqueuedAt must be now, UTC: %v", m.EnqueuedAt)
	}
	if p.props.ContentType != "application/avro" || !p.props.Persistent {
		t.Fatalf("props: %+v", p.props)
	}
}

func TestAMQPStep_EnvelopeCorrelationIDOverride(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	spec := publishSpec()
	spec.Envelope.CorrelationID = "corr-given"
	runOne(t, spec, accepts, f)
	onlyPublish(t, f)
	m, err := amqpengine.DecodeEnvelope(f.publishes[0].body, f.publishes[0].props.Headers)
	if err != nil {
		t.Fatal(err)
	}
	if m.CorrelationID != "corr-given" || f.publishes[0].props.CorrelationID != "corr-given" {
		t.Fatalf("a given correlation_id wins on the envelope AND the property: %q / %q", m.CorrelationID, f.publishes[0].props.CorrelationID)
	}
}

func TestAMQPStep_UserIDOnlyWhenGiven(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	runOne(t, publishSpec(), accepts, f)
	onlyPublish(t, f)
	if f.publishes[0].props.UserID != "" {
		t.Fatalf("user-id must NOT be set when the step gives none: %q", f.publishes[0].props.UserID)
	}
	g := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	spec := publishSpec()
	spec.UserID = "hop"
	runOne(t, spec, accepts, g)
	onlyPublish(t, g)
	if g.publishes[0].props.UserID != "hop" {
		t.Fatalf("user-id must be set when the step gives one: %q", g.publishes[0].props.UserID)
	}
}

// ⛔ THE ONE-PUBLISH RULE, at the executor: a failing claim is never retried or re-published.
func TestAMQPStep_PublishExactlyOnceEvenWhenClaimFails(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: false, Code: 403, Text: "ACCESS_REFUSED"}}
	dial, envs := dialerFor(f)
	res := Run("tr-amqp-once", []Step{AMQPStep("spoof", publishSpec(), scenario.AMQPStepWant{Broker: accepts}, dial)})
	if res.Status != "failed" {
		t.Fatalf("the claim must fail: %+v", res)
	}
	if len(f.publishes) != 1 {
		t.Fatalf("PublishConfirmed must be called EXACTLY once, got %d", len(f.publishes))
	}
	if len(*envs) != 1 || (*envs)[0] != "MSGBUS_ARGUS_TEST_URL" || f.closed != 1 {
		t.Fatalf("one connection from the url_env NAME, and it is closed: dials=%v closed=%d", *envs, f.closed)
	}
}

func TestAMQPStep_RawBody(t *testing.T) {
	f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
	spec := AMQPSpec{Op: "publish", URLEnv: "X", Exchange: "e", RoutingKey: "rk", Body: "raw ${saved.id}", HasBody: true, BodyContentType: "text/plain"}
	dial, _ := dialerFor(f)
	res := Run("tr-body", []Step{
		{Name: "seed", Run: func(string, map[string]string) report.StepResult {
			return report.StepResult{Status: "passed", Captured: map[string]string{"id": "42"}}
		}},
		AMQPStep("raw", spec, scenario.AMQPStepWant{Broker: accepts}, dial),
	})
	if res.Status != "passed" || len(f.publishes) != 1 {
		t.Fatalf("raw body publish: %+v publishes=%d", res, len(f.publishes))
	}
	if string(f.publishes[0].body) != "raw 42" || f.publishes[0].props.ContentType != "text/plain" {
		t.Fatalf("${saved.id} binds into a raw body at run time: %q %+v", f.publishes[0].body, f.publishes[0].props)
	}
}

func TestAMQPStep_OtherOps(t *testing.T) {
	t.Run("consume", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{Code: 403, Text: "ACCESS_REFUSED"}}
		sr := runOne(t, AMQPSpec{Op: "consume", URLEnv: "X", Queue: "msgbus.agent.hop", Wait: time.Second}, refuses403, f)
		if sr.Status != "passed" || len(f.consumes) != 1 || f.consumes[0] != "msgbus.agent.hop" || len(f.publishes) != 0 {
			t.Fatalf("consume: %+v %+v", sr, f)
		}
	})
	t.Run("declare_queue", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{Code: 403}}
		sr := runOne(t, AMQPSpec{Op: "declare_queue", URLEnv: "X", Queue: "q1"}, refuses403, f)
		if sr.Status != "passed" || len(f.queues) != 1 || f.queues[0] != "q1" {
			t.Fatalf("declare_queue: %+v %+v", sr, f)
		}
	})
	t.Run("declare_exchange defaults to topic", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		sr := runOne(t, AMQPSpec{Op: "declare_exchange", URLEnv: "X", Exchange: "x1"}, accepts, f)
		if sr.Status != "passed" || len(f.exchanges) != 1 || f.exchanges[0] != [2]string{"x1", "topic"} {
			t.Fatalf("declare_exchange: %+v %+v", sr, f)
		}
	})
	// P3 #24c — the cleanup ops. Guard refusal is decided BEFORE Run (argus.amqpStepSpec /
	// scenario.amqpStepErrors); these prove the ops themselves reach the right Broker method and are
	// judged by the same broker claim every other op is.
	t.Run("queue_delete", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		sr := runOne(t, AMQPSpec{Op: "queue_delete", URLEnv: "X", Queue: "q1"}, accepts, f)
		if sr.Status != "passed" || len(f.deletedQueues) != 1 || f.deletedQueues[0] != "q1" {
			t.Fatalf("queue_delete: %+v %+v", sr, f)
		}
	})
	t.Run("queue_unbind", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		sr := runOne(t, AMQPSpec{Op: "queue_unbind", URLEnv: "X", Queue: "q1", Exchange: "x1", RoutingKey: "rk1"}, accepts, f)
		if sr.Status != "passed" || len(f.unbinds) != 1 || f.unbinds[0] != [3]string{"q1", "x1", "rk1"} {
			t.Fatalf("queue_unbind: %+v %+v", sr, f)
		}
	})
	t.Run("exchange_delete", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{Code: 404, Text: "NOT_FOUND"}}
		sr := runOne(t, AMQPSpec{Op: "exchange_delete", URLEnv: "X", Exchange: "x1"}, scenario.BrokerClaim{Code: 404}, f)
		if sr.Status != "passed" || len(f.deletedExchgs) != 1 || f.deletedExchgs[0] != "x1" {
			t.Fatalf("exchange_delete: %+v %+v", sr, f)
		}
	})
}

// P3 #24a — a consume can now assert on the delivered message body, not just whether one arrived.
func TestAMQPStep_ConsumeBodyClaim(t *testing.T) {
	spec := func() AMQPSpec { return AMQPSpec{Op: "consume", URLEnv: "X", Queue: "q", Wait: time.Second} }
	body := mcp.BodyAssert{Field: "status", Op: mcp.BodyEqualsOp, Value: "ok"}

	t.Run("passes when the delivered message satisfies the assertion", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`{"status":"ok"}`)}
		sr := runOneWant(t, spec(), scenario.AMQPStepWant{Broker: accepts, Body: []mcp.BodyAssert{body}}, f)
		if sr.Status != "passed" {
			t.Fatalf("a matching delivered body must pass: %+v", sr)
		}
		if len(sr.AssertionsEnforced) != 2 || sr.AssertionsEnforced[0] != "broker accepts" ||
			sr.AssertionsEnforced[1] != `field status equals "ok"` {
			t.Fatalf("assertions_enforced must record both the broker claim and the body claim: %+v", sr.AssertionsEnforced)
		}
	})
	t.Run("fails when the delivered message does not, and never echoes the asserted value", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`{"status":"error","detail":"reveal-code-42"}`)}
		sr := runOneWant(t, spec(), scenario.AMQPStepWant{Broker: accepts, Body: []mcp.BodyAssert{body}}, f)
		if sr.Status != "failed" {
			t.Fatalf("a non-matching delivered body must fail: %+v", sr)
		}
		if strings.Contains(sr.Observed, "reveal-code-42") || strings.Contains(sr.Observed, "\"ok\"") {
			t.Fatalf("VR-C8: Observed must never echo the delivered content or the asserted value: %q", sr.Observed)
		}
	})
	t.Run("the existing broker-only forms are unaffected by an empty want.Body", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("anything")}
		sr := runOneWant(t, spec(), scenario.AMQPStepWant{Broker: accepts}, f)
		if sr.Status != "passed" {
			t.Fatalf("broker accepts alone, with no body claim, must behave exactly as before: %+v", sr)
		}
	})
}

// P3 #24d — `queue is empty` is the one claim an empty consume can PASS.
func TestAMQPStep_QueueIsEmptyClaim(t *testing.T) {
	spec := AMQPSpec{Op: "consume", URLEnv: "X", Queue: "q", Wait: time.Second}
	want := scenario.AMQPStepWant{Empty: true}

	t.Run("passes on a timeout with no message", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: false, Code: 0, Text: "timed out waiting for a message"}}
		sr := runOneWant(t, spec, want, f)
		if sr.Status != "passed" {
			t.Fatalf("an empty queue must PASS `queue is empty`: %+v", sr)
		}
		if len(sr.AssertionsEnforced) != 1 || sr.AssertionsEnforced[0] != "queue is empty" {
			t.Fatalf("assertions_enforced: %+v", sr.AssertionsEnforced)
		}
	})
	t.Run("fails, showing what arrived, when a message actually arrives", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("surprise-delivery")}
		sr := runOneWant(t, spec, want, f)
		if sr.Status != "failed" {
			t.Fatalf("a delivered message must FAIL `queue is empty`: %+v", sr)
		}
		if !strings.Contains(sr.Observed, "surprise-delivery") {
			t.Fatalf("observed must show what arrived (reality, not the claim's value): %q", sr.Observed)
		}
	})
	t.Run("a genuine broker refusal on the queue is a DIFFERENT failure from emptiness", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: false, Code: 403, Text: "ACCESS_REFUSED"}}
		sr := runOneWant(t, spec, want, f)
		if sr.Status != "failed" {
			t.Fatalf("a 403 must fail `queue is empty` too (it is not silence): %+v", sr)
		}
		if !strings.Contains(sr.Observed, "403") || strings.Contains(sr.Observed, "no message arrived") {
			t.Fatalf("observed must show the REFUSAL, not the empty-queue wording: %q", sr.Observed)
		}
	})
}

// P3 #24b — custom publish headers, on both payload forms.
func TestAMQPStep_PublishCustomHeaders(t *testing.T) {
	t.Run("envelope publish: custom headers ride alongside the schema marker", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		spec := publishSpec()
		spec.Headers = map[string]string{"x-retry-count": "3"}
		runOne(t, spec, accepts, f)
		onlyPublish(t, f)
		hdrs := f.publishes[0].props.Headers
		if hdrs["x-retry-count"] != "3" {
			t.Fatalf("custom header must reach the broker: %+v", hdrs)
		}
		if _, ok := hdrs["x-envelope-schema"]; !ok {
			t.Fatalf("the envelope's own schema header must survive alongside the custom one: %+v", hdrs)
		}
	})
	t.Run("raw body publish: custom headers are sent even with no envelope", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		spec := AMQPSpec{Op: "publish", URLEnv: "X", Exchange: "e", RoutingKey: "rk", Body: "x", HasBody: true,
			Headers: map[string]string{"x-source": "argus"}}
		runOne(t, spec, accepts, f)
		onlyPublish(t, f)
		if f.publishes[0].props.Headers["x-source"] != "argus" {
			t.Fatalf("custom header on a raw-body publish must reach the broker: %+v", f.publishes[0].props.Headers)
		}
	})
	t.Run("${saved.<var>} binds in a header value at run time", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{OK: true}}
		spec := publishSpec()
		spec.Headers = map[string]string{"x-order-id": "${saved.id}"}
		res := Run("tr-hdr", []Step{
			{Name: "seed", Run: func(string, map[string]string) report.StepResult {
				return report.StepResult{Status: "passed", Captured: map[string]string{"id": "ord-9"}}
			}},
			AMQPStep("spoof", spec, scenario.AMQPStepWant{Broker: accepts}, func(string) (Broker, error) { return f, nil }),
		})
		if res.Status != "passed" || len(f.publishes) != 1 {
			t.Fatalf("run: %+v publishes=%d", res, len(f.publishes))
		}
		if f.publishes[0].props.Headers["x-order-id"] != "ord-9" {
			t.Fatalf("${saved.id} must bind inside a header value: %+v", f.publishes[0].props.Headers)
		}
	})
}

// The broker URL and its credential never reach a step result or the report — neither from a
// connect error nor from an Outcome.Text.
func TestAMQPStep_CredentialNeverInResult(t *testing.T) {
	const leak = "amqp://user:secret@broker.internal:5672/vhost"
	t.Setenv("ARGUS_TEST_LEAKY_URL", leak)

	t.Run("connect error", func(t *testing.T) {
		dial := func(string) (Broker, error) { return nil, errors.New("dial " + leak + ": connection refused") }
		res := Run("tr-leak", []Step{AMQPStep("spoof", AMQPSpec{Op: "declare_queue", URLEnv: "ARGUS_TEST_LEAKY_URL", Queue: "q"}, scenario.AMQPStepWant{Broker: accepts}, dial)})
		assertNoSecret(t, res)
		if res.Steps[0].Status != report.StepNotMeasured || res.Status != report.StatusError {
			t.Fatalf("an unreachable broker is not-measured / error, like an unreachable SUT: %+v", res)
		}
	})
	t.Run("outcome text", func(t *testing.T) {
		f := &fakeBroker{outcome: amqpengine.Outcome{Code: 403, Text: "refused on " + leak + " password secret"}}
		dial, _ := dialerFor(f)
		res := Run("tr-leak", []Step{AMQPStep("spoof", AMQPSpec{Op: "declare_queue", URLEnv: "ARGUS_TEST_LEAKY_URL", Queue: "q"}, scenario.AMQPStepWant{Broker: accepts}, dial)})
		assertNoSecret(t, res)
		if !strings.Contains(res.Steps[0].Observed, "403") {
			t.Fatalf("the code still shows: %q", res.Steps[0].Observed)
		}
	})
}

func onlyPublish(t *testing.T, f *fakeBroker) {
	t.Helper()
	if len(f.publishes) != 1 {
		t.Fatalf("want exactly one publish, got %d", len(f.publishes))
	}
}

func assertNoSecret(t *testing.T, res report.ScenarioResult) {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"secret", "user:", "broker.internal"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("%q leaked into the result/report: %s", bad, b)
		}
	}
}

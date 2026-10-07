package argus

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/chain"
	"github.com/OneDro1d/argus-runner/internal/config"
	envelope "github.com/OneDro1d/argus-runner/internal/msgenvelope"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D18b — the "amqp" chain step through the WHOLE chain path: scenario.Parse -> runChainScenario
// (claims, ${correlation_id} resolution, preflight) -> chain.AMQPStep -> chain.Run, with a fake
// broker behind dialAMQPBroker, so no RabbitMQ is needed.

type argusFakeBroker struct {
	mu          sync.Mutex
	outcome     amqpengine.Outcome
	bodies      [][]byte
	props       []amqpengine.Props
	other       int
	dialed      []string
	closed      int
	dialError   error
	consumeBody []byte // TestAMQPChain_ConsumeBodyClaimResolvesCid8 — the delivered message a `consume` returns
}

func (f *argusFakeBroker) PublishConfirmed(_, _ string, body []byte, props amqpengine.Props) amqpengine.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, body)
	f.props = append(f.props, props)
	return f.outcome
}
func (f *argusFakeBroker) Consume(string, time.Duration) (amqpengine.Outcome, []byte) {
	f.other++
	return f.outcome, f.consumeBody
}
func (f *argusFakeBroker) DeclareQueue(string) amqpengine.Outcome { f.other++; return f.outcome }
func (f *argusFakeBroker) DeclareExchange(string, string) amqpengine.Outcome {
	f.other++
	return f.outcome
}
func (f *argusFakeBroker) DeleteQueue(string) amqpengine.Outcome { f.other++; return f.outcome }
func (f *argusFakeBroker) UnbindQueue(string, string, string) amqpengine.Outcome {
	f.other++
	return f.outcome
}
func (f *argusFakeBroker) DeleteExchange(string) amqpengine.Outcome { f.other++; return f.outcome }
func (f *argusFakeBroker) Close() error                             { f.closed++; return nil }

// withFakeBroker swaps dialAMQPBroker for the test's lifetime.
func withFakeBroker(t *testing.T, f *argusFakeBroker) {
	t.Helper()
	prev := dialAMQPBroker
	dialAMQPBroker = func(urlEnv string) (chain.Broker, error) {
		f.dialed = append(f.dialed, urlEnv)
		if f.dialError != nil {
			return nil, f.dialError
		}
		return f, nil
	}
	t.Cleanup(func() { dialAMQPBroker = prev })
}

const spoofTrigger = `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"exchange":"msgbus.inbox","routing_key":"inbox.agent.hop","user_id":"hop",` +
	`"envelope":{"to_inbox":"agent.hop","from_agent_id":"argus-test","intent":"DECISION_REQUEST","prompt":"argus-test: ${correlation_id}"}}]}`

func TestAMQPChain_SpoofRefusedAndEnvelopeResolved(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{Code: 406, Text: "PRECONDITION_FAILED - user_id property set to 'hop'"}}
	withFakeBroker(t, f)
	s := scenario.Parse(chainRunMD("", spoofTrigger, "### Runnable\n- step spoof: broker refuses with 406\n"))
	res := runChainScenario(&config.Config{}, s, "tr-amqp-e2e", "testkit/ui")
	if res.Status != "passed" {
		t.Fatalf("a 406 must pass `broker refuses with 406`: %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	if len(f.bodies) != 1 || len(f.dialed) != 1 || f.dialed[0] != "MSGBUS_ARGUS_TEST_URL" || f.closed != 1 {
		t.Fatalf("one dial by NAME, one publish, one close: dialed=%v bodies=%d closed=%d", f.dialed, len(f.bodies), f.closed)
	}
	m, err := amqpengine.DecodeEnvelope(f.bodies[0], f.props[0].Headers)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.Prompt != "argus-test: tr-amqp-e2e" {
		t.Fatalf("${correlation_id} must resolve in the prompt: %q", m.Prompt)
	}
	if m.Intent != envelope.IntentDecisionRequest || m.ToInbox != "agent.hop" || m.FromAgentID != "argus-test" || m.CorrelationID != "tr-amqp-e2e" {
		t.Fatalf("envelope: %+v", m)
	}
	if f.props[0].UserID != "hop" {
		t.Fatalf("user_id prop: %q", f.props[0].UserID)
	}
	if got := res.Steps[0].AssertionsEnforced; len(got) != 1 || got[0] != "broker refuses with 406" {
		t.Fatalf("assertions_enforced: %v", got)
	}
}

func TestAMQPChain_FailingClaimPublishesOnceAndNeverLeaksTheURL(t *testing.T) {
	const leak = "amqp://argus:secret@broker.internal:5672/"
	t.Setenv("MSGBUS_ARGUS_TEST_URL", leak)
	f := &argusFakeBroker{outcome: amqpengine.Outcome{Code: 403, Text: "ACCESS_REFUSED via " + leak}}
	withFakeBroker(t, f)
	s := scenario.Parse(chainRunMD("", spoofTrigger, "### Runnable\n- step spoof: broker refuses with 406\n"))
	res := runChainScenario(&config.Config{}, s, "tr-amqp-once", "testkit/ui")
	if res.Status != "failed" {
		t.Fatalf("a 403 must fail `broker refuses with 406`: %+v", res)
	}
	if len(f.bodies) != 1 {
		t.Fatalf("PublishConfirmed must be called EXACTLY once, got %d", len(f.bodies))
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "argus:") {
		t.Fatalf("the URL/credential leaked into the report: %s", b)
	}
	if !strings.Contains(res.Failure.Observed, "403") {
		t.Fatalf("the report shows the observed code: %q", res.Failure.Observed)
	}
	if res.Failure.Expected == nil || *res.Failure.Expected != "broker refuses with 406" {
		t.Fatalf("failure.expected carries the claim (test hat): %+v", res.Failure.Expected)
	}
}

func TestAMQPChain_ConnectFailureIsNotMeasuredAndRedacted(t *testing.T) {
	f := &argusFakeBroker{dialError: errors.New("amqpengine: dial: dial tcp amqp://u:secret@h:5672: refused")}
	withFakeBroker(t, f)
	s := scenario.Parse(chainRunMD("", spoofTrigger, "### Runnable\n- step spoof: broker refuses with 406\n"))
	res := runChainScenario(&config.Config{}, s, "tr-amqp-down", "testkit/ui")
	if res.Status != report.StatusError || len(f.bodies) != 0 {
		t.Fatalf("an unreachable broker is an execution error, nothing published: %+v", res)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("credential leaked: %s", b)
	}
}

// ⛔ Defence in depth: a file that slipped past validation with poll/always is refused at
// preflight and NOTHING is published.
func TestAMQPChain_PreflightRefusesPollAlwaysAndForeignClaims(t *testing.T) {
	cases := map[string]struct{ trig, expect, want string }{
		"poll": {strings.Replace(spoofTrigger, `"op":"publish",`, `"op":"publish","poll":{"timeout":"5s","interval":"1s"},`, 1),
			"### Runnable\n- step spoof: broker refuses with 406\n", "poll"},
		"always": {strings.Replace(spoofTrigger, `"op":"publish",`, `"op":"publish","always":true,`, 1),
			"### Runnable\n- step spoof: broker refuses with 406\n", "always"},
		"non-broker claim": {spoofTrigger, "### Runnable\n- step spoof: status=200\n", "not an amqp-step claim"},
		"url_env literal": {strings.Replace(spoofTrigger, `"MSGBUS_ARGUS_TEST_URL"`, `"amqp://u:p@h/"`, 1),
			"### Runnable\n- step spoof: broker refuses with 406\n", "url_env"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
			withFakeBroker(t, f)
			res := runChainScenario(&config.Config{}, scenario.Parse(chainRunMD("", c.trig, c.expect)), "tr-pf", "testkit/ui")
			if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, c.want) ||
				!strings.Contains(res.Failure.Observed, "preflight") {
				t.Fatalf("want a preflight refusal naming %q: %+v", c.want, res.Failure)
			}
			if len(f.bodies) != 0 || len(f.dialed) != 0 {
				t.Fatalf("nothing may be dialed or published: %+v", f)
			}
			if strings.Contains(res.Failure.Observed, "u:p@") {
				t.Fatalf("the refusal echoed the literal: %q", res.Failure.Observed)
			}
		})
	}
}

// ⛔ The run loop re-fires a rate-limited scenario once (pacer.handle). A chain carrying an amqp
// step must NEVER be re-fired — that would re-publish into a real inbox.
func TestAMQPChain_NeverRefiredOnRateLimit(t *testing.T) {
	withAMQP := scenario.Parse(chainRunMD("", `{"steps":[{"type":"mcp","name":"a","tool":"t","args":{}},`+
		`{"type":"amqp","name":"b","op":"declare_queue","url_env":"X","queue":"q"}]}`,
		"### Runnable\n- step a: result.isError == false\n- step b: broker refuses with 403\n"))
	if mayRefire(withAMQP) {
		t.Fatal("a chain with an amqp step must not be re-fired after a rate-limit")
	}
	mcpOnly := scenario.Parse(chainRunMD("", `{"steps":[{"type":"mcp","name":"a","tool":"t","args":{}}]}`,
		"### Runnable\n- step a: result.isError == false\n"))
	if !mayRefire(mcpOnly) {
		t.Fatal("a chain with no amqp step keeps today's single re-fire")
	}
}

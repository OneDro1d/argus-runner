package argus

// AC-D18b integration test: the amqp chain step against a REAL RabbitMQ, through the CHAIN path —
// scenario.Parse -> runChainScenario -> chain.AMQPStep -> chain.DialAMQP -> amqpengine — not
// amqpengine alone. It reproduces internal/amqpengine/integration_test.go's refusal cases as ONE
// chain scenario, so it also proves the chain continues past each (expected) refusal.
//
// Broker fixture: identical to internal/amqpengine/integration_test.go — a restricted user mirroring
// msgbus's "argus-test" worker identity:
//
//	vhost configure: ^$                                     (deny-all)
//	vhost write:     ^(msgbus\.inbox|msgbus\.presence)$
//	vhost read:      ^(msgbus\.agent\.argus-test|msgbus\.presence\.stream)$
//	topic write on msgbus.inbox: ^inbox\.agent\.hop$
//	topic read on msgbus.inbox:  ^$
//
// Skipped unless ARGUS_TEST_AMQP_URL (the restricted user's URL) is set; ARGUS_TEST_AMQP_ADMIN_URL
// (an administrator on the same throwaway broker) must be set alongside it, to provision the
// topology and read the allowed publish back.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// amqpIntegrationFixture is the chain scenario the integration test runs: amqpengine's four refusal
// cases (spoofed user_id 406, routing key inbox.all 403, consume msgbus.agent.hop 403, declare
// queue 403) and one allowed publish (accepted), in one chain.
func amqpIntegrationFixture() string {
	env := `"envelope":{"to_inbox":"agent.hop","from_agent_id":"argus-test","intent":"INFORM","prompt":"argus-test AC-D18b: ${correlation_id}"}`
	trig := `{"steps":[
		{"type":"amqp","name":"spoof","op":"publish","url_env":"ARGUS_TEST_AMQP_URL","exchange":"msgbus.inbox","routing_key":"inbox.agent.hop","user_id":"someone-else",` + env + `},
		{"type":"amqp","name":"all","op":"publish","url_env":"ARGUS_TEST_AMQP_URL","exchange":"msgbus.inbox","routing_key":"inbox.all","user_id":"argus-test",` + env + `},
		{"type":"amqp","name":"steal","op":"consume","url_env":"ARGUS_TEST_AMQP_URL","queue":"msgbus.agent.hop","wait":"5s"},
		{"type":"amqp","name":"declare","op":"declare_queue","url_env":"ARGUS_TEST_AMQP_URL","queue":"msgbus.agent.spoof-probe-ac-d18b"},
		{"type":"amqp","name":"send","op":"publish","url_env":"ARGUS_TEST_AMQP_URL","exchange":"msgbus.inbox","routing_key":"inbox.agent.hop",` + env + `}
	]}`
	expect := "### Runnable\n" +
		"- step spoof: broker refuses with 406\n" +
		"- step all: broker refuses with 403\n" +
		"- step steal: broker refuses with 403\n" +
		"- step declare: broker refuses with 403\n" +
		"- step send: broker accepts\n"
	return chainRunMD("", trig, expect)
}

// Not env-gated: the integration fixture is a scenario the validator accepts, so a broker run can
// only fail on the broker's answers, never on the fixture's shape.
func TestAMQPChain_IntegrationFixtureIsValid(t *testing.T) {
	if _, errs := scenario.Validate(amqpIntegrationFixture()); len(errs) != 0 {
		t.Fatalf("the integration fixture must validate: %v", errs)
	}
}

func TestAMQPChain_Integration(t *testing.T) {
	url := os.Getenv("ARGUS_TEST_AMQP_URL")
	if url == "" {
		t.Skip("ARGUS_TEST_AMQP_URL not set — skipping AC-D18b amqp chain-step integration test")
	}
	adminURL := os.Getenv("ARGUS_TEST_AMQP_ADMIN_URL")
	if adminURL == "" {
		t.Fatal("ARGUS_TEST_AMQP_ADMIN_URL must be set alongside ARGUS_TEST_AMQP_URL (same setup script exports both)")
	}
	const (
		exchange = "msgbus.inbox"
		hopQueue = "msgbus.agent.hop"
		allowed  = "inbox.agent.hop"
	)

	adminConn, err := amqp.Dial(adminURL)
	if err != nil {
		t.Fatalf("admin dial failed: %s", amqpengine.RedactURL(err.Error()))
	}
	defer adminConn.Close()
	setup, err := adminConn.Channel()
	if err != nil {
		t.Fatalf("admin channel: %v", err)
	}
	if err := setup.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		t.Fatalf("declare %s: %v", exchange, err)
	}
	if _, err := setup.QueueDeclare(hopQueue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare %s: %v", hopQueue, err)
	}
	if err := setup.QueueBind(hopQueue, allowed, exchange, false, nil); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := setup.QueuePurge(hopQueue, false); err != nil {
		t.Fatalf("purge: %v", err)
	}
	setup.Close()

	md := amqpIntegrationFixture()
	if _, errs := scenario.Validate(md); len(errs) != 0 {
		t.Fatalf("the fixture must be a valid scenario: %v", errs)
	}

	const corr = "tr-ac-d18b-integration"
	res := runChainScenario(&config.Config{}, scenario.Parse(md), corr, "testkit/ui")
	for _, st := range res.Steps {
		t.Logf("step %s: %s — %s", st.Name, st.Status, st.Observed)
	}
	if res.Status != "passed" {
		t.Fatalf("every refusal case and the allowed publish must pass their claims: %q %+v", res.Status, res.Failure)
	}

	b, _ := json.Marshal(res)
	if strings.Contains(string(b), url) || strings.Contains(string(b), adminURL) {
		t.Fatal("a broker URL leaked into the report")
	}

	// The allowed publish was actually delivered, exactly once, with ${correlation_id} resolved.
	ch, err := adminConn.Channel()
	if err != nil {
		t.Fatalf("admin channel: %v", err)
	}
	defer ch.Close()
	deliveries, err := ch.ConsumeWithContext(context.Background(), hopQueue, "", false, false, false, false, nil)
	if err != nil {
		t.Fatalf("admin consume: %v", err)
	}
	select {
	case d := <-deliveries:
		_ = d.Ack(false)
		m, derr := amqpengine.DecodeEnvelope(d.Body, map[string]any(d.Headers))
		if derr != nil {
			t.Fatalf("decode: %v", derr)
		}
		if m.Prompt != "argus-test AC-D18b: "+corr || m.CorrelationID != corr || m.FromAgentID != "argus-test" {
			t.Fatalf("delivered envelope: %+v", m)
		}
		if d.UserId != "" {
			t.Fatalf("user-id must not be set when the step gives none: %q", d.UserId)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the allowed publish was never delivered")
	}
	select {
	case d := <-deliveries:
		_ = d.Ack(false)
		t.Fatalf("a second message reached %s — the one-publish rule is broken (or a refused publish landed)", hopQueue)
	case <-time.After(time.Second):
	}
}

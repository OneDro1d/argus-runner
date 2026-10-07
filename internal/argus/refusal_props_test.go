package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D16 — DeriveProps wires a declared "broker refuses with <code>" bullet into the props the
// AMQP Java Request sampler reads: expect.refusal_code (the content check, TemplateReads/
// enforced.go) + the connection it publishes over (mq.amqp.*), taken from the SAME message-broker
// target the tap already uses (VR10-S3 — never a second credential).
func refusalScenarioMD(id, expect string, userIDHeader string) string {
	lines := []string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: Message Flow", "- **Tags**: http, message-flow", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`",
	}
	if userIDHeader != "" {
		lines = append(lines, "User-Id: "+userIDHeader)
	}
	lines = append(lines,
		"", "## EXPECT", "### Runnable", "- "+expect, "",
		"### Non-runnable", "- a fixture", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	)
	return strings.Join(lines, "\n")
}

func refusalBrokerConfig() *config.Config {
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	c.Targets.MessageBroker = &config.MQTarget{
		URL:           "amqp://argus:s3cr3t-broker-pw@rabbitmq:5672/",
		ManagementURL: "http://rabbitmq:15672",
		Queues:        map[string]string{"incoming": "orders.incoming.q"},
		Exchanges:     map[string]string{"incoming": "orders.incoming"},
		RoutingKeys:   map[string]string{"incoming": "orders.created"},
	}
	return c
}

func TestDeriveProps_RefusalDeclared_EmitsExpectedCodeAndAMQPConnection(t *testing.T) {
	md := refusalScenarioMD("MF-020", "broker refuses with 406", "guest")
	p, err := DeriveProps(refusalBrokerConfig(), scenario.Parse(md), "tr-rf-1")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	if p["expect.refusal_code"] != "406" {
		t.Errorf("expect.refusal_code = %q, want 406", p["expect.refusal_code"])
	}
	// (the refusal sampler is the same Java setUri as AMQP Load): the configured URL ends in
	// "/", which amqp091-go reads as vhost "/" but the Java client reads as the EMPTY vhost — every connect
	// would fail 530 NOT_ALLOWED, so a declared 403/406 could never be observed. The default vhost travels as
	// NO path; credentials are stripped.
	if p["mq.amqp.uri"] != "amqp://rabbitmq:5672" {
		t.Errorf("mq.amqp.uri = %q, want the URL with credentials stripped and the default vhost as no path", p["mq.amqp.uri"])
	}
	if p["mq.amqp.username"] != "argus" {
		t.Errorf("mq.amqp.username = %q, want argus", p["mq.amqp.username"])
	}
	if p["mq.amqp.password"] != "s3cr3t-broker-pw" {
		t.Errorf("mq.amqp.password = %q, want the config's broker password", p["mq.amqp.password"])
	}
	if p["mq.exchange"] != "orders.incoming" {
		t.Errorf("mq.exchange = %q, want the config's incoming exchange", p["mq.exchange"])
	}
	if p["mq.routing_key"] != "orders.created" {
		t.Errorf("mq.routing_key = %q, want the config's incoming routing key", p["mq.routing_key"])
	}
	if p["mq.user_id"] != "guest" {
		t.Errorf("mq.user_id = %q, want the TRIGGER's declared User-Id", p["mq.user_id"])
	}
}

func TestDeriveProps_RefusalNotDeclared_EmitsNothing(t *testing.T) {
	md := refusalScenarioMD("MF-021", "event == OrderCreated", "")
	p, err := DeriveProps(refusalBrokerConfig(), scenario.Parse(md), "tr-rf-2")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	for _, k := range []string{"expect.refusal_code", "mq.amqp.uri", "mq.amqp.username", "mq.amqp.password", "mq.user_id"} {
		if p[k] != "" {
			t.Errorf("%s = %q, want empty — no refusal declared", k, p[k])
		}
	}
}

// A refusal bullet accepts the same name forms the grammar does (RefusalCodeNames).
func TestDeriveProps_RefusalDeclared_AcceptsNamedCodes(t *testing.T) {
	for _, c := range []struct{ bullet, wantCode string }{
		{"broker refuses with PRECONDITION_FAILED", "406"},
		{"broker refuses with ACCESS_REFUSED", "403"},
		{"broker refuses with NOT_FOUND", "404"},
		{"broker refuses with RESOURCE_LOCKED", "405"},
	} {
		t.Run(c.bullet, func(t *testing.T) {
			md := refusalScenarioMD("MF-022", c.bullet, "")
			p, err := DeriveProps(refusalBrokerConfig(), scenario.Parse(md), "tr-rf-3")
			if err != nil {
				t.Fatalf("DeriveProps: %v", err)
			}
			if p["expect.refusal_code"] != c.wantCode {
				t.Errorf("expect.refusal_code = %q, want %q", p["expect.refusal_code"], c.wantCode)
			}
		})
	}
}

// `User-Id` is excluded from the ordinary author-header set (headers.go): it is an AMQP property,
// not an HTTP header, and must never be sent as one to the SUT.
func TestDeriveProps_UserIdHeaderIsNeverSentAsAnHTTPHeader(t *testing.T) {
	md := refusalScenarioMD("MF-023", "broker refuses with 406", "guest")
	p, err := DeriveProps(refusalBrokerConfig(), scenario.Parse(md), "tr-rf-4")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	if p["mq.user_id"] != "guest" {
		t.Fatalf("mq.user_id = %q, want guest", p["mq.user_id"])
	}
	for k, v := range p {
		if strings.HasPrefix(k, "trigger.header_") && strings.HasSuffix(k, "_name") && v == "User-Id" {
			t.Errorf("User-Id was ALSO wired as an author header (%s=%s) — it must be AMQP-only", k, v)
		}
	}
}

// A scenario with no message-broker target configured at all must not panic and must derive no
// refusal props — defensive, mirrors the tap's own nil-safety.
func TestDeriveProps_RefusalDeclared_NoMessageBrokerTarget_NoPanicNoProps(t *testing.T) {
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	md := refusalScenarioMD("MF-024", "broker refuses with 406", "")
	p, err := DeriveProps(c, scenario.Parse(md), "tr-rf-5")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	if p["expect.refusal_code"] != "" || p["mq.amqp.uri"] != "" {
		t.Errorf("no message_broker target configured, want no refusal props: %v", p)
	}
}

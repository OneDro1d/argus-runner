package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The broker target's `envelope`, `streams` and `vhost` (for the bus chain step), at load time =
// validate-config time. No credential value is in any of them.

func loadBusYAML(t *testing.T, mq string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: p\ntargets:\n  message_broker:\n    type: amqp\n    url: ${BUS_CFG_TEST_URL}\n" + mq
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUS_CFG_TEST_URL", "amqp://svc:pw@broker.internal:5672/acme-orders")
	return Load(p)
}

const busGoodMQ = `    vhost: acme-orders
    envelope:
      correlation:
        - property: correlation_id
        - header: x-correlation-id
        - body: correlation_id
      list_payload: true
    streams:
      logs: { exchange: tr.logs, routing_key: logs.orders }
`

func TestBusTarget_LoadsEnvelopeStreamsAndVhost(t *testing.T) {
	c, err := loadBusYAML(t, busGoodMQ)
	if err != nil {
		t.Fatal(err)
	}
	mq := c.Targets.MessageBroker
	if len(mq.Envelope.Correlation) != 3 || !mq.Envelope.ListPayload || mq.Envelope.Correlation[2].Body != "correlation_id" ||
		mq.Streams["logs"].Exchange != "tr.logs" || mq.Vhost != "acme-orders" {
		t.Fatalf("decoded %+v %+v", mq.Envelope, mq.Streams)
	}
	// the NAME is read from the declaration, the value was expanded for the connection
	if name, ok := mq.BusURLEnv(); !ok || name != "BUS_CFG_TEST_URL" {
		t.Fatalf("BusURLEnv = %q %v", name, ok)
	}
	if strings.Contains(mq.URLEnvName, "pw") {
		t.Fatal("URLEnvName carries a value")
	}
	if v, ok := mq.URLVhost(); !ok || v != "acme-orders" {
		t.Fatalf("URLVhost = %q %v", v, ok)
	}
	if why := mq.BusPreflightProblem("targets.message_broker"); why != "" {
		t.Fatalf("preflight problem: %s", why)
	}
}

func TestBusTarget_NoCorrelationDeclaredIsAPreflightProblem_NoDefault(t *testing.T) {
	c, err := loadBusYAML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	why := c.Targets.MessageBroker.BusPreflightProblem("targets.message_broker")
	if !strings.Contains(why, "declares no envelope.correlation") || !strings.Contains(why, "there is no default") {
		t.Fatalf("got %q", why)
	}
}

func TestBusTarget_AUrlThatIsNotOneWholeReference(t *testing.T) {
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: p\ntargets:\n  message_broker:\n    url: amqp://${BUS_CFG_U}:${BUS_CFG_P}@h:5672/\n    envelope:\n      correlation:\n        - body: id\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUS_CFG_U", "u")
	t.Setenv("BUS_CFG_P", "p")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Targets.MessageBroker.BusURLEnv(); ok {
		t.Fatal("a url built from two references is not one whole ${NAME}")
	}
	if why := c.Targets.MessageBroker.BusPreflightProblem("targets.message_broker"); !strings.Contains(why, "must be one whole ${NAME} reference") {
		t.Fatalf("got %q", why)
	}
}

func TestBusTarget_VhostMismatchIsAPreflightProblem(t *testing.T) {
	c, err := loadBusYAML(t, strings.Replace(busGoodMQ, "vhost: acme-orders", "vhost: other", 1))
	if err != nil {
		t.Fatal(err)
	}
	why := c.Targets.MessageBroker.BusPreflightProblem("targets.message_broker")
	if !strings.Contains(why, `vhost is "other" but the URL names another vhost ("acme-orders")`) {
		t.Fatalf("got %q", why)
	}
	if strings.Contains(why, "pw") || strings.Contains(why, "broker.internal") {
		t.Fatalf("the URL leaked: %q", why)
	}
}

func TestBusTarget_StrictKeysAndValidationAtLoad(t *testing.T) {
	cases := []struct{ name, mq, want string }{
		{"an unknown key in envelope", "    envelope:\n      correlaton: []\n", `unknown key "correlaton" under a message_broker target's envelope — accepted: correlation, list_payload`},
		{"an unknown key in a source", "    envelope:\n      correlation:\n        - bodyy: x\n", `unknown key "bodyy" under a message_broker target's envelope.correlation entry (set exactly one) — accepted: property, header, body`},
		{"an unknown key in a stream", "    streams:\n      logs: { exchange: a, routing_key: b.c, queue: q }\n", `unknown key "queue" under a message_broker target's streams entry — accepted: exchange, routing_key`},
		{"two sources in one entry", "    envelope:\n      correlation:\n        - property: correlation_id\n          body: id\n", "envelope.correlation[0] must set exactly one of property, header, body"},
		{"an empty entry", "    envelope:\n      correlation:\n        - {}\n", "envelope.correlation[0] must set exactly one"},
		{"an unknown property", "    envelope:\n      correlation:\n        - property: nonsense\n", "is not one of correlation_id, message_id, type, app_id, reply_to"},
		{"a bad body path", "    envelope:\n      correlation:\n        - body: 'a b'\n", "is not a dotted body path"},
		{"a stream without a key", "    streams:\n      logs: { exchange: a }\n", "streams.logs needs both an exchange and a routing_key"},
		{"a stream bound to everything", "    streams:\n      logs: { exchange: a, routing_key: '#' }\n", "binds the whole exchange"},
		{"a placeholder in a stream", "    streams:\n      logs: { exchange: '${X}', routing_key: a.b }\n", "carries ${…} - a binding is a literal"},
		{"a placeholder in the vhost", "    vhost: '${V}'\n", "vhost carries ${…}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadBusYAML(t, c.mq)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a load error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestBusTarget_StreamLookup(t *testing.T) {
	c, err := loadBusYAML(t, busGoodMQ)
	if err != nil {
		t.Fatal(err)
	}
	mq := c.Targets.MessageBroker
	if s, err := mq.BusStreamNamed("targets.message_broker", "logs"); err != nil || s.RoutingKey != "logs.orders" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := mq.BusStreamNamed("targets.message_broker", "nope"); err == nil || !strings.Contains(err.Error(), `unknown stream "nope"`) || !strings.Contains(err.Error(), "declares: logs") {
		t.Fatalf("%v", err)
	}
}

// the URL name is listed for the onboarding preflight, the value never is (the existing walker; pinned here for the bus target)
func TestBusTarget_TheUrlReferenceIsListedAndNoValueIsInTheDeclarations(t *testing.T) {
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	body := "project:\n  name: p\ntargets:\n  message_broker_targets:\n    sut-bus:\n      url: ${BUS_CFG_TEST_URL}\n" +
		"      envelope:\n        correlation:\n          - body: correlation_id\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := ParseUnresolved(p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range c.EnvRefs() {
		if r.Name == "BUS_CFG_TEST_URL" && r.Field == "targets.message_broker_targets.sut-bus.url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("EnvRefs = %+v", c.EnvRefs())
	}
	if name, ok := c.Targets.MessageBrokerTargets["sut-bus"].BusURLEnv(); !ok || name != "BUS_CFG_TEST_URL" {
		t.Fatalf("an unresolved parse also knows the name: %q %v", name, ok)
	}
}

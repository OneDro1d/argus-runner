package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// V30-004 T-B (VR13-MF) — THE TAP IS DECLARED ONLY WHEN CONTENT IS ASSERTED.
//
// The gate looked obvious and was wrong in exactly one case. `ParseDBExpect` sets `RowCount = 0` and
// `Columns = {}` when `has_rows` is false (dbexpect.go:193-198) — so "tap when expect.columns or
// expect.row_count is present" would tap on precisely the `- no rows` case F-4 says must NEVER tap.
// A tap bound before the trigger would itself hold the message the scenario asserts was never
// published, and the negative case would fail against its own instrument.
//
// The gate is therefore: emit mq.tap.* when `expect.columns` is NON-EMPTY, or (`expect.row_count`
// is present AND `expect.has_rows` is not false).
func TestDeriveProps_EmitsTheTapPropertiesOnlyWhenContentIsAsserted(t *testing.T) {
	propsForMF := func(t *testing.T, layer, expect string) map[string]string {
		t.Helper()
		md := strings.Join([]string{
			"# Scenario: t", "",
			"## Metadata", "- **ID**: MF-001", "- **Layer**: " + layer, "- **Tags**: http, message-flow", "",
			"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
			"## EXPECT", "### Runnable", expect, "",
			"### Non-runnable", "- a fixture", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		c := &config.Config{}
		c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
		c.Targets.MessageBroker = &config.MQTarget{
			URL:           "amqp://argus:pw@rabbitmq:5672/",
			ManagementURL: "http://rabbitmq:15672",
			Queues:        map[string]string{"incoming": "orders.incoming.q"},
			Exchanges:     map[string]string{"incoming": "orders.incoming"},
			// a content check without this key is refused before dispatch (tap_routing_key_test.go)
			RoutingKeys: map[string]string{"incoming": "orders.created"},
		}
		p, err := DeriveProps(c, scenario.Parse(md), "tr-mf-1")
		if err != nil {
			t.Fatalf("DeriveProps: %v", err)
		}
		return p
	}

	t.Run("a column assertion declares a tap", func(t *testing.T) {
		p := propsForMF(t, "Message Flow", "- event == OrderCreated")
		if p["mq.tap.queue"] == "" {
			t.Fatal("a content assertion must declare a tap, or nothing evaluates it")
		}
		// the queue is per-RUN, so two scenarios never share one
		if !strings.Contains(p["mq.tap.queue"], "tr-mf-1") {
			t.Errorf("mq.tap.queue = %q, want it scoped to this run's correlation id", p["mq.tap.queue"])
		}
		if !strings.HasPrefix(p["mq.tap.queue"], "argus-tap-") {
			t.Errorf("mq.tap.queue = %q, want the argus-tap- prefix the SUT's grant is written against", p["mq.tap.queue"])
		}
		if p["mq.exchange"] != "orders.incoming" {
			t.Errorf("mq.exchange = %q, want the config's incoming exchange", p["mq.exchange"])
		}
		if p["mq.routing_key"] != "orders.created" {
			t.Errorf("mq.routing_key = %q, want the config's incoming routing key — the tap binds with it", p["mq.routing_key"])
		}
		if p["mq.negative.queue"] != "" {
			t.Errorf("the negative check must NOT be armed on a positive scenario: %q", p["mq.negative.queue"])
		}
	})

	t.Run("a row_count assertion declares a tap", func(t *testing.T) {
		if p := propsForMF(t, "Message Flow", "- row_count == 1"); p["mq.tap.queue"] == "" {
			t.Fatal("a row_count assertion must declare a tap")
		}
	})

	t.Run("⛔ `- no rows` declares NO tap, and arms the negative check instead", func(t *testing.T) {
		p := propsForMF(t, "Message Flow", "- no rows")
		if p["mq.tap.queue"] != "" {
			t.Errorf("the negative case must NOT tap — the tap would hold the very message it asserts was never published: %q", p["mq.tap.queue"])
		}
		if p["mq.tap.ttl_ms"] != "" || p["mq.exchange"] != "" || p["mq.routing_key"] != "" {
			t.Errorf("no tap property may be emitted for the negative case: %v", p)
		}
		if p["mq.negative.queue"] != "orders.incoming.q" {
			t.Errorf("mq.negative.queue = %q, want the SUT's own incoming queue", p["mq.negative.queue"])
		}
	})

	t.Run("a scenario asserting no content declares neither", func(t *testing.T) {
		p := propsForMF(t, "Message Flow", "- status=202")
		if p["mq.tap.queue"] != "" || p["mq.negative.queue"] != "" {
			t.Errorf("nothing to evaluate means nothing to arm: %v", p)
		}
	})

	t.Run("⛔ and a DATABASE scenario never taps, whatever it asserts", func(t *testing.T) {
		// The tap belongs to the broker layers. database-state.jmx has its own JDBC verify.
		if p := propsForMF(t, "Database State", "- row_count == 1"); p["mq.tap.queue"] != "" {
			t.Errorf("a database scenario declared a broker tap: %q", p["mq.tap.queue"])
		}
	})
}

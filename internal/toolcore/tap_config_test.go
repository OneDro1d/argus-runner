package toolcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// V30-004 F-6 (VR13-MF) — A SUT WHOSE BROKER CONFIG THE TAP CANNOT USE IS REPORTED BEFORE A RUN.
//
// The tap binds a queue to the SUT's incoming exchange with a routing key. An exchange is only half
// an address: a `topic` exchange delivers NOTHING to a queue bound with the wrong key, so a tap with
// no key would receive nothing, and every content assertion would fail for a reason that is ours.
//
// ⛔ THIS IS NOT AN AUTHORING REFUSAL, and the distinction is structural rather than stylistic.
// ExpectProblems is handed a SCENARIO and never a *config.Config — on the control plane there is no
// SUT config in scope at all — so "this SUT declares no routing_keys.incoming" can only be said by
// the config-aware path: `argus validate-config --scenarios`, which holds both.
func TestValidateConfig_ReportsABrokerConfigTheTapCannotUse(t *testing.T) {
	write := func(t *testing.T, brokerBlock, expectBody string) (string, string) {
		t.Helper()
		dir := t.TempDir()
		cfg := filepath.Join(dir, "argus-config.yaml")
		if err := os.WriteFile(cfg, []byte(strings.Join([]string{
			"project:", "  name: t", "",
			"targets:",
			"  http:", "    base_url: http://sut.invalid",
			brokerBlock, "",
		}, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		scDir := filepath.Join(dir, "scenarios")
		d := filepath.Join(scDir, "message-flow")
		_ = os.MkdirAll(d, 0o755)
		md := strings.Join([]string{
			"# Scenario: m", "",
			"## Metadata", "- **ID**: MSGF-001", "- **Layer**: Message Flow", "- **Tags**: http, message-flow", "",
			"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
			"## EXPECT", "### Runnable", expectBody, "",
			"### Non-runnable", "- a fixture", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		if err := os.WriteFile(filepath.Join(d, "MSGF-001.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
		return cfg, scDir
	}

	const full = "  message_broker:\n    type: amqp\n    url: amqp://u:p@rabbitmq:5672/\n    management_url: http://rabbitmq:15672\n    exchanges: {incoming: orders.incoming}\n    queues: {incoming: orders.incoming.q}\n    routing_keys: {incoming: orders.created}"
	const noKey = "  message_broker:\n    type: amqp\n    url: amqp://u:p@rabbitmq:5672/\n    management_url: http://rabbitmq:15672\n    exchanges: {incoming: orders.incoming}\n    queues: {incoming: orders.incoming.q}"
	const noExchange = "  message_broker:\n    type: amqp\n    url: amqp://u:p@rabbitmq:5672/\n    management_url: http://rabbitmq:15672\n    queues: {incoming: orders.incoming.q}\n    routing_keys: {incoming: orders.created}"

	run := func(t *testing.T, brokerBlock, expectBody string) string {
		t.Helper()
		cfg, scDir := write(t, brokerBlock, expectBody)
		payload, _, err := ValidateConfig(Env{ConfigPath: cfg, ScenariosDir: scDir, Tier: "compose"})
		if err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
		return errorText(payload.(map[string]any))
	}

	t.Run("no routing_keys.incoming is reported", func(t *testing.T) {
		text := run(t, noKey, "- event == OrderCreated")
		if !strings.Contains(text, "routing_keys") {
			t.Fatalf("a content-asserting Message Flow scenario against a config with no routing key must be reported: %q", text)
		}
		if !strings.Contains(text, "MSGF-001") {
			t.Errorf("the report must name the scenario that needs it: %q", text)
		}
	})

	t.Run("no exchanges.incoming is reported too", func(t *testing.T) {
		if text := run(t, noExchange, "- event == OrderCreated"); !strings.Contains(text, "exchanges") {
			t.Fatalf("an exchange is the other half of the address and must be reported: %q", text)
		}
	})

	t.Run("a complete config is not reported", func(t *testing.T) {
		if text := run(t, full, "- event == OrderCreated"); strings.Contains(text, "routing_keys") {
			t.Errorf("a config the tap can use must not be reported: %q", text)
		}
	})

	t.Run("⛔ and a scenario that asserts no content never needs either", func(t *testing.T) {
		// No content assertion means no tap, so the config facts the tap needs are irrelevant.
		if text := run(t, noKey, "- status=202"); strings.Contains(text, "routing_keys") {
			t.Errorf("a scenario that declares no tap must not demand the tap's config: %q", text)
		}
	})
}

// ⛔ THE ENTRY THE SCENARIO RUNS AGAINST (0.3.32 rebuild adversary gate). The run refuses a tap whose SELECTED broker
// entry — `targets.message_broker`, or the `message_broker_targets` entry a **Target** names — has no
// routing_keys.incoming, a blank key included (argus.ErrNoRoutingKey). validate-config must judge that same entry, or
// the pre-run check passes a scenario the run refuses, and flags one the run accepts.
func TestValidateConfig_JudgesTheBrokerEntryTheScenarioSelects(t *testing.T) {
	broker := func(indent, host, key string) string {
		lines := []string{
			"type: amqp", "url: amqp://u:p@" + host + ":5672/", "management_url: http://" + host + ":15672",
			"exchanges: {incoming: " + host + ".incoming}", "queues: {incoming: " + host + ".incoming.q}",
		}
		if key != "" {
			lines = append(lines, "routing_keys: {incoming: "+key+"}")
		}
		return indent + strings.Join(lines, "\n"+indent)
	}
	run := func(t *testing.T, plainKey, auditKey, target string) string {
		t.Helper()
		dir := t.TempDir()
		cfg := filepath.Join(dir, "argus-config.yaml")
		if err := os.WriteFile(cfg, []byte(strings.Join([]string{
			"project:", "  name: t", "",
			"targets:",
			"  http:", "    base_url: http://sut.invalid",
			"  message_broker:", broker("    ", "rabbitmq", plainKey),
			"  message_broker_targets:", "    audit:", broker("      ", "audit", auditKey), "",
		}, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := []string{"## Metadata", "- **ID**: MSGF-020", "- **Layer**: Message Flow", "- **Tags**: http, message-flow"}
		if target != "" {
			meta = append(meta, "- **Target**: "+target)
		}
		scDir := filepath.Join(dir, "scenarios")
		d := filepath.Join(scDir, "message-flow")
		_ = os.MkdirAll(d, 0o755)
		md := strings.Join(append(append([]string{"# Scenario: m", ""}, meta...),
			"", "## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
			"## EXPECT", "### Runnable", "- event == OrderCreated", "",
			"### Non-runnable", "- a fixture", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", ""), "\n")
		if err := os.WriteFile(filepath.Join(d, "MSGF-020.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
		payload, _, err := ValidateConfig(Env{ConfigPath: cfg, ScenariosDir: scDir, Tier: "compose"})
		if err != nil {
			t.Fatalf("ValidateConfig: %v", err)
		}
		return errorText(payload.(map[string]any))
	}

	t.Run("a named entry with no key is reported although the plain slot has one", func(t *testing.T) {
		if text := run(t, "orders.created", "", "audit"); !strings.Contains(text, "routing_keys") {
			t.Errorf("the run refuses this scenario (its audit entry has no key), and validate-config said nothing: %q", text)
		}
	})
	t.Run("a named entry with a key is not reported although the plain slot has none", func(t *testing.T) {
		if text := run(t, "", "audit.created", "audit"); strings.Contains(text, "routing_keys") {
			t.Errorf("the scenario runs against its audit entry, which has a key — yet validate-config reported one missing: %q", text)
		}
	})
	t.Run("a blank key is reported, as the run treats it", func(t *testing.T) {
		if text := run(t, `"   "`, "audit.created", ""); !strings.Contains(text, "routing_keys") {
			t.Errorf("a blank routing key is refused at run time, and validate-config said nothing: %q", text)
		}
	})
}

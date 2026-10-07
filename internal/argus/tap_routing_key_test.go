package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// tap_routing_key_test.go — found in the 0.3.32 Release QA; built into the next 0.3.32 build on the owner's
// ruling (2026-09-14: "if anything is clear how to fix – include it in the build").
//
// A Message Flow content check reads the published message through a tap queue BOUND WITH
// `routing_keys.incoming` of the scenario's broker target (V30-004). A config written before 0.3.32 has no such
// key, and DeriveProps handed the template an EMPTY routing key: the tap bound nothing, the SUT's broker got a
// queue for nothing, and the scenario failed "the tap returned 0 message(s)" — a sentence that names neither the
// cause nor the fix. The key's absence is known before anything is sent, so it is refused there, by name.

func mfScenarioMD(id string, expect ...string) string {
	return strings.Join([]string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: Message Flow", "- **Tags**: http, message-flow", "",
		"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
		"## EXPECT", "### Runnable", strings.Join(expect, "\n"), "",
		"### Non-runnable", "- a fixture", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

func brokerConfig(routingKey string) *config.Config {
	c := &config.Config{}
	c.Project.Name = "order-service"
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	c.Targets.MessageBroker = &config.MQTarget{
		URL:           "amqp://argus:pw@rabbitmq:5672/",
		ManagementURL: "http://rabbitmq:15672",
		Queues:        map[string]string{"incoming": "orders.incoming.q"},
		Exchanges:     map[string]string{"incoming": "orders.incoming"},
	}
	if routingKey != "" {
		c.Targets.MessageBroker.RoutingKeys = map[string]string{"incoming": routingKey}
	}
	return c
}

func TestDeriveProps_AContentCheckWithNoRoutingKeyIsRefusedByName(t *testing.T) {
	for name, key := range map[string]string{"no routing_keys at all": "", "a blank key": "   "} {
		t.Run(name, func(t *testing.T) {
			c := brokerConfig("")
			if key != "" {
				c.Targets.MessageBroker.RoutingKeys = map[string]string{"incoming": key}
			}
			_, err := DeriveProps(c, scenario.Parse(mfScenarioMD("MF-010", "- event == OrderCreated")), "tr-rk-1")
			if err == nil {
				t.Fatal("a content check with no routing key was accepted — the tap would bind nothing and the run would " +
					"report \"the tap returned 0 message(s)\" instead of the missing key")
			}
			if !strings.Contains(err.Error(), "routing_keys.incoming") {
				t.Errorf("the refusal does not name the key to add: %v", err)
			}
		})
	}
}

// ⛔ ONLY THE TAP NEEDS THE KEY. The negative case reads the SUT's own queue depth and a scenario asserting no
// content arms nothing — refusing those would break every pre-0.3.32 config for no reason.
func TestDeriveProps_OnlyTheTapNeedsARoutingKey(t *testing.T) {
	for name, expect := range map[string]string{
		"the negative case reads the SUT's own queue":  "- no rows",
		"a scenario asserting no content taps nothing": "- status=202",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DeriveProps(brokerConfig(""), scenario.Parse(mfScenarioMD("MF-011", expect)), "tr-rk-2"); err != nil {
				t.Errorf("refused although no tap is declared: %v", err)
			}
		})
	}
}

// tapCountingRunner counts template runs. A refused scenario must never reach one: every run of
// message-flow.jmx with a tap declares a queue on the SUT's broker.
type tapCountingRunner struct{ calls int }

func (r *tapCountingRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	r.calls++
	return os.WriteFile(jtlPath, []byte("timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n"), 0o644)
}

func TestRunAll_AContentCheckWithNoRoutingKeyNeverReachesTheBroker(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(filepath.Join(scDir, "message-flow"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := mfScenarioMD("MF-012", "- status=202", "- event == OrderCreated")
	if err := os.WriteFile(filepath.Join(scDir, "message-flow", "MF-012.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &tapCountingRunner{}
	rr, err := RunAll(brokerConfig(""), scDir, filepath.Join(dir, "results"), "order-service", "", "", "", "", r)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Errorf("the template ran %d time(s): a tap was declared on the SUT's broker for a check that could never see a message", r.calls)
	}
	res, _ := rr.Report.Find("MF-012")
	if res == nil || res.Failure == nil {
		t.Fatalf("MF-012 has no result or no failure: %+v", res)
	}
	if res.Status != report.StatusError {
		t.Errorf("status = %q, want %q — nothing was measured and the gap is the config's; only `failed` blames the SUT",
			res.Status, report.StatusError)
	}
	if !strings.Contains(res.Failure.Observed, "routing_keys.incoming") {
		t.Errorf("observed %q does not name the missing key", res.Failure.Observed)
	}
}

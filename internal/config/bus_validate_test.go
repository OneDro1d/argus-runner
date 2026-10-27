package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `argus validate-config --scenarios <dir>` cross-checks a chain scenario's bus steps against the config:
// the step's target must be a message_broker target and its stream must be declared on THAT target. Each mismatch names the
// scenario file and the step.

func busScenarioMD(id, step string) string {
	return "# Scenario: " + id + "\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: Message Flow\n- **Tags**: chain\n\n" +
		"## TRIGGER\nPOST `chain`\n\n```json\n{\"steps\":[" + step + "]}\n```\n\n" +
		"## EXPECT\n### Runnable\n- step bus1: body has count >= 1\n\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A - a unit-test fixture.\n"
}

func TestValidate_BusStepTargetAndStreamAreCrossCheckedAgainstTheConfig(t *testing.T) {
	t.Setenv("BUS_CFG_TEST_URL", "amqp://u:[REDACTED]@broker.internal:5672/")
	t.Setenv("BUS_CFG_SECOND_URL", "amqp://u:[REDACTED]@second.internal:5672/")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	cfg := "project:\n  name: p\ntargets:\n  message_broker:\n    type: amqp\n    url: ${BUS_CFG_TEST_URL}\n" + busGoodMQ +
		"  message_broker_targets:\n    second:\n      type: amqp\n      url: ${BUS_CFG_SECOND_URL}\n" +
		"      envelope:\n        correlation:\n          - body: id\n" +
		"      streams:\n        audit: { exchange: tr.audit, routing_key: audit.orders }\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	sc := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(filepath.Join(sc, "chains"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id, step string
		want     string // "" = no bus error expected, else a substring of the one error
	}{
		{"BUS-OK-PLAIN-STREAM", `{"type":"bus","name":"bus1","stream":"logs"}`, ""},
		{"BUS-OK-BINDING", `{"type":"bus","name":"bus1","exchange":"tr.logs","routing_key":"logs.orders"}`, ""},
		{"BUS-OK-NAMED", `{"type":"bus","name":"bus1","target":"second","stream":"audit"}`, ""},
		{"BUS-BAD-STREAM", `{"type":"bus","name":"bus1","stream":"nope"}`, `unknown stream "nope" - targets.message_broker declares: logs`},
		{"BUS-BAD-TARGET", `{"type":"bus","name":"bus1","target":"missing","stream":"logs"}`, `"missing"`},
		{"BUS-BAD-STREAM-ON-OTHER-TARGET", `{"type":"bus","name":"bus1","target":"second","stream":"logs"}`,
			`unknown stream "logs" - targets.message_broker_targets.second declares: audit`},
	}
	for _, r := range rows {
		if err := os.WriteFile(filepath.Join(sc, "chains", r.id+".md"), []byte(busScenarioMD(r.id, r.step)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	errs, n, err := c.Validate(sc)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n != len(rows) {
		t.Fatalf("discovered %d scenarios, want %d", n, len(rows))
	}
	by := map[string][]string{}
	for _, e := range errs {
		by[e.Scenario] = append(by[e.Scenario], e.Message)
	}
	for _, r := range rows {
		got := by[r.id]
		if r.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: no error expected, got %v", r.id, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(got[0], r.want) {
			t.Errorf("%s: want one error containing %q, got %v", r.id, r.want, got)
			continue
		}
		if !strings.Contains(got[0], "chains/"+r.id+".md") || !strings.Contains(got[0], `bus step "bus1"`) {
			t.Errorf("%s: the error must name the scenario file and the step, got %q", r.id, got[0])
		}
	}
}

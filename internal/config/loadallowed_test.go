package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// S9: load runs only against a target the OPERATOR marked, off by default.

const brokerTargets = `project:
  name: p
targets:
  http:
    base_url: http://sut.invalid
  message_broker_targets:
    load-lab:
      type: amqp
      url: amqp://loaduser:s3cr3t-token-91x@load-lab.invalid:5672/
      management_url: http://load-lab.invalid:15672
    other:
      type: amqp
      url: amqp://loaduser:s3cr3t-token-91x@other.invalid:5672/
`

func loadFrom(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func amqpScenario(target string, steps string) *scenario.Scenario {
	return scenario.Parse(strings.Join([]string{
		"# Scenario: t", "", "## Metadata", "- **ID**: L-001", "- **Layer**: AMQP Load", "- **Tags**: http, load",
		"- **Target**: " + target, "",
		"## TRIGGER", "POST `/amqp-load`", "",
		"## EXPECT", "### Runnable", "- broker is not blocked", "",
		"## LOAD", "- **Steps**: " + steps, "- **Step Duration Seconds**: 60", "- **Target P95 Ms**: 250", "- **Max Error Rate**: 0.01", "",
		"## TIMEOUT", "30s", "", "## CLEANUP", "N/A — a unit-test fixture.", "",
	}, "\n"))
}

func TestLoadAllowed_AbsentMeansNoTargetAllowsLoad(t *testing.T) {
	c, err := loadFrom(t, brokerTargets)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LoadAllowedTargets) != 0 {
		t.Fatalf("absent block decoded as %v", c.LoadAllowedTargets)
	}
	if r := c.LoadRefusal(amqpScenario("load-lab", "10")); r == "" {
		t.Fatal("no load_allowed_targets, and the scenario was allowed: the default must be OFF")
	}
}

func TestLoadAllowed_ListedTargetIsAllowed_UnlistedIsNot(t *testing.T) {
	c, err := loadFrom(t, brokerTargets+"load_allowed_targets:\n  load-lab:\n    max_sessions: 1000\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := c.LoadRefusal(amqpScenario("load-lab", "10, 1000")); r != "" {
		t.Errorf("a listed target was refused: %s", r)
	}
	if r := c.LoadRefusal(amqpScenario("other", "10")); r == "" {
		t.Error("an unlisted target was allowed")
	}
}

func TestLoadAllowed_CeilingRefusesAStepAboveMaxSessions(t *testing.T) {
	c, err := loadFrom(t, brokerTargets+"load_allowed_targets:\n  load-lab:\n    max_sessions: 100\n")
	if err != nil {
		t.Fatal(err)
	}
	r := c.LoadRefusal(amqpScenario("load-lab", "10, 500"))
	if !strings.Contains(r, "500") || !strings.Contains(r, "max_sessions") || !strings.Contains(r, "100") {
		t.Fatalf("ceiling refusal does not name the steps and the ceiling: %q", r)
	}
}

// The refusal text is shown to the author and logged: it never carries a URL, host or credential.
func TestLoadAllowed_RefusalNeverEchoesAnAddressOrCredential(t *testing.T) {
	c, err := loadFrom(t, brokerTargets)
	if err != nil {
		t.Fatal(err)
	}
	r := c.LoadRefusal(amqpScenario("load-lab", "10"))
	for _, leak := range []string{"load-lab.invalid", "amqp://", "s3cr3t-token-91x", "loaduser"} {
		if strings.Contains(r, leak) {
			t.Errorf("refusal leaks %q: %s", leak, r)
		}
	}
	if !strings.Contains(r, "refused before firing") || !strings.Contains(r, "Nothing was sent") {
		t.Errorf("refusal text is not the documented one: %s", r)
	}
}

func TestLoadAllowed_NameMustBeABrokerTarget(t *testing.T) {
	_, err := loadFrom(t, brokerTargets+"load_allowed_targets:\n  load-lb:\n    max_sessions: 10\n")
	if err == nil || !strings.Contains(err.Error(), "load_allowed_targets.load-lb") || !strings.Contains(err.Error(), "did you mean load-lab") {
		t.Fatalf("a name that is no broker target must be refused with a hint: %v", err)
	}
}

func TestLoadAllowed_PlainSlotCannotBeListed(t *testing.T) {
	body := strings.Replace(brokerTargets, "  message_broker_targets:", "  message_broker:\n    url: amqp://u:x@plain.invalid:5672/\n  message_broker_targets:", 1)
	_, err := loadFrom(t, body+"load_allowed_targets:\n  message_broker:\n    max_sessions: 10\n")
	if err == nil {
		t.Fatal("the plain slot was listable")
	}
}

func TestLoadAllowed_SharedOrProductionURLIsRefused(t *testing.T) {
	for _, field := range []string{"url: amqp://u:x@rabbit.prod.internal:5672/", "management_url: http://rabbit.shared.internal:15672"} {
		body := strings.Replace(brokerTargets, "      type: amqp\n      url: amqp://loaduser:s3cr3t-token-91x@load-lab.invalid:5672/\n      management_url: http://load-lab.invalid:15672\n",
			"      type: amqp\n      "+field+"\n", 1)
		_, err := loadFrom(t, body+"load_allowed_targets:\n  load-lab:\n    max_sessions: 10\n")
		if err == nil || !strings.Contains(err.Error(), "shared/production") {
			t.Errorf("%s: want a shared/production refusal, got %v", field, err)
		}
		if err != nil && (strings.Contains(err.Error(), "prod.internal") || strings.Contains(err.Error(), "shared.internal")) {
			t.Errorf("the refusal echoes the address: %v", err)
		}
	}
}

func TestLoadAllowed_MaxSessionsBoundsAndTypoAreLoud(t *testing.T) {
	for _, bad := range []string{"max_sessions: 0\n", "max_sessions: 10001\n", "max_sesions: 10\n"} {
		_, err := loadFrom(t, brokerTargets+"load_allowed_targets:\n  load-lab:\n    "+bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// An OLD executor decodes the same file with the strict doc as it was BEFORE this key existed. The key
// must fall into its inline map and be ignored: a rollback must not take an executor down.
type oldStrictTargetsDoc struct {
	Targets        Targets            `yaml:"targets"`
	RateLimit      *RateLimit         `yaml:"rate_limit"`
	Package        *Package           `yaml:"package"`
	MoneyWrites    *MoneyWritesConfig `yaml:"money_writes"`
	MessageSchemas MessageSchemas     `yaml:"message_schemas"`
	Rest           map[string]any     `yaml:",inline"`
}

func TestLoadAllowed_OldExecutorDecodesIgnoring(t *testing.T) {
	body := brokerTargets + "load_allowed_targets:\n  load-lab:\n    max_sessions: 1000\n"
	dec := yaml.NewDecoder(bytes.NewReader([]byte(body)))
	dec.KnownFields(true)
	var d oldStrictTargetsDoc
	if err := dec.Decode(&d); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("an old executor's strict decode refused the new top-level key: %v", err)
	}
	if _, ok := d.Rest["load_allowed_targets"]; !ok {
		t.Error("the key did not land in the old executor's lenient inline map")
	}
	// ...and the control: put the same key UNDER targets and the old strict decode refuses it, which is
	// exactly why the gate is top-level (design fact 3).
	under := strings.Replace(brokerTargets, "      management_url: http://load-lab.invalid:15672\n", "      management_url: http://load-lab.invalid:15672\n      allow_load: true\n", 1)
	dec = yaml.NewDecoder(bytes.NewReader([]byte(under)))
	dec.KnownFields(true)
	if err := dec.Decode(&oldStrictTargetsDoc{}); err == nil {
		t.Fatal("control failed: a key under targets was accepted by the old strict doc, so the premise of the top-level key is gone")
	}
}

func TestLoadAllowed_ValidateReportsAnUnlistedAMQPLoadScenario(t *testing.T) {
	c, err := loadFrom(t, brokerTargets)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "amqp-load")
	_ = os.MkdirAll(sub, 0o755)
	md := "# Scenario: t\n\n## Metadata\n- **ID**: L-001\n- **Layer**: AMQP Load\n- **Tags**: http, load\n- **Target**: load-lab\n\n## TRIGGER\nPOST `/amqp-load`\n\n## EXPECT\n### Runnable\n- broker is not blocked\n\n## LOAD\n- **Steps**: 10\n- **Step Duration Seconds**: 60\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A — fixture.\n"
	if err := os.WriteFile(filepath.Join(sub, "L-001.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	errs, _, err := c.Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, e := range errs {
		if e.Scenario == "L-001" && strings.Contains(e.Message, "load_allowed_targets does not list") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("validate-config did not report the refusal: %+v", errs)
	}
}

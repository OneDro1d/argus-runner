package toolcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// The AUTHOR path (every writer goes through ValidateAll: Validate + argus.ExpectProblems) accepts a
// well-formed AMQP Load scenario and refuses one that carries a status bullet.
func TestValidateAll_AMQPLoadScenario(t *testing.T) {
	md := "# Scenario: t\n\n## Metadata\n- **ID**: AL-001\n- **Layer**: AMQP Load\n- **Tags**: http, load\n- **Target**: load-lab\n\n## TRIGGER\nPOST `/amqp-load`\n\n## EXPECT\n### Runnable\n- broker is not blocked\n- every step is measured\n\n### Non-runnable\n- the ramp reports the tested limit\n\n## LOAD\n- **Steps**: 10, 100\n- **Step Duration Seconds**: 60\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A — the sampler deletes its queues.\n"
	if _, errs := ValidateAll(md); len(errs) != 0 {
		t.Fatalf("the author path refuses a well-formed AMQP Load scenario: %+v", errs)
	}
	bad := strings.Replace(md, "- broker is not blocked\n", "- status=200\n", 1)
	if _, errs := ValidateAll(bad); len(errs) == 0 {
		t.Fatal("the author path accepted a status= bullet on an AMQP Load scenario")
	}
}

// S9, the authoring-time half: validate_config reports an AMQP Load scenario whose target the operator
// has not marked as allowing load. (The run-time door is internal/argus; this one is advice.)
func TestValidateConfig_ReportsAnAMQPLoadScenarioOnAnUnlistedTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: p\ntargets:\n  message_broker_targets:\n    load-lab:\n      type: amqp\n      url: amqp://u:x@load-lab.invalid:5672/\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := filepath.Join(dir, "scenarios", "amqp-load")
	_ = os.MkdirAll(sc, 0o755)
	md := "# Scenario: t\n\n## Metadata\n- **ID**: AL-001\n- **Layer**: AMQP Load\n- **Tags**: http, load\n- **Target**: load-lab\n\n## TRIGGER\nPOST `/amqp-load`\n\n## EXPECT\n### Runnable\n- broker is not blocked\n\n## LOAD\n- **Steps**: 10\n- **Step Duration Seconds**: 60\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A — fixture.\n"
	if err := os.WriteFile(filepath.Join(sc, "AL-001.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	out, invalid, err := ValidateConfig(Env{ConfigPath: cfg, ScenariosDir: filepath.Join(dir, "scenarios")})
	if err != nil {
		t.Fatal(err)
	}
	if !invalid {
		t.Fatal("validate_config passed a config that would refuse the scenario at run time")
	}
	errs, _ := out.(map[string]any)["errors"].([]config.ConfigError)
	var hit bool
	for _, e := range errs {
		if e.Scenario == "AL-001" && strings.Contains(e.Message, "load_allowed_targets does not list") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("no S9 refusal among %+v", errs)
	}
}

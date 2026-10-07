package config

import (
	"os"
	"path/filepath"
	"testing"
)

// AC-D18b — a chain made only of `amqp` steps dials the broker named by each step's url_env; it
// never touches the SUT's MCP endpoint or an HTTP target, so validate-config must demand neither
// targets.mcp nor targets.http for it (mirror of AC-D20b for http steps).
func TestValidate_PureAMQPChainNeedsNoMCPOrHTTPTarget(t *testing.T) {
	dir := t.TempDir()
	md := "# Scenario: c\n\n## Metadata\n- **ID**: CHA-1\n- **Layer**: Message Flow\n- **Tags**: chain\n\n" +
		"## TRIGGER\nPOST `chain`\n\n```json\n{\"steps\":[{\"type\":\"amqp\",\"name\":\"a\",\"op\":\"declare_queue\"," +
		"\"url_env\":\"ARGUS_TEST_AMQP_URL\",\"queue\":\"q\"}]}\n```\n\n" +
		"## EXPECT\n### Runnable\n- step a: broker refuses with 403\n"
	if err := os.WriteFile(filepath.Join(dir, "CHA-1.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	c := loadYAML(t, "project:\n  name: msgbus\nscenarios:\n  timeout_default: 20s\n")
	errs, n, err := c.Validate(dir)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n != 1 {
		t.Fatalf("the scenario must be discovered, count=%d", n)
	}
	if len(errs) != 0 {
		t.Fatalf("a chain of amqp steps only must require no target: %+v", errs)
	}
}

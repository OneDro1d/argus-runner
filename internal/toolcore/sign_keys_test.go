package toolcore

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// `argus validate-scenario --config` refuses a `sign` key reference the config does not declare.
func TestValidateScenarioWithConfig_UndeclaredSigningKeyIsRefused(t *testing.T) {
	md := strings.Join([]string{
		"# Scenario: c", "", "## Metadata", "- **ID**: CHN-SIGN", "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "", "```json",
		`{"steps":[{"type":"mcp","name":"w","tool":"t","args":{"a":"x"},"sign":{"key":"MEMBER_A_KEY","fields":[{"path":"a"}],"header":"X-Sig"}}]}`,
		"```", "", "## EXPECT", "### Runnable", "- step w: body contains ok", "", "## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A - a unit-test fixture.", ""}, "\n")
	c := &config.Config{}
	out, failed, err := ValidateScenarioWithConfig(md, c)
	if err != nil || !failed {
		t.Fatalf("an undeclared key reference must make the scenario invalid: failed=%v err=%v out=%v", failed, err, out)
	}
	m, _ := out.(map[string]any)
	got := ""
	if errs, ok := m["schema_errors"].([]string); ok {
		got = strings.Join(errs, " ")
	}
	if !strings.Contains(got, "MEMBER_A_KEY") {
		t.Fatalf("the refusal must name the reference: %v", m)
	}
	c.CheckEnv = []string{"MEMBER_A_KEY"}
	if _, failed, _ := ValidateScenarioWithConfig(md, c); failed {
		t.Fatalf("a declared reference validates")
	}
}

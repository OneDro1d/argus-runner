package toolcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `argus validate-config --scenarios <dir>` applied two of the scenario rules to the files (the EXPECT
// classifier and the id rule) and none of the others, so it answered valid:true and said NOTHING about a check the
// control plane's author_write_scenario then refused (seen live: `## TIMEOUT 60s` on an HTTP Ingestion check, ceiling
// 30 s). Every rule the writers apply is now REPORTED by validate-config, as a warning naming the file and the line.
// It does not change `valid`: a kit that validated yesterday still validates (see the finding for why).

func warningText(t *testing.T, payload any) string {
	t.Helper()
	b, err := json.Marshal(payload.(map[string]any)["warnings"])
	if err != nil {
		t.Fatalf("warnings: %v", err)
	}
	return string(b)
}

func TestValidateConfig_ReportsAScenarioTheWritersWouldRefuse_AsAWarningNamingFileAndLine(t *testing.T) {
	e := tierEnv(t, mcpTierCfg)
	e.Tier = "compose"
	md := strings.Replace(mcpScenarioWith(), "## TIMEOUT\n30s", "## TIMEOUT\n60s", 1)
	if !strings.Contains(md, "## TIMEOUT\n60s") {
		t.Fatal("the fixture did not take the 60s timeout") // a mutation that did not apply reads as green
	}
	if _, verrs := ValidateAll(md); len(verrs) == 0 {
		t.Fatal("the fixture must be a scenario the writers refuse, or this test proves nothing")
	}
	os.MkdirAll(filepath.Join(e.ScenariosDir, "mcp"), 0o755)
	os.WriteFile(filepath.Join(e.ScenariosDir, "mcp", "MCP-001-slow.md"), []byte(md), 0o644)

	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if failed {
		t.Fatalf("a scenario rule is reported, it does not make the CONFIG invalid; errors: %s", errorText(payload.(map[string]any)))
	}
	text := warningText(t, payload)
	for _, want := range []string{"MCP-001-slow.md", "## TIMEOUT 60s exceeds the ceiling", "line ", "refuse"} {
		if !strings.Contains(text, want) {
			t.Errorf("the warnings lack %q; got %s", want, text)
		}
	}
}

func TestValidateConfig_ACleanKitGetsNoScenarioRuleWarning(t *testing.T) {
	e := tierEnv(t, mcpTierCfg)
	e.Tier = "compose"
	md := mcpScenarioWith()
	if _, verrs := ValidateAll(md); len(verrs) != 0 {
		t.Fatalf("the clean fixture is not clean: %+v", verrs)
	}
	os.MkdirAll(filepath.Join(e.ScenariosDir, "mcp"), 0o755)
	os.WriteFile(filepath.Join(e.ScenariosDir, "mcp", "MCP-001-ok.md"), []byte(md), 0o644)
	payload, _, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if text := warningText(t, payload); strings.Contains(text, "MCP-001-ok.md") {
		t.Errorf("a clean scenario was reported: %s", text)
	}
}

// A problem validate-config already reports as an ERROR (the EXPECT classifier) is not said a second time as a warning.
func TestValidateConfig_AnExpectProblemIsAnErrorOnly_NotAlsoAWarning(t *testing.T) {
	e := tierEnv(t, mcpTierCfg)
	e.Tier = "compose"
	bullet := `content[0].text == "document not found"`
	os.MkdirAll(filepath.Join(e.ScenariosDir, "mcp"), 0o755)
	os.WriteFile(filepath.Join(e.ScenariosDir, "mcp", "MCP-001-content.md"), []byte(mcpScenarioWith(bullet)), 0o644)
	payload, failed, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if !failed {
		t.Fatal("the EXPECT refusal stays an error")
	}
	if text := warningText(t, payload); strings.Contains(text, bullet[:20]) {
		t.Errorf("the EXPECT problem is repeated as a warning: %s", text)
	}
}

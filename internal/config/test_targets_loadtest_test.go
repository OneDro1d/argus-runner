package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// `load_test: never` on a test_targets[] entry. Key, refusal, validate-config error and the
// run-time refusal text. (The executor's door that USES the text is tested in internal/argus.)

const ltNeverCfg = brokerTargets + `load_allowed_targets:
  load-lab:
    max_sessions: 1000
test_targets:
  - name: live
    label: msgbus live
    load_test: never
    match: { scenario_prefixes: [L-] }
  - name: lab
    match: { scenario_prefixes: [H-] }
`

func httpLoadScenario(id string) *scenario.Scenario {
	return scenario.Parse(strings.Join([]string{
		"# Scenario: t", "", "## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: http", "",
		"## TRIGGER", "GET `/ping`", "", "## EXPECT", "### Runnable", "- status is 200", "",
		"## LOAD", "- **Users**: 5", "- **Ramp Seconds**: 5", "- **Duration Seconds**: 30", "- **Target P95 Ms**: 250", "- **Max Error Rate**: 0.01", "",
		"## TIMEOUT", "30s", "",
	}, "\n"))
}

func plainScenario(id string) *scenario.Scenario {
	return scenario.Parse(strings.Join([]string{
		"# Scenario: t", "", "## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: http", "",
		"## TRIGGER", "GET `/ping`", "", "## EXPECT", "### Runnable", "- status is 200", "", "## TIMEOUT", "30s", "",
	}, "\n"))
}

func TestLoadTest_KeyParses_AbsentIsToday(t *testing.T) {
	c, err := loadFrom(t, ltNeverCfg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.TestTargets[0].NeverLoadTested() || c.TestTargets[1].NeverLoadTested() {
		t.Fatalf("TestTargets = %#v", c.TestTargets)
	}
}

func TestLoadTest_OtherValueRefusedByNameAtLoad(t *testing.T) {
	_, err := loadFrom(t, ttBase+"test_targets:\n  - {name: live, load_test: sometimes, match: {tags: [x]}}\n")
	if err == nil {
		t.Fatal("load_test: sometimes was accepted")
	}
	for _, w := range []string{"load_test", "live", "never"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal %q lacks %q", err, w)
		}
	}
}

func TestLoadTest_NeverRefusalNamesTargetAndKey_ForAMQPAndHTTPLoad(t *testing.T) {
	c, err := loadFrom(t, ltNeverCfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*scenario.Scenario{"amqp": amqpScenario("load-lab", "10"), "http ## LOAD": httpLoadScenario("L-002")} {
		r := c.NeverLoadRefusal(s)
		if r == "" {
			t.Errorf("%s: a load check of a never target was not refused", name)
			continue
		}
		for _, w := range []string{`"live"`, "load_test: never", "Nothing was sent"} {
			if !strings.Contains(r, w) {
				t.Errorf("%s: refusal lacks %q: %s", name, w, r)
			}
		}
		for _, leak := range []string{"load-lab.invalid", "s3cr3t", "amqp://"} {
			if strings.Contains(r, leak) {
				t.Errorf("%s: refusal leaks %q", name, leak)
			}
		}
	}
}

func TestLoadTest_NeverRefusalLeavesOtherChecksAlone(t *testing.T) {
	c, err := loadFrom(t, ltNeverCfg)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.NeverLoadRefusal(plainScenario("L-003")); r != "" {
		t.Errorf("a NON-load check of a never target was refused: %s", r)
	}
	if r := c.NeverLoadRefusal(httpLoadScenario("H-001")); r != "" {
		t.Errorf("a load check of a target that does not declare the key was refused: %s", r)
	}
	c2, _ := loadFrom(t, ttBase)
	if r := c2.NeverLoadRefusal(httpLoadScenario("L-002")); r != "" {
		t.Errorf("no test_targets at all, and a load check was refused: %s", r)
	}
}

func TestLoadTest_ValidateConfigReportsALoadCheckOfANeverTarget(t *testing.T) {
	c, err := loadFrom(t, ltNeverCfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "http")
	_ = os.MkdirAll(sub, 0o755)
	write := func(name string, s string) {
		if err := os.WriteFile(filepath.Join(sub, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	md := func(id, load string) string {
		return "# Scenario: t\n\n## Metadata\n- **ID**: " + id + "\n- **Layer**: HTTP Ingestion\n- **Tags**: http\n\n## TRIGGER\nGET `/ping`\n\n## EXPECT\n### Runnable\n- status is 200\n\n" + load + "## TIMEOUT\n30s\n"
	}
	load := "## LOAD\n- **Users**: 5\n- **Ramp Seconds**: 5\n- **Duration Seconds**: 30\n- **Target P95 Ms**: 250\n- **Max Error Rate**: 0.01\n\n"
	write("L-002.md", md("L-002", load)) // never target, load
	write("L-003.md", md("L-003", ""))   // never target, no load
	write("H-001.md", md("H-001", load)) // lab, load
	errs, _, err := c.Validate(dir)
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, e := range errs {
		if e.Scenario == "L-003" || e.Scenario == "H-001" {
			if strings.Contains(e.Message, "load_test") {
				t.Errorf("a check that must pass was reported: %+v", e)
			}
		}
		if e.Scenario == "L-002" && strings.Contains(e.Message, "L-002.md") && strings.Contains(e.Message, `"live"`) && strings.Contains(e.Message, "load_test: never") {
			hit = true
			t.Logf("VALIDATE-CONFIG ERROR: %s", e.Message)
		}
	}
	if !hit {
		t.Fatalf("validate-config did not report the load check of a never target by file, target and key: %+v", errs)
	}
}

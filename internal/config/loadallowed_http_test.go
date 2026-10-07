package config

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Promise 4: S9 applies unchanged to an HTTP Load ramp. Its http target must be listed under
// load_allowed_targets, its top step within that entry's max_sessions, and the refusal never names a URL.

const httpTargets = `project:
  name: p
targets:
  http:
    base_url: http://sut.invalid
  http_targets:
    api-lab:
      base_url: http://api-lab.invalid:8080
    api-other:
      base_url: http://api-other.invalid:8080
  message_broker_targets:
    load-lab:
      type: amqp
      url: amqp://loaduser:[REDACTED]@load-lab.invalid:5672/
`

func httpLoadScenarioFor(target, steps string) *scenario.Scenario {
	return scenario.Parse(strings.Join([]string{
		"# Scenario: t", "", "## Metadata", "- **ID**: HL-001", "- **Layer**: HTTP Load", "- **Tags**: http, load",
		"- **Target**: " + target, "",
		"## TRIGGER", "GET `/api/items`", "",
		"## EXPECT", "### Runnable", "- every step is measured", "",
		"## LOAD", "- **Steps**: " + steps, "- **Step Duration Seconds**: 60", "- **Target P95 Ms**: 250", "- **Max Error Rate**: 0.01", "",
		"## TIMEOUT", "10s", "", "## CLEANUP", "N/A — a unit-test fixture.", "",
	}, "\n"))
}

func TestLoadAllowed_HTTPLoad_UnlistedTargetIsRefusedByName(t *testing.T) {
	c, err := loadFrom(t, httpTargets+"load_allowed_targets:\n  api-lab: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := c.LoadRefusal(httpLoadScenarioFor("api-lab", "10, 20")); r != "" {
		t.Fatalf("a listed http target was refused: %s", r)
	}
	r := c.LoadRefusal(httpLoadScenarioFor("api-other", "10"))
	t.Logf("REFUSAL TEXT: %s", r)
	for _, want := range []string{"refused before firing", `http target "api-other"`, "load_allowed_targets", "dedicated test deployment", "Nothing was sent", "preflight"} {
		if !strings.Contains(r, want) {
			t.Errorf("refusal lacks %q: %s", want, r)
		}
	}
	if strings.Contains(r, "api-other.invalid") || strings.Contains(r, "http://") {
		t.Errorf("refusal leaks the target's URL: %s", r)
	}
	if m := c.loadValidateMessage(httpLoadScenarioFor("api-other", "10")); !strings.Contains(m, `http target "api-other", which load_allowed_targets does not list`) {
		t.Errorf("validate-config message = %q", m)
	}
}

func TestLoadAllowed_HTTPLoad_AbsentBlockRefusesEveryRamp(t *testing.T) {
	c, err := loadFrom(t, httpTargets)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.LoadRefusal(httpLoadScenarioFor("api-lab", "10")); r == "" {
		t.Fatal("with no load_allowed_targets an HTTP Load ramp was allowed")
	}
}

// A load_allowed_targets entry that names a BROKER does not allow a ramp at an http target of the same word: the
// operator marked the broker, not that host.
func TestLoadAllowed_HTTPLoad_ABrokerEntryDoesNotAllowAnHTTPRamp(t *testing.T) {
	c, err := loadFrom(t, httpTargets+"load_allowed_targets:\n  load-lab: {}\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := c.LoadRefusal(httpLoadScenarioFor("load-lab", "10")); !strings.Contains(r, `http target "load-lab"`) {
		t.Fatalf("an HTTP ramp at a name the operator listed only as a broker was allowed: %q", r)
	}
}

func TestLoadAllowed_HTTPLoad_TopStepAboveMaxSessionsIsRefused(t *testing.T) {
	c, err := loadFrom(t, httpTargets+"load_allowed_targets:\n  api-lab:\n    max_sessions: 50\n")
	if err != nil {
		t.Fatal(err)
	}
	r := c.LoadRefusal(httpLoadScenarioFor("api-lab", "10, 40, 80"))
	if !strings.Contains(r, "its Steps reach 80 users and load_allowed_targets.api-lab.max_sessions is 50") {
		t.Fatalf("refusal = %q", r)
	}
	if m := c.loadValidateMessage(httpLoadScenarioFor("api-lab", "10, 40, 80")); !strings.Contains(m, "Steps reach 80 users but load_allowed_targets.api-lab.max_sessions is 50") {
		t.Errorf("validate-config message = %q", m)
	}
}

func TestLoadAllowed_AnHTTPTargetEntryIsAccepted_AndAProductionLookingOneIsRefused(t *testing.T) {
	if _, err := loadFrom(t, httpTargets+"load_allowed_targets:\n  api-lab: {}\n"); err != nil {
		t.Fatalf("an entry naming a targets.http_targets entry was refused at load: %v", err)
	}
	prod := strings.Replace(httpTargets, "http://api-lab.invalid:8080", "https://api.prod.example.com", 1)
	_, err := loadFrom(t, prod+"load_allowed_targets:\n  api-lab: {}\n")
	if err == nil || !strings.Contains(err.Error(), "load_allowed_targets.api-lab: its base_url looks shared/production") {
		t.Fatalf("a production-looking http target was accepted as a load target: %v", err)
	}
	if strings.Contains(err.Error(), "api.prod.example.com") {
		t.Errorf("the refusal echoes the URL: %v", err)
	}
}

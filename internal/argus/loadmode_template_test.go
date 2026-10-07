package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-11 — THE Go↔JMX LOAD CONTRACT, GUARDED (same reasoning as
// TestTemplateReadsTheBodyPropertiesDeriveEmits): DeriveProps computes load.users/load.ramp/
// load.duration in Go; every JMX template's MAIN thread group must read them, with the SAME
// defaults DeriveProps relies on to leave an unprofiled scenario unchanged.
var loadDrivenTemplates = []string{
	"http-ingestion.jmx", "http-idempotency.jmx", "database-state.jmx",
	"message-flow.jmx", "external-delivery.jmx", "saga-presence.jmx", "cleanup-sql.jmx",
}

func TestTemplatesReadTheLoadPropertiesDeriveEmits(t *testing.T) {
	for _, name := range loadDrivenTemplates {
		t.Run(name, func(t *testing.T) {
			jmx := readTemplate(t, name)
			for _, want := range []string{
				`${__P(load.users,1)}`,
				`${__P(load.ramp,0)}`,
				`${__P(load.scheduler,false)}`,
				`${__P(load.duration,0)}`,
			} {
				if !strings.Contains(jmx, want) {
					t.Errorf("%s does not read %q — a load profile would not reach the thread group", name, want)
				}
			}
		})
	}
}

func minimalLoadScenario(t *testing.T, loadBlock string) *scenario.Scenario {
	t.Helper()
	lines := []string{
		"# Scenario: t", "", "## Metadata",
		"- **ID**: T-001", "- **Layer**: HTTP Ingestion", "- **Tags**: http, t", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
		"## EXPECT", "### Runnable", "- status=202", "",
	}
	if loadBlock != "" {
		lines = append(lines, "## LOAD", loadBlock, "")
	}
	lines = append(lines, "## TIMEOUT", "30s", "", "## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	return scenario.Parse(strings.Join(lines, "\n"))
}

// A scenario with NO `## LOAD` emits NO load.* properties at all — the default
// `${__P(load.users,1)}` etc. in the template is what applies, so the request stream a
// pre-existing scenario fires is BYTE-IDENTICAL to before AC-11.
func TestDeriveProps_NoLoadProfileEmitsNoLoadProperties(t *testing.T) {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	s := minimalLoadScenario(t, "")
	if s.Load != nil {
		t.Fatalf("fixture must carry no Load, got %+v", s.Load)
	}
	props, err := DeriveProps(c, s, "tr-t")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	for _, k := range []string{"load.users", "load.ramp", "load.scheduler", "load.duration"} {
		if v, ok := props[k]; ok {
			t.Errorf("a scenario with no ## LOAD must emit no %q, got %q — the default would no longer apply", k, v)
		}
	}
}

// A declared `## LOAD` reaches DeriveProps as load.users / load.ramp / load.scheduler / load.duration.
func TestDeriveProps_LoadProfileDrivesThreadGroupProperties(t *testing.T) {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	body := "- **Users**: 50\n- **Ramp Seconds**: 10\n- **Duration Seconds**: 60\n" +
		"- **Target P95 Ms**: 300\n- **Max Error Rate**: 0.05"
	s := minimalLoadScenario(t, body)
	if s.Load == nil {
		t.Fatalf("fixture must parse a Load profile")
	}
	props, err := DeriveProps(c, s, "tr-t")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	want := map[string]string{
		"load.users": "50", "load.ramp": "10", "load.scheduler": "true", "load.duration": "60",
	}
	for k, v := range want {
		if got := props[k]; got != v {
			t.Errorf("props[%q] = %q, want %q", k, got, v)
		}
	}
}

// DurationSeconds == 0 ("loop-count only, no scheduler cutoff") emits users/ramp but leaves
// scheduler/duration unset — the template default (scheduler=false) applies, exactly as an
// unprofiled scenario, so a burst-only profile does not accidentally cap the run at 0 seconds.
func TestDeriveProps_LoadProfileZeroDurationOmitsScheduler(t *testing.T) {
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	body := "- **Users**: 5\n- **Ramp Seconds**: 0\n- **Duration Seconds**: 0\n" +
		"- **Target P95 Ms**: 300\n- **Max Error Rate**: 0.05"
	s := minimalLoadScenario(t, body)
	props, err := DeriveProps(c, s, "tr-t")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}
	if props["load.users"] != "5" {
		t.Errorf("load.users = %q, want 5", props["load.users"])
	}
	if _, ok := props["load.scheduler"]; ok {
		t.Errorf("DurationSeconds=0 must not set load.scheduler, got %q", props["load.scheduler"])
	}
	if _, ok := props["load.duration"]; ok {
		t.Errorf("DurationSeconds=0 must not set load.duration, got %q", props["load.duration"])
	}
}

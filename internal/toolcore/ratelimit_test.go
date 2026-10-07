package toolcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rlEnv writes a config + N scenarios of the given shape and returns the Env validate_config reads.
func rlEnv(t *testing.T, cfgBody string, scenarios map[string]string) Env {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	sc := filepath.Join(dir, "scenarios", "http-ingestion")
	if err := os.MkdirAll(sc, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, md := range scenarios {
		if err := os.WriteFile(filepath.Join(sc, id+".md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Env{ConfigPath: p, ScenariosDir: filepath.Join(dir, "scenarios")}
}

func plainScenario(id, expect string) string {
	return strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "", "## EXPECT", "### Runnable", "- " + expect, "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

// a chain scenario is SEVERAL calls, not one — the estimate must count its steps.
func chainScenario(id string, steps int) string {
	tick := "\x60"
	var b strings.Builder
	b.WriteString(strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion",
		"- **Tags**: chain", "", "## TRIGGER", "POST " + tick + "${MCP_URL}" + tick, "",
		tick + tick + tick + "json", `{"steps":[`,
	}, "\n"))
	for i := 0; i < steps; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"mcp","name":"s","tool":"t","args":{}}`)
	}
	b.WriteString("]}\n" + tick + tick + tick + "\n\n## EXPECT\n- result.isError == false\n")
	return b.String()
}

const rlTargets = "project:\n  name: t\ntargets:\n  http:\n    base_url: http://x:8080\n  mcp:\n    base_url: http://x:9000/mcp\n"

const rlCfgDeclared = rlTargets +
	"rate_limit:\n  requests: 3\n  per: minute\n  signature:\n    body_contains: rate_limited\n"

const rlCfgUndeclared = rlTargets

// D11: validate_config reports how many requests the pack will send — scenarios PLUS chain steps,
// because a chain is several calls, not one.
func TestValidateConfig_RequestsEstimatedCountsChainSteps(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	e := rlEnv(t, rlCfgUndeclared, map[string]string{
		"ORD-001": plainScenario("ORD-001", "status=202"),
		"ORD-002": plainScenario("ORD-002", "status=202"),
		"CHN-001": chainScenario("CHN-001", 4),
	})
	p, _, err := ValidateConfig(e)
	if err != nil {
		t.Fatal(err)
	}
	m := p.(map[string]any)
	if got := m["requests_estimated"]; got != 6 {
		t.Fatalf("requests_estimated = %v; want 6 (two single-call scenarios + a 4-step chain)", got)
	}
}

// D11, the trigger: BOTH conditions or no warning — a rate_limit is declared AND the pack exceeds
// it. And D11 corrected 2026-09-05: it is INFORMATION ONLY. valid stays true and nothing blocks.
func TestValidateConfig_RateLimitWarningFiresOnlyWhenJustified(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")

	// declared 3/minute, pack sends 4 → warn.
	over := rlEnv(t, rlCfgDeclared, map[string]string{
		"ORD-001": plainScenario("ORD-001", "status=202"),
		"CHN-001": chainScenario("CHN-001", 3),
	})
	p, hadErr, err := ValidateConfig(over)
	if err != nil {
		t.Fatal(err)
	}
	if hadErr {
		t.Error("the pre-run warning is INFORMATION ONLY — it must never make validate_config fail")
	}
	m := p.(map[string]any)
	if m["valid"] != true {
		t.Errorf("valid = %v; the warning must not invalidate the config", m["valid"])
	}
	warns, _ := m["warnings"].([]string)
	line := ""
	for _, w := range warns {
		if strings.Contains(w, "requests") && strings.Contains(w, "pause") {
			line = w
		}
	}
	if line == "" {
		t.Fatalf("no rate-limit warning in %v", warns)
	}
	for _, want := range []string{"~4 requests", "3/minute", "retry once"} {
		if !strings.Contains(line, want) {
			t.Errorf("the warning must carry %q — it is what tells the operator what may happen: %q", want, line)
		}
	}

	// declared 3/minute, pack sends 2 → no warning.
	under := rlEnv(t, rlCfgDeclared, map[string]string{
		"ORD-001": plainScenario("ORD-001", "status=202"),
		"ORD-002": plainScenario("ORD-002", "status=202"),
	})
	p2, _, _ := ValidateConfig(under)
	for _, w := range p2.(map[string]any)["warnings"].([]string) {
		if strings.Contains(w, "pause") {
			t.Errorf("a pack within the limit produced a warning: %q", w)
		}
	}

	// no rate_limit declared → never a warning, whatever the pack size.
	none := rlEnv(t, rlCfgUndeclared, map[string]string{
		"ORD-001": plainScenario("ORD-001", "status=202"),
		"CHN-001": chainScenario("CHN-001", 9),
	})
	p3, _, _ := ValidateConfig(none)
	for _, w := range p3.(map[string]any)["warnings"].([]string) {
		if strings.Contains(w, "pause") {
			t.Errorf("an undeclared SUT produced a rate-limit warning: %q", w)
		}
	}
	if _, ok := p3.(map[string]any)["rate_limit"]; ok {
		t.Error("an undeclared SUT must not report a rate_limit block")
	}
}

// D11: validate_config echoes the declared limit, so the reader can check the estimate against it
// instead of taking our arithmetic on trust.
func TestValidateConfig_EchoesTheDeclaredLimit(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	e := rlEnv(t, rlCfgDeclared, map[string]string{"ORD-001": plainScenario("ORD-001", "status=202")})
	p, _, err := ValidateConfig(e)
	if err != nil {
		t.Fatal(err)
	}
	rl, ok := p.(map[string]any)["rate_limit"].(map[string]any)
	if !ok {
		t.Fatalf("the declared limit is not echoed: %+v", p)
	}
	if rl["requests"] != 3 || rl["per"] != "minute" {
		t.Errorf("rate_limit echo = %+v; want 3/minute", rl)
	}
}

// D11 / SA §1.4.R1: runner__run echoes the SAME warning — the operator who starts a run without
// validating first must still be told what may happen. It is information only: the run still runs.
func TestRun_EchoesTheRateLimitWarning(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	// Two mcp scenarios against an endpoint that answers nothing: the run is real, both scenarios
	// come back not-measured, and the WARNING is what this test is about.
	unreachable := "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n" +
		"rate_limit:\n  requests: 1\n  per: minute\n  signature:\n    body_contains: rate_limited\n"
	e := rlEnv(t, unreachable, map[string]string{
		"MCP-001": mcpScenarioForEstimate("MCP-001"),
		"MCP-002": mcpScenarioForEstimate("MCP-002"),
	})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"

	p, _, err := Run(e, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := p.(map[string]any)["rate_limit_warning"].(string)
	if !strings.Contains(w, "~2 requests") || !strings.Contains(w, "1/minute") {
		t.Fatalf("runner__run must echo the same warning; got %q", w)
	}
}

// with no rate_limit declared, runner__run says nothing about rate limits at all.
func TestRun_NoWarningWhenUndeclared(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	e := rlEnv(t, "project:\n  name: t\ntargets:\n  mcp:\n    base_url: http://127.0.0.1:1/mcp\n",
		map[string]string{"MCP-001": mcpScenarioForEstimate("MCP-001")})
	e.ResultsRoot = t.TempDir()
	e.Instance = "local"

	p, _, err := Run(e, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(map[string]any)["rate_limit_warning"]; ok {
		t.Fatal("an undeclared SUT got a rate-limit warning")
	}
}

func mcpScenarioForEstimate(id string) string {
	tick := "\x60"
	return strings.Join([]string{
		"# Scenario: " + id, "", "## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion",
		"- **Tags**: mcp", "", "## TRIGGER", "POST " + tick + "${MCP_URL}" + tick, "",
		tick + tick + tick + "json", `{"transport":"streamable-http","tool":"t","args":{}}`,
		tick + tick + tick, "", "## EXPECT", "### Runnable", "- result.isError == false", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

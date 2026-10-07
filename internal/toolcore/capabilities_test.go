package toolcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func capEnv(t *testing.T, body string) Env {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Env{ConfigPath: p, ScenariosDir: dir}
}

// Onboarding policy (warn-only): validate-config surfaces a nudge for each undeclared log-field
// translation-table entry, but STILL PASSES (valid:true, no errors) — a non-breaking reminder.
func TestValidateConfig_LogFieldWarnings(t *testing.T) {
	// INT-019 added a reachability probe to ValidateConfig. This test is about CONFIG nudges, and its
	// base_url host `x` would cost a real 2s DNS timeout on every run — so the dial is switched off here.
	// Nothing this test asserts is weakened: reachability reports into its own key.
	t.Setenv(probeDisableEnv, "1")
	// declares none of the four -> valid:true + 4 warnings.
	e := capEnv(t, "project:\n  name: bare\ntargets:\n  http:\n    base_url: http://x:8080\n")
	p, hadErr, err := ValidateConfig(e)
	if err != nil {
		t.Fatal(err)
	}
	if hadErr {
		t.Error("warn-only: validate-config must still pass (warnings are NOT errors), got hadErr=true")
	}
	m := p.(map[string]any)
	if m["valid"] != true {
		t.Errorf("valid must be true with only warnings, got %v", m["valid"])
	}
	warns, _ := m["warnings"].([]string)
	if len(warns) != 4 {
		t.Fatalf("want 4 undeclared-field nudges, got %d: %v", len(warns), warns)
	}
	// all four declared -> zero warnings.
	e2 := capEnv(t, "project:\n  name: social\ntargets:\n  http:\n    base_url: http://x:8080\nobservability:\n  loki:\n    correlation_field: request_id\n    level_field: level\n    saga_event_field: event\n    saga_event_value: tool_dispatch\n")
	p2, _, _ := ValidateConfig(e2)
	if w, _ := p2.(map[string]any)["warnings"].([]string); len(w) != 0 {
		t.Errorf("all four declared -> 0 warnings, got %v", w)
	}
}

// CHANGE-4: an MCP SUT with no /metrics and no DB (Memstore shape) reports its surface as
// DECLARED GAPS — behavioural-MCP-testable, with metrics/request-rate flagged — never a failure.
func TestCapabilities_MemstoreHonestGaps(t *testing.T) {
	// memstore shape: MCP, no /metrics, no DB, with SUT-declared correlation + saga gaps.
	e := capEnv(t, "project:\n  name: memstore\ntargets:\n  mcp:\n    base_url: http://memstore-gateway:8090/mcp\n    transport: streamable-http\n    auth:\n      type: none\ndeclared_gaps:\n  - \"correlation: no inbound correlation propagation\"\n  - \"saga: none emitted (no event_type=saga)\"\nobservability:\n  prometheus:\n    enabled: false\n")
	p, err := Capabilities(e)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	b, _ := json.Marshal(p)
	s := string(b)
	if !strings.Contains(s, "MCP-testable (streamable-http, auth none)") {
		t.Errorf("memstore summary must report MCP-testable + transport/auth: %s", s)
	}
	if !strings.Contains(s, "declared gap(s) — see declared_gaps") {
		t.Errorf("memstore summary must headline declared gaps: %s", s)
	}
	// declared_gaps must MERGE the SUT-declared (correlation, saga) with the derived DB gap.
	// (The /metrics request-rate gap is retired — the request/error-rate panel is runner-owned.)
	for _, want := range []string{"no inbound correlation propagation", "no event_type=saga", "Database-State"} {
		if !strings.Contains(s, want) {
			t.Errorf("memstore declared_gaps must include %q (declared + derived merged): %s", want, s)
		}
	}
	// the retired /metrics request-rate gap must NOT reappear.
	if strings.Contains(s, "request-rate") || strings.Contains(s, "request_rate_query") {
		t.Errorf("the /metrics request-rate gap is retired and must not be reported: %s", s)
	}
	// it is reported, NOT failed.
	if !strings.Contains(s, "NOT a failure") {
		t.Errorf("a declared gap must be framed as reported-not-failed: %s", s)
	}
}

// metrics.declared is NEUTRAL info (does the SUT expose /metrics) — it is NOT a dashboard
// gap anymore (the request/error-rate panel is runner-owned, argus_sut_requests_total). A
// SUT with prometheus enabled reports metrics.declared:true and gets NO metrics gap.
func TestCapabilities_MetricsNeutralNoGap(t *testing.T) {
	// SECRETS-VAR-PARITY (D1): the config references ${SOCIAL_MCP_TOKEN}; declare it so Load
	// resolves it (an unset referenced ${VAR} now hard-fails — the point of this build).
	t.Setenv("SOCIAL_MCP_TOKEN", "smcp_test")
	e := capEnv(t, "project:\n  name: social\ntargets:\n  mcp:\n    base_url: http://social-gateway:8080/mcp\n    auth:\n      type: bearer\n      bearer_token: ${SOCIAL_MCP_TOKEN}\nobservability:\n  loki:\n    correlation_field: request_id\n  prometheus:\n    enabled: true\n")
	p, err := Capabilities(e)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	b, _ := json.Marshal(p)
	s := string(b)
	// metrics is reported as neutral info, never as a request_rate_query sub-field or a gap.
	if !strings.Contains(s, `"metrics":{"declared":true}`) {
		t.Errorf("metrics.declared must be reported as neutral info (declared:true), no request_rate_query sub-field: %s", s)
	}
	if strings.Contains(s, "request-rate") || strings.Contains(s, "request_rate_query") {
		t.Errorf("the /metrics request-rate gap is retired — it must not be reported: %s", s)
	}
	if !strings.Contains(s, `"auth":"bearer"`) {
		t.Errorf("social must report bearer auth mode: %s", s)
	}
	if !strings.Contains(s, `"correlation_field":"request_id"`) {
		t.Errorf("social must report the declared correlation_field=request_id: %s", s)
	}
}

// validate-config says so when an http base_url carries a path the runner will drop.
// Warn-only — the config stays valid, exactly as before.
func TestValidateConfig_WarnsWhenBaseURLCarriesAPath(t *testing.T) {
	t.Setenv(probeDisableEnv, "1")
	e := capEnv(t, "project:\n  name: documenso\ntargets:\n  http:\n    base_url: http://documenso:3100/api/v2\nobservability:\n  loki:\n    correlation_field: request_id\n    level_field: level\n    saga_event_field: event\n    saga_event_value: tool_dispatch\n")
	p, hadErr, err := ValidateConfig(e)
	if err != nil {
		t.Fatal(err)
	}
	if hadErr {
		t.Error("a base_url path is advisory: validate-config must still pass")
	}
	warns, _ := p.(map[string]any)["warnings"].([]string)
	if len(warns) != 1 || !strings.Contains(warns[0], "/api/v2") {
		t.Fatalf("want exactly the base_url path warning, got %q", warns)
	}
}

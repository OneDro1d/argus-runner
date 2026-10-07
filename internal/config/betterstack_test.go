package config

import (
	"os"
	"strings"
	"testing"
)

// AC-D13: observability.betterstack lets an argus-config select BetterStack (Telemetry)
// instead of the bundled Loki as the log/saga source. Exactly one of observability.loki /
// observability.betterstack may be declared — never both — and validation says so BY NAME
// (never a generic "invalid config").

const bsBase = "project:\n  name: t\ntargets:\n  http:\n    base_url: http://sut:8080\n"

func bsMinimalBlock() string {
	return `observability:
  betterstack:
    query_url: https://eu-nbg-2-connect.betterstackdata.com
    credential: ${BS_CREDENTIAL}
    team_id: "123456"
    sources:
      accounting: accounting-service
      trade_execution: trade-execution-service
`
}

// Both loki AND betterstack declared → refused BY NAME, never silently picking one.
func TestBetterStack_BothLokiAndBetterStackSet_Refused(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "user:pass")
	body := bsBase + `observability:
  loki:
    url: http://loki:3100
  betterstack:
    query_url: https://eu-nbg-2-connect.betterstackdata.com
    credential: ${BS_CREDENTIAL}
    team_id: "123456"
    sources:
      accounting: accounting-service
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("both loki and betterstack declared must be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "observability.loki") || !strings.Contains(err.Error(), "observability.betterstack") {
		t.Errorf("refusal must name BOTH blocks, got: %v", err)
	}
}

// Neither declared → unchanged default behaviour (falls back to Loki's bundled defaults);
// this must NOT be a validation error — every pre-existing argus-config omits this entirely.
func TestBetterStack_NeitherSet_DefaultsToLokiUnchanged(t *testing.T) {
	c, err := loadCfgText(t, bsBase)
	if err != nil {
		t.Fatalf("a config with no observability.loki/betterstack must still load: %v", err)
	}
	if c.UseBetterStack() {
		t.Error("UseBetterStack() must be false when neither block is declared")
	}
	if c.LokiURL() != "http://loki:3100" {
		t.Errorf("LokiURL() must keep its bundled default, got %q", c.LokiURL())
	}
}

// A source name that is not a safe identifier (spaces, quotes, SQL metacharacters) is refused
// BY NAME at load time — it must never reach query construction.
func TestBetterStack_BadSourceName_Refused(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "user:pass")
	body := bsBase + `observability:
  betterstack:
    query_url: https://eu-nbg-2-connect.betterstackdata.com
    credential: ${BS_CREDENTIAL}
    team_id: "123456"
    sources:
      "accounting'; DROP TABLE x; --": accounting-service
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("an unsafe source name must be refused at load time")
	}
	if !strings.Contains(err.Error(), "observability.betterstack.sources") {
		t.Errorf("refusal must name observability.betterstack.sources, got: %v", err)
	}
}

// The team id is built into the table name the same way, so an unsafe one is refused by name too.
func TestBetterStack_BadTeamID_Refused(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "user:pass")
	body := bsBase + `observability:
  betterstack:
    query_url: https://eu-nbg-2-connect.betterstackdata.com
    credential: ${BS_CREDENTIAL}
    team_id: "1_x') UNION SELECT 1 --"
    sources:
      accounting: accounting-service
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("an unsafe team id must be refused at load time")
	}
	if !strings.Contains(err.Error(), "observability.betterstack.team_id") {
		t.Errorf("refusal must name observability.betterstack.team_id, got: %v", err)
	}
}

// The credential MUST be a ${VAR} reference — never a literal secret committed to the file.
func TestBetterStack_LiteralCredential_Refused(t *testing.T) {
	body := bsBase + `observability:
  betterstack:
    query_url: https://eu-nbg-2-connect.betterstackdata.com
    credential: sk-super-secret-literal-value
    team_id: "123456"
    sources:
      accounting: accounting-service
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("a literal credential must be refused")
	}
	if !strings.Contains(err.Error(), "observability.betterstack.credential") {
		t.Errorf("refusal must name observability.betterstack.credential, got: %v", err)
	}
	if strings.Contains(err.Error(), "sk-super-secret-literal-value") {
		t.Errorf("refusal must NEVER echo the literal secret value back, got: %v", err)
	}
}

// A referenced-but-unset ${VAR} fails loud (D1 parity with every other credential field).
func TestBetterStack_UnsetEnvVar_FailsLoud(t *testing.T) {
	os.Unsetenv("BS_CREDENTIAL_MISSING")
	body := bsBase + `observability:
  betterstack:
    query_url: https://eu-nbg-2-connect.betterstackdata.com
    credential: ${BS_CREDENTIAL_MISSING}
    team_id: "123456"
    sources:
      accounting: accounting-service
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("an unresolved ${VAR} credential must fail loud")
	}
	if !strings.Contains(err.Error(), "BS_CREDENTIAL_MISSING") {
		t.Errorf("refusal must name the missing var, got: %v", err)
	}
}

// query_url / team_id / sources are each required, by name.
func TestBetterStack_MissingRequiredFields_RefusedByName(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "user:pass")
	cases := []struct {
		name, body, mustContain string
	}{
		{"no query_url", bsBase + "observability:\n  betterstack:\n    credential: ${BS_CREDENTIAL}\n    team_id: \"1\"\n    sources: {a: svc}\n", "observability.betterstack.query_url"},
		{"no team_id", bsBase + "observability:\n  betterstack:\n    query_url: https://x\n    credential: ${BS_CREDENTIAL}\n    sources: {a: svc}\n", "observability.betterstack.team_id"},
		{"no sources", bsBase + "observability:\n  betterstack:\n    query_url: https://x\n    credential: ${BS_CREDENTIAL}\n    team_id: \"1\"\n", "observability.betterstack.sources"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadCfgText(t, c.body)
			if err == nil {
				t.Fatal("must be refused")
			}
			if !strings.Contains(err.Error(), c.mustContain) {
				t.Errorf("refusal must name %q, got: %v", c.mustContain, err)
			}
		})
	}
}

// Accessors: defaults + explicit values, resolved credential (D1/D2 parity — expandEnv runs it
// through the SAME mechanism as every other credential field).
func TestBetterStack_Accessors(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "svc-user:svc-pass")
	c, err := loadCfgText(t, bsBase+bsMinimalBlock())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.UseBetterStack() {
		t.Fatal("UseBetterStack() must be true when the block is declared")
	}
	if c.BetterStackQueryURL() != "https://eu-nbg-2-connect.betterstackdata.com" {
		t.Errorf("BetterStackQueryURL() = %q", c.BetterStackQueryURL())
	}
	if c.BetterStackCredential() != "svc-user:svc-pass" {
		t.Errorf("BetterStackCredential() must resolve the ${VAR}, got %q", c.BetterStackCredential())
	}
	if c.BetterStackTeamID() != "123456" {
		t.Errorf("BetterStackTeamID() = %q", c.BetterStackTeamID())
	}
	if got := c.BetterStackSources(); got["accounting"] != "accounting-service" || got["trade_execution"] != "trade-execution-service" {
		t.Errorf("BetterStackSources() = %v", got)
	}
	// default correlation field paths: message_json.correlationId, THEN correlation_id.
	if got := c.BetterStackCorrelationFields(); len(got) != 2 || got[0] != "message_json.correlationId" || got[1] != "correlation_id" {
		t.Errorf("BetterStackCorrelationFields() default = %v", got)
	}
	if c.BetterStackLevelField() != "level" {
		t.Errorf("BetterStackLevelField() default = %q, want level", c.BetterStackLevelField())
	}
	if c.BetterStackSagaEventField() != "event_type" {
		t.Errorf("BetterStackSagaEventField() default = %q, want event_type", c.BetterStackSagaEventField())
	}
	if got := c.BetterStackSagaEventValues(); len(got) != 1 || got[0] != "saga" {
		t.Errorf("BetterStackSagaEventValues() default = %v, want [saga]", got)
	}
}

// Explicit correlation_fields / level_field / saga fields override the defaults.
func TestBetterStack_ExplicitFieldOverrides(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "u:p")
	body := bsBase + `observability:
  betterstack:
    query_url: https://x
    credential: ${BS_CREDENTIAL}
    team_id: "1"
    sources:
      gw: gateway
    correlation_fields: ["request_id"]
    level_field: lvl
    saga_event_field: event
    saga_event_values: ["tool_dispatch", "ghost_dispatch"]
`
	c, err := loadCfgText(t, body)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.BetterStackCorrelationFields(); len(got) != 1 || got[0] != "request_id" {
		t.Errorf("correlation_fields override = %v", got)
	}
	if c.BetterStackLevelField() != "lvl" {
		t.Errorf("level_field override = %q", c.BetterStackLevelField())
	}
	if c.BetterStackSagaEventField() != "event" {
		t.Errorf("saga_event_field override = %q", c.BetterStackSagaEventField())
	}
	if got := c.BetterStackSagaEventValues(); len(got) != 2 || got[0] != "tool_dispatch" || got[1] != "ghost_dispatch" {
		t.Errorf("saga_event_values override = %v", got)
	}
}

// An unsafe field path (correlation_fields) is refused, same reasoning as source names: these
// names get built into JSONExtractString(...) SQL fragments and must never carry SQL syntax.
func TestBetterStack_BadFieldPath_Refused(t *testing.T) {
	t.Setenv("BS_CREDENTIAL", "u:p")
	body := bsBase + `observability:
  betterstack:
    query_url: https://x
    credential: ${BS_CREDENTIAL}
    team_id: "1"
    sources:
      gw: gateway
    correlation_fields: ["message_json.correlationId'); DROP TABLE x; --"]
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("an unsafe correlation field path must be refused at load time")
	}
	if !strings.Contains(err.Error(), "observability.betterstack.correlation_fields") {
		t.Errorf("refusal must name observability.betterstack.correlation_fields, got: %v", err)
	}
}

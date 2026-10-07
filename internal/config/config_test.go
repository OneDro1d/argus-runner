package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RO-10 / D5: the argus-owned block (deploy + observability.{loki,prometheus})
// must be PARSED from argus-config.yaml. Before this round observability.loki.url /
// observability.prometheus.* were dead keys (only observability.grafana was read),
// so the obs stack could not be parameterized per SUT and "one config" was a lie.
const argusBlockYAML = `
project:
  name: my-service
targets:
  http:
    base_url: http://my-api:8080
scenarios:
  timeout_default: 20s
deploy:
  compose_project: my-sut
  network: my-sut_default
observability:
  loki:
    url: http://loki:3100
    level_field: lvl
    error_match: "(?i)error|warn|fatal"
    derive_project_from_container: true
  prometheus:
    enabled: true
    endpoint: /metrics
    targets: ["my-api:9090", "my-worker:9090"]
  grafana:
    dashboard_template: dashboards/argus-overview.json
`

// The three tests that stood here — TestGrafanaPublicURL_DefaultAndOverride,
// _EnvOverridesDefaultButNotExplicitConfig and _UnsetEnvKeepsTheLocalhostDefault — asserted the
// PRE-M3-FX contract: a scalar public_url, an environment fallback, and a built-in localhost default.
// Section E removed all three (VR-E3/E8), so they are replaced rather than adjusted: they were
// correct tests of a behaviour that no longer exists, and keeping a weakened version of them would
// assert a contract nobody holds.
//
// Their replacements live in tiermap_test.go, and the difference is the point:
//   - the scalar form is now a PARSE error carrying the migration shape;
//   - a tier with no declared value REFUSES, naming the tier and the field;
//   - the environment rescues nothing, because a silent fallback is what let a wrong config look
//     correct for weeks (INT-008).

func TestDeploymentProbe_Accessors(t *testing.T) {
	// absent → no auto-detect (both accessors empty)
	c := loadYAML(t, "project:\n  name: s\n")
	if c.DeploymentProbeURL() != "" || c.DeploymentProbeField() != "" {
		t.Errorf("absent probe = (%q,%q), want empty", c.DeploymentProbeURL(), c.DeploymentProbeField())
	}
	// declared → resolved + trimmed
	c2 := loadYAML(t, "project:\n  name: s\ndeploy:\n  deployment_probe:\n    url: \"  http://order-api:8080/healthz \"\n    fingerprint_field: version\n")
	if got := c2.DeploymentProbeURL(); got != "http://order-api:8080/healthz" {
		t.Errorf("DeploymentProbeURL = %q, want the trimmed url", got)
	}
	if got := c2.DeploymentProbeField(); got != "version" {
		t.Errorf("DeploymentProbeField = %q, want version", got)
	}
}

func loadYAML(t *testing.T, body string) *Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestParse_ArgusOwnedBlock(t *testing.T) {
	c := loadYAML(t, argusBlockYAML)

	// deploy: (RO-05/07 — SUT compose project + network)
	if c.Deploy.ComposeProject != "my-sut" {
		t.Errorf("deploy.compose_project = %q, want my-sut", c.Deploy.ComposeProject)
	}
	if c.Deploy.Network != "my-sut_default" {
		t.Errorf("deploy.network = %q, want my-sut_default", c.Deploy.Network)
	}
	// observability.loki (RO-05/08 — was a DEAD key before this round)
	if c.Observability.Loki.URL != "http://loki:3100" {
		t.Errorf("observability.loki.url = %q (was a dead key)", c.Observability.Loki.URL)
	}
	if c.Observability.Loki.LevelField != "lvl" {
		t.Errorf("observability.loki.level_field = %q, want lvl", c.Observability.Loki.LevelField)
	}
	if c.Observability.Loki.ErrorMatch != "(?i)error|warn|fatal" {
		t.Errorf("observability.loki.error_match = %q", c.Observability.Loki.ErrorMatch)
	}
	if !c.Observability.Loki.DeriveProjectFromContainer {
		t.Error("observability.loki.derive_project_from_container not parsed")
	}
	// observability.prometheus (RO-07 — was a DEAD key before this round)
	if got := c.Observability.Prometheus.Targets; len(got) != 2 || got[0] != "my-api:9090" {
		t.Errorf("observability.prometheus.targets = %v, want [my-api:9090 my-worker:9090]", got)
	}
}

// Resolver helpers: explicit values win; absent ones fall back to safe defaults so a
// minimal config still works and the bundled obs stack URLs are the default.
func TestResolvers_ExplicitWin(t *testing.T) {
	c := loadYAML(t, argusBlockYAML)
	cases := map[string]struct{ got, want string }{
		"SUTProject":   {c.SUTProject(), "my-sut"},
		"SUTNetwork":   {c.SUTNetwork(), "my-sut_default"},
		"LokiURL":      {c.LokiURL(), "http://loki:3100"},
		"LevelField":   {c.LevelField(), "lvl"},
		"ErrorMatch":   {c.ErrorMatch(), "(?i)error|warn|fatal"},
		"ProjectLabel": {c.ProjectLabel(), "my-service"}, // falls back to project.name when project_label unset
	}
	for name, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s() = %q, want %q", name, tc.got, tc.want)
		}
	}
	if !c.DeriveProjectFromContainer() {
		t.Error("DeriveProjectFromContainer() = false, want true")
	}
	if got := c.PromTargets(); len(got) != 2 {
		t.Errorf("PromTargets() = %v", got)
	}
}

// CHANGE-1: the MCP endpoint/transport/token are declared in targets.mcp; resolvers
// default transport to streamable-http and return "" for an absent block.
func TestMCPResolvers(t *testing.T) {
	const mcpYAML = `
project:
  name: social
targets:
  mcp:
    base_url: http://social-gateway:8080
    transport: http-sse
    auth:
      type: bearer
      bearer_token: smcp_abc123
`
	c := loadYAML(t, mcpYAML)
	if got := c.MCPBaseURL(); got != "http://social-gateway:8080" {
		t.Errorf("MCPBaseURL() = %q", got)
	}
	if got := c.MCPTransport(); got != "http-sse" {
		t.Errorf("MCPTransport() = %q, want http-sse", got)
	}
	if got := c.MCPToken(); got != "smcp_abc123" {
		t.Errorf("MCPToken() = %q", got)
	}
	// absent block → empty base/token, default transport.
	bare := loadYAML(t, "project:\n  name: bare\ntargets:\n  http:\n    base_url: http://x:8080\n")
	if bare.MCPBaseURL() != "" || bare.MCPToken() != "" {
		t.Errorf("absent targets.mcp must yield empty base/token, got %q/%q", bare.MCPBaseURL(), bare.MCPToken())
	}
	if bare.MCPTransport() != "streamable-http" {
		t.Errorf("default MCPTransport() = %q, want streamable-http", bare.MCPTransport())
	}
	// declared with no transport → default.
	noTr := loadYAML(t, "project:\n  name: e\ntargets:\n  mcp:\n    base_url: http://memstore-gateway:8090\n")
	if noTr.MCPTransport() != "streamable-http" {
		t.Errorf("declared-but-no-transport MCPTransport() = %q, want streamable-http", noTr.MCPTransport())
	}
}

// CHANGE-1: an mcp/chain scenario's wiring is targets.mcp, not targets.http. Validate
// must (a) flag a missing targets.mcp.base_url for an mcp scenario, and (b) NOT demand
// targets.http from a pure-MCP SUT.
func TestValidate_MCPScenarioRequiresTargetsMCP(t *testing.T) {
	scnDir := t.TempDir()
	mcpMD := "# Scenario: mcp\n\n## Metadata\n- **ID**: MCP-200\n- **Layer**: HTTP Ingestion\n- **Tags**: mcp, critical\n\n## TRIGGER\nPOST `${MCP_URL}`\n\n```json\n{\"transport\":\"streamable-http\",\"tool\":\"t\",\"args\":{}}\n```\n\n## EXPECT\n- result.isError == false\n"
	if err := os.WriteFile(filepath.Join(scnDir, "MCP-200.md"), []byte(mcpMD), 0o600); err != nil {
		t.Fatal(err)
	}
	// (a) no targets.mcp + no targets.http → exactly one error, about targets.mcp (NOT http).
	noMCP := loadYAML(t, "project:\n  name: social\nscenarios:\n  timeout_default: 20s\n")
	errs, count, err := noMCP.Validate(scnDir)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if count != 1 {
		t.Fatalf("scenarios_found = %d, want 1", count)
	}
	if len(errs) != 1 || !contains(errs[0].Message, "targets.mcp.base_url") {
		t.Fatalf("want 1 targets.mcp error, got %+v", errs)
	}
	for _, e := range errs {
		if contains(e.Message, "targets.http") {
			t.Errorf("an mcp scenario must NOT be flagged for missing targets.http: %q", e.Message)
		}
	}
	// (b) targets.mcp declared (and NO targets.http) → clean.
	withMCP := loadYAML(t, "project:\n  name: social\ntargets:\n  mcp:\n    base_url: http://social-gateway:8080\nscenarios:\n  timeout_default: 20s\n")
	errs, _, _ = withMCP.Validate(scnDir)
	if len(errs) != 0 {
		t.Fatalf("a pure-MCP config (targets.mcp, no targets.http) must validate clean, got %+v", errs)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// CHANGE-2: the correlation/saga field names are declarable; explicit wins, absent → canonical default.
func TestCorrelationFieldResolvers(t *testing.T) {
	c := loadYAML(t, "project:\n  name: social\nobservability:\n  loki:\n    correlation_field: request_id\n    saga_event_field: kind\n    saga_event_value: audit\n")
	if c.CorrelationField() != "request_id" {
		t.Errorf("CorrelationField() = %q, want request_id", c.CorrelationField())
	}
	if c.SagaEventField() != "kind" || c.SagaEventValue() != "audit" {
		t.Errorf("saga field/value = %q/%q, want kind/audit", c.SagaEventField(), c.SagaEventValue())
	}
	bare := loadYAML(t, "project:\n  name: bare\ntargets:\n  http:\n    base_url: http://x:8080\n")
	if bare.CorrelationField() != "correlation_id" {
		t.Errorf("default CorrelationField() = %q, want correlation_id", bare.CorrelationField())
	}
	if bare.SagaEventField() != "event_type" || bare.SagaEventValue() != "saga" {
		t.Errorf("default saga field/value = %q/%q, want event_type/saga", bare.SagaEventField(), bare.SagaEventValue())
	}
}

// Onboarding policy (warn-only): LogFieldMappingWarnings nudges for each of the four log-field
// translation-table entries the SUT owner did NOT declare explicitly — surfacing reliance on a
// default without failing validation.
func TestLogFieldMappingWarnings(t *testing.T) {
	// none of the four declared -> 4 nudges (one per field name), each saying "declare it explicitly".
	none := loadYAML(t, "project:\n  name: bare\ntargets:\n  http:\n    base_url: http://x:8080\n")
	w := none.LogFieldMappingWarnings()
	if len(w) != 4 {
		t.Fatalf("a config declaring none of the four must nudge 4x, got %d: %v", len(w), w)
	}
	joined := strings.Join(w, " | ")
	for _, key := range []string{"correlation_field", "level_field", "saga_event_field", "saga_event_value"} {
		if !strings.Contains(joined, key) {
			t.Errorf("missing a nudge for %q: %v", key, w)
		}
	}
	if !strings.Contains(joined, "declare it explicitly") {
		t.Errorf("nudge wording must say 'declare it explicitly': %v", w)
	}
	// all four declared -> zero nudges.
	all := loadYAML(t, "project:\n  name: social\nobservability:\n  loki:\n    correlation_field: request_id\n    level_field: level\n    saga_event_field: event\n    saga_event_value: tool_dispatch\n")
	if got := all.LogFieldMappingWarnings(); len(got) != 0 {
		t.Errorf("all four declared must nudge 0x, got %v", got)
	}
	// partial (only correlation_field) -> 3 nudges, none for correlation_field.
	partial := loadYAML(t, "project:\n  name: p\nobservability:\n  loki:\n    correlation_field: request_id\n")
	pw := partial.LogFieldMappingWarnings()
	if len(pw) != 3 || strings.Contains(strings.Join(pw, " "), "correlation_field") {
		t.Errorf("partial (correlation_field set) must nudge 3x excluding correlation_field, got %v", pw)
	}
}

func TestResolvers_Defaults(t *testing.T) {
	// A minimal config with NO argus block: helpers must not panic and must
	// return safe defaults (bundled Loki, the SUT-agnostic error|warn matcher, level).
	c := loadYAML(t, "project:\n  name: bare\ntargets:\n  http:\n    base_url: http://x:8080\n")
	if c.LokiURL() != "http://loki:3100" {
		t.Errorf("default LokiURL = %q, want http://loki:3100", c.LokiURL())
	}
	if c.LevelField() != "level" {
		t.Errorf("default LevelField = %q, want level", c.LevelField())
	}
	if c.ErrorMatch() != "(?i)error|warn" {
		t.Errorf("default ErrorMatch = %q, want (?i)error|warn", c.ErrorMatch())
	}
	if c.ProjectLabel() != "bare" {
		t.Errorf("default ProjectLabel = %q, want bare (project.name)", c.ProjectLabel())
	}
	if c.SUTProject() != "" || c.SUTNetwork() != "" {
		t.Errorf("absent deploy block should yield empty SUTProject/SUTNetwork, got %q/%q", c.SUTProject(), c.SUTNetwork())
	}
}

// TestLokiURLConfigured_NoBundledFallback (T3.2): unlike LokiURL, this accessor must return ""
// when the operator declared nothing — --obs=adopt needs to tell "declared" apart from "silently
// defaulted" so it can refuse rather than pointing the executor at a Loki adopt never deploys.
func TestLokiURLConfigured_NoBundledFallback(t *testing.T) {
	bare := loadYAML(t, "project:\n  name: bare\n")
	if got := bare.LokiURLConfigured(); got != "" {
		t.Errorf("LokiURLConfigured() on a config with no observability.loki.url = %q, want \"\" (no bundled-default fallback)", got)
	}
	if got := bare.LokiURL(); got != "http://loki:3100" {
		t.Errorf("LokiURL() must keep its bundled-default fallback unchanged, got %q", got)
	}
	declared := loadYAML(t, "project:\n  name: p\nobservability:\n  loki:\n    url: http://operator-loki.example:3100\n")
	if got := declared.LokiURLConfigured(); got != "http://operator-loki.example:3100" {
		t.Errorf("LokiURLConfigured() = %q, want the declared url", got)
	}
	if got := declared.LokiURL(); got != "http://operator-loki.example:3100" {
		t.Errorf("LokiURL() must also honor a declared url, got %q", got)
	}
}

// TestPushgatewayURL (T3.2): OPTIONAL, following the LokiURL accessor pattern but with NO
// bundled-default fallback at all — bundled mode never reads this, and in adopt mode "" is the
// correct "no pushgateway" signal, not a missing value to paper over.
func TestPushgatewayURL(t *testing.T) {
	bare := loadYAML(t, "project:\n  name: bare\n")
	if got := bare.PushgatewayURL(); got != "" {
		t.Errorf("PushgatewayURL() with none declared = %q, want \"\"", got)
	}
	declared := loadYAML(t, "project:\n  name: p\nobservability:\n  pushgateway:\n    url: http://operator-pushgateway.example:9091\n")
	if got := declared.PushgatewayURL(); got != "http://operator-pushgateway.example:9091" {
		t.Errorf("PushgatewayURL() = %q, want the declared url", got)
	}
}

// UC163 bus guardrail: a shared/production-looking broker URL warns; a per-instance one does not.
// Plus the UC030 layer→target accessor.
func TestBusGuardrailAndTargetForLayer(t *testing.T) {
	for _, c := range []struct {
		u    string
		want bool
	}{
		{"amqp://rabbitmq:5672", false},
		{"amqp://product-service:5672", false}, // "product" must NOT match "prod"
		{"amqp://rabbitmq.prod.internal:5672", true},
		{"amqp://mq.shared.corp:5672", true},
		{"https://rabbit.production.example/api", true},
		{"", false},
	} {
		if got := looksSharedOrProd(c.u); got != c.want {
			t.Errorf("looksSharedOrProd(%q) = %v, want %v", c.u, got, c.want)
		}
	}
	if tgt, ok := TargetForLayer("Message Flow"); !ok || tgt != "message_broker" {
		t.Errorf("TargetForLayer(Message Flow) = %q,%v; want message_broker,true", tgt, ok)
	}
	if tgt, ok := TargetForLayer("HTTP Ingestion"); !ok || tgt != "http" {
		t.Errorf("TargetForLayer(HTTP Ingestion) = %q,%v; want http,true", tgt, ok)
	}
	if _, ok := TargetForLayer("Error Path"); ok {
		t.Error("Error Path maps to no concrete target — want ok=false")
	}
}

// GAP-3 (2026-07-22): the MCP client timeout was HARDCODED at 30s with no per-SUT knob
// (internal/mcp/client.go). Social's PASSING calls measured 16.1s and 19.1s, so it runs near the
// ceiling and a slow-but-healthy SUT is unavoidably flaky through no fault of its own — four of
// its eight failures were transport timeouts at exactly 30.00s. The timeout must be declarable.
func TestMCPTimeout_DeclarableWithSafeDefault(t *testing.T) {
	var c Config
	if got := c.MCPTimeout(); got != 30*time.Second {
		t.Errorf("an undeclared timeout must default to 30s, got %v", got)
	}
	c.Targets.MCP = &MCPTarget{TimeoutSeconds: 90}
	if got := c.MCPTimeout(); got != 90*time.Second {
		t.Errorf("declared timeout_seconds:90 → %v", got)
	}
	c.Targets.MCP.TimeoutSeconds = -5 // nonsense must not disable the timeout entirely
	if got := c.MCPTimeout(); got != 30*time.Second {
		t.Errorf("a non-positive timeout must fall back to the default, got %v", got)
	}
}

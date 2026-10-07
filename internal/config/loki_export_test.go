package config

import (
	"os"
	"strings"
	"testing"
)

// T3.1 (E3 export hosted-Loki target): observability.loki.push_url + observability.loki.credential
// let a SUT declare a HOSTED Loki (e.g. Grafana Cloud) as the --obs=export target. url stays the
// READ/query side (already used by adopt, LokiURLConfigured); push_url is the NEW write side, and
// credential is a ${VAR}-only Basic-Auth pair, same shape/rule as observability.betterstack.credential.

const lokiExportBase = "project:\n  name: t\ntargets:\n  http:\n    base_url: http://sut:8080\n"

func lokiExportBlock() string {
	return `observability:
  loki:
    url: https://logs-prod.example.grafana.net
    push_url: https://logs-prod.example.grafana.net/loki/api/v1/push
    credential: ${LOKI_EXPORT_CREDENTIAL}
`
}

// push_url / credential parse and resolve.
func TestLokiExport_PushURLAndCredential_Parse(t *testing.T) {
	t.Setenv("LOKI_EXPORT_CREDENTIAL", "hosted-user:hosted-pass")
	c, err := loadCfgText(t, lokiExportBase+lokiExportBlock())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.LokiURLConfigured(); got != "https://logs-prod.example.grafana.net" {
		t.Errorf("LokiURLConfigured() = %q", got)
	}
	if got := c.LokiPushURLConfigured(); got != "https://logs-prod.example.grafana.net/loki/api/v1/push" {
		t.Errorf("LokiPushURLConfigured() = %q", got)
	}
	if got := c.LokiCredential(); got != "hosted-user:hosted-pass" {
		t.Errorf("LokiCredential() must resolve the ${VAR}, got %q", got)
	}
}

// A config declaring neither push_url nor credential must load unchanged (every pre-existing
// argus-config, and bundled/adopt configs) — both new accessors report "".
func TestLokiExport_Undeclared_EmptyAccessors(t *testing.T) {
	c, err := loadCfgText(t, lokiExportBase+"observability:\n  loki:\n    url: http://loki:3100\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.LokiPushURLConfigured(); got != "" {
		t.Errorf("LokiPushURLConfigured() must be empty when undeclared, got %q", got)
	}
	if got := c.LokiCredential(); got != "" {
		t.Errorf("LokiCredential() must be empty when undeclared, got %q", got)
	}
}

// The credential MUST be a ${VAR} reference — never a literal secret committed to the file. Same
// rule, same wording style as observability.betterstack.credential's own refusal.
func TestLokiExport_LiteralCredential_Refused(t *testing.T) {
	body := lokiExportBase + `observability:
  loki:
    url: https://logs-prod.example.grafana.net
    push_url: https://logs-prod.example.grafana.net/loki/api/v1/push
    credential: sk-super-secret-literal-value
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("a literal credential must be refused")
	}
	if !strings.Contains(err.Error(), "observability.loki.credential") {
		t.Errorf("refusal must name observability.loki.credential, got: %v", err)
	}
	if strings.Contains(err.Error(), "sk-super-secret-literal-value") {
		t.Errorf("refusal must NEVER echo the literal secret value back, got: %v", err)
	}
}

// A referenced-but-unset ${VAR} fails loud — D1 parity with every other credential field,
// including observability.betterstack.credential.
func TestLokiExport_UnsetEnvVar_FailsLoud(t *testing.T) {
	os.Unsetenv("LOKI_EXPORT_CREDENTIAL_MISSING")
	body := lokiExportBase + `observability:
  loki:
    url: https://logs-prod.example.grafana.net
    push_url: https://logs-prod.example.grafana.net/loki/api/v1/push
    credential: ${LOKI_EXPORT_CREDENTIAL_MISSING}
`
	_, err := loadCfgText(t, body)
	if err == nil {
		t.Fatal("an unresolved ${VAR} credential must fail loud")
	}
	if !strings.Contains(err.Error(), "LOKI_EXPORT_CREDENTIAL_MISSING") {
		t.Errorf("refusal must name the missing var, got: %v", err)
	}
}

// EnvRefs (the unresolved-parse report secrets-scan/onboard.sh's preflight reads) must include
// observability.loki.credential, exactly as it already includes observability.betterstack.credential —
// this is what makes onboard.sh's early secrets preflight refuse a missing LOKI credential BEFORE
// any cluster mutation, with no new special-case code (see cmd/argus secrets-scan, onboard.sh step 2b).
func TestLokiExport_EnvRefs_IncludesLokiCredential(t *testing.T) {
	p := writeCfg(t, lokiExportBase+`observability:
  loki:
    url: https://logs-prod.example.grafana.net
    push_url: https://logs-prod.example.grafana.net/loki/api/v1/push
    credential: ${LOKI_EXPORT_CREDENTIAL}
`)
	c, err := ParseUnresolved(p)
	if err != nil {
		t.Fatalf("ParseUnresolved: %v", err)
	}
	refs := c.EnvRefs()
	found := false
	for _, r := range refs {
		if r.Name == "LOKI_EXPORT_CREDENTIAL" && r.Field == "observability.loki.credential" {
			found = true
		}
	}
	if !found {
		t.Errorf("EnvRefs() must report LOKI_EXPORT_CREDENTIAL (field observability.loki.credential), got: %+v", refs)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg writes body to a temp argus-config.yaml and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// allFieldsYAML references ${VAR} in every one of the 9 enumerated fields (D2 full parity;
// http.base_url + message_broker.management_url added by the secrets-preflight gate).
const allFieldsYAML = `project:
  name: t
targets:
  http:
    base_url: ${HTTP_BASE}
  auth:
    type: bearer
    bearer_token: ${HTTP_TOK}
  mcp:
    base_url: ${MCP_BASE}
    transport: streamable-http
    auth:
      type: bearer
      bearer_token: ${MCP_TOK}
  database:
    jdbc_url: ${DB_URL}
    username: ${DB_USER}
    password: ${DB_PASS}
  message_broker:
    url: ${BROKER_URL}
    management_url: ${MGMT_URL}
`

// D2: every enumerated field resolves from the process environment at load.
func TestExpandEnv_ResolvesAllEnumeratedFields(t *testing.T) {
	t.Setenv("HTTP_BASE", "http://my-api:8080")
	t.Setenv("HTTP_TOK", "http-secret")
	t.Setenv("MCP_BASE", "http://memstore:8090/mcp")
	t.Setenv("MCP_TOK", "mcp-secret")
	t.Setenv("DB_URL", "jdbc:postgresql://db:5432/x")
	t.Setenv("DB_USER", "sut_ro")
	t.Setenv("DB_PASS", "db-secret")
	t.Setenv("BROKER_URL", "amqp://u:p@rabbit:5672/")
	t.Setenv("MGMT_URL", "http://mu:mp@rabbit:15672")

	c, err := Load(writeCfg(t, allFieldsYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.Targets.HTTP.BaseURL; got != "http://my-api:8080" {
		t.Errorf("http.base_url = %q, want the env value", got)
	}
	if got := c.Targets.MessageBroker.ManagementURL; got != "http://mu:mp@rabbit:15672" {
		t.Errorf("broker.management_url = %q, want the env value", got)
	}
	if got := c.Targets.Auth.BearerToken; got != "http-secret" {
		t.Errorf("http auth.bearer_token = %q, want http-secret", got)
	}
	if got := c.Targets.MCP.BaseURL; got != "http://memstore:8090/mcp" {
		t.Errorf("mcp.base_url = %q, want the env value", got)
	}
	if got := c.Targets.MCP.Auth.BearerToken; got != "mcp-secret" {
		t.Errorf("mcp auth.bearer_token = %q, want mcp-secret", got)
	}
	if got := c.Targets.Database.JDBCURL; got != "jdbc:postgresql://db:5432/x" {
		t.Errorf("db.jdbc_url = %q", got)
	}
	if got := c.Targets.Database.Username; got != "sut_ro" {
		t.Errorf("db.username = %q", got)
	}
	if got := c.Targets.Database.Password; got != "db-secret" {
		t.Errorf("db.password = %q", got)
	}
	if got := c.Targets.MessageBroker.URL; got != "amqp://u:p@rabbit:5672/" {
		t.Errorf("broker.url = %q", got)
	}
}

// D1: a referenced-but-unset ${VAR} is a HARD error at Load — never a silent empty credential.
func TestExpandEnv_UnsetIsHardFail(t *testing.T) {
	os.Unsetenv("SECRETS_VAR_MISSING_ONE")
	body := `project:
  name: t
targets:
  mcp:
    base_url: http://localhost:8090/mcp
    auth:
      type: bearer
      bearer_token: ${SECRETS_VAR_MISSING_ONE}
`
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatal("Load must FAIL when a referenced ${VAR} is unset (D1)")
	}
	if !strings.Contains(err.Error(), "SECRETS_VAR_MISSING_ONE") {
		t.Errorf("error must name the missing var; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "bearer_token") {
		t.Errorf("error should point at the field; got %q", err.Error())
	}
}

// D1: an empty (set-but-blank) env var counts as unset — still a hard fail.
func TestExpandEnv_EmptyEnvIsHardFail(t *testing.T) {
	t.Setenv("SECRETS_VAR_EMPTY", "")
	body := `project:
  name: t
targets:
  auth:
    type: bearer
    bearer_token: ${SECRETS_VAR_EMPTY}
`
	if _, err := Load(writeCfg(t, body)); err == nil {
		t.Fatal("Load must FAIL when a referenced ${VAR} is set-but-empty (D1)")
	}
}

// D1: multiple missing vars are ALL listed in one error.
func TestExpandEnv_MultipleMissingAllListed(t *testing.T) {
	os.Unsetenv("SV_MISS_A")
	os.Unsetenv("SV_MISS_B")
	body := `project:
  name: t
targets:
  auth:
    bearer_token: ${SV_MISS_A}
  database:
    jdbc_url: jdbc:postgresql://db/x
    username: u
    password: ${SV_MISS_B}
`
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatal("Load must FAIL")
	}
	if !strings.Contains(err.Error(), "SV_MISS_A") || !strings.Contains(err.Error(), "SV_MISS_B") {
		t.Errorf("error must list BOTH missing vars; got %q", err.Error())
	}
}

// The error must NEVER echo a resolved secret value (secrets discipline).
func TestExpandEnv_ErrorNeverLeaksValue(t *testing.T) {
	os.Unsetenv("SV_MISS_C")
	t.Setenv("SV_PRESENT", "super-secret-value")
	body := `project:
  name: t
targets:
  auth:
    bearer_token: ${SV_MISS_C}
  mcp:
    base_url: http://x/mcp
    auth:
      bearer_token: ${SV_PRESENT}
`
	_, err := Load(writeCfg(t, body))
	if err == nil {
		t.Fatal("Load must FAIL on the missing var")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Errorf("error leaked a resolved secret value: %q", err.Error())
	}
}

// ${cid} / ${correlation_id} are runtime placeholders (resolveVars), left literal by load-time
// expansion — and must NOT be reported as missing env vars.
func TestExpandEnv_RuntimeTokensLeftLiteral(t *testing.T) {
	body := `project:
  name: t
targets:
  auth:
    bearer_token: ${cid}
  database:
    jdbc_url: jdbc:postgresql://db/x
    username: u
    password: ${correlation_id}
`
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("runtime placeholders must not hard-fail Load: %v", err)
	}
	if c.Targets.Auth.BearerToken != "${cid}" {
		t.Errorf("${cid} should be left literal, got %q", c.Targets.Auth.BearerToken)
	}
	if c.Targets.Database.Password != "${correlation_id}" {
		t.Errorf("${correlation_id} should be left literal, got %q", c.Targets.Database.Password)
	}
}

// A bare $ (e.g. in a password) is NOT a placeholder — braces are required — so it is preserved.
func TestExpandEnv_BareDollarPreserved(t *testing.T) {
	body := `project:
  name: t
targets:
  database:
    jdbc_url: jdbc:postgresql://db/x
    username: u
    password: pa$$w0rd$
`
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Targets.Database.Password != "pa$$w0rd$" {
		t.Errorf("bare $ mangled: got %q, want pa$$w0rd$", c.Targets.Database.Password)
	}
}

// Regression: a fully-literal config (no ${}) still loads unchanged.
func TestExpandEnv_LiteralConfigUnchanged(t *testing.T) {
	body := `project:
  name: t
targets:
  http:
    base_url: http://localhost:8080
  auth:
    type: bearer
    bearer_token: tok-default
`
	c, err := Load(writeCfg(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Targets.Auth.BearerToken != "tok-default" {
		t.Errorf("literal token changed: %q", c.Targets.Auth.BearerToken)
	}
}

package config

import (
	"os"
	"testing"
)

// SECRETS-PREFLIGHT: EnvRefs reports every ${VAR} the config references in the D2-parity
// fields — WITHOUT resolving them. This is the COLLECT-stage source of truth the onboarder
// uses to know which names to look up in the product folder's .env before anything runs.

// ParseUnresolved must load a config whose ${VAR}s are all unset — its whole job is to run
// BEFORE values exist (Load would D1-hard-fail here).
func TestParseUnresolved_NoHardFailOnUnsetVars(t *testing.T) {
	os.Unsetenv("ER_UNSET_TOK")
	body := `project:
  name: t
targets:
  mcp:
    base_url: http://sut:8080/mcp
    auth:
      type: bearer
      bearer_token: ${ER_UNSET_TOK}
`
	c, err := ParseUnresolved(writeCfg(t, body))
	if err != nil {
		t.Fatalf("ParseUnresolved must not D1-fail on unset vars: %v", err)
	}
	if c.Targets.MCP.Auth.BearerToken != "${ER_UNSET_TOK}" {
		t.Errorf("placeholder must be left literal, got %q", c.Targets.MCP.Auth.BearerToken)
	}
}

// All 7 enumerated fields are scanned; names + fields are reported in field order.
func TestEnvRefs_AllEnumeratedFields(t *testing.T) {
	c, err := ParseUnresolved(writeCfg(t, allFieldsYAML))
	if err != nil {
		t.Fatalf("ParseUnresolved: %v", err)
	}
	refs := c.EnvRefs()
	want := []EnvRef{
		{Name: "HTTP_BASE", Field: "targets.http.base_url"},
		{Name: "HTTP_TOK", Field: "targets.auth.bearer_token"},
		{Name: "MCP_BASE", Field: "targets.mcp.base_url"},
		{Name: "MCP_TOK", Field: "targets.mcp.auth.bearer_token"},
		{Name: "DB_USER", Field: "targets.database.username"},
		{Name: "DB_PASS", Field: "targets.database.password"},
		{Name: "DB_URL", Field: "targets.database.jdbc_url"},
		{Name: "BROKER_URL", Field: "targets.message_broker.url"},
		{Name: "MGMT_URL", Field: "targets.message_broker.management_url"},
	}
	if len(refs) != len(want) {
		t.Fatalf("got %d refs, want %d: %+v", len(refs), len(want), refs)
	}
	for i, w := range want {
		if refs[i] != w {
			t.Errorf("refs[%d] = %+v, want %+v", i, refs[i], w)
		}
	}
}

// ${cid}/${correlation_id} are runtime placeholders — never reported as secret references.
func TestEnvRefs_RuntimeTokensExcluded(t *testing.T) {
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
	c, err := ParseUnresolved(writeCfg(t, body))
	if err != nil {
		t.Fatalf("ParseUnresolved: %v", err)
	}
	if refs := c.EnvRefs(); len(refs) != 0 {
		t.Errorf("runtime tokens must be excluded, got %+v", refs)
	}
}

// The same name in two fields is reported once per field (the preflight dedupes names itself;
// the per-field detail powers the guided error message).
func TestEnvRefs_SameNameMultipleFields(t *testing.T) {
	body := `project:
  name: t
targets:
  auth:
    bearer_token: ${SHARED_TOK}
  mcp:
    base_url: http://x/mcp
    auth:
      bearer_token: ${SHARED_TOK}
`
	c, err := ParseUnresolved(writeCfg(t, body))
	if err != nil {
		t.Fatalf("ParseUnresolved: %v", err)
	}
	refs := c.EnvRefs()
	if len(refs) != 2 || refs[0].Name != "SHARED_TOK" || refs[1].Name != "SHARED_TOK" {
		t.Errorf("want the shared name reported per field, got %+v", refs)
	}
	if refs[0].Field == refs[1].Field {
		t.Errorf("fields must differ, got %+v", refs)
	}
}

// A ${VAR}-free config reports zero references (Path A stays silent/unattended).
func TestEnvRefs_LiteralConfigEmpty(t *testing.T) {
	body := `project:
  name: t
targets:
  http:
    base_url: http://localhost:8080
  auth:
    type: bearer
    bearer_token: tok-default
`
	c, err := ParseUnresolved(writeCfg(t, body))
	if err != nil {
		t.Fatalf("ParseUnresolved: %v", err)
	}
	if refs := c.EnvRefs(); len(refs) != 0 {
		t.Errorf("literal config must report no refs, got %+v", refs)
	}
}

// Load's behavior is unchanged by the refactor: same resolution, same D1 hard-fail.
// (Guarded by the existing expandenv_test.go suite.)

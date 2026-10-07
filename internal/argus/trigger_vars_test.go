package argus

import (
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// SY-8 (Hub under test, 2026-09-14): a scenario that must run as a DIFFERENT identity than
// the config default names its token as `Authorization: Bearer ${SOME_TOKEN}` — the same ${VAR}
// form every other author header (headers.go) and every mcp/ui/chain payload already resolves.
// T3.d: an unresolved ${…} must never reach the wire. The Authorization override and the http
// trigger payload were the two remaining DeriveProps paths that handed JMeter the literal
// placeholder, so a two-identity SUT (operator PAT vs builder PAT) could not be expressed at all.
func TestDeriveProps_AuthorizationOverrideResolvesVars(t *testing.T) {
	t.Setenv("ARGUS_TEST_OPERATOR_TOKEN", "syn_operator")
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	c.Targets.Auth = &config.AuthTarget{BearerToken: "tok-default"}
	s := &scenario.Scenario{ID: "ARG-SYN-005", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${HUB_URL}/agent/mcp",
			Headers: map[string]string{"Authorization": "Bearer ${ARGUS_TEST_OPERATOR_TOKEN}"}},
		Expect: []string{"status=200"}}
	if got := mustProps(t, c, s, "tr-1")["auth.header"]; got != "Bearer syn_operator" {
		t.Errorf("Authorization override must resolve ${VAR}, got auth.header = %q", got)
	}
	// An unset variable stays literal (UC-82) so preflight can name it — never silently emptied,
	// and never the config default (that would be the false auth bypass 4.8 closed).
	s.Trigger.Headers["Authorization"] = "Bearer ${ARGUS_TEST_UNSET_TOKEN}"
	if got := mustProps(t, c, s, "tr-2")["auth.header"]; got != "Bearer ${ARGUS_TEST_UNSET_TOKEN}" {
		t.Errorf("unset ${VAR} must stay literal, got auth.header = %q", got)
	}
	// Present-but-empty still means "send no token" (4.8).
	s.Trigger.Headers["Authorization"] = ""
	if got, ok := mustProps(t, c, s, "tr-3")["auth.header"]; !ok || got != "" {
		t.Errorf("empty Authorization must still send no token, got ok=%v %q", ok, got)
	}
}

// The http trigger payload resolves ${VAR} from the environment and ${cid} to the run's
// correlation id, exactly as an mcp payload does (parseMCPSpec). A provisioning call whose body
// names the environment (env_name, customer_slug, builder_user_id) is otherwise unwritable
// without a literal per-environment value inside a shared scenario file.
func TestDeriveProps_TriggerPayloadResolvesVarsAndCid(t *testing.T) {
	t.Setenv("ARGUS_TEST_ENV_NAME", "argus-dev")
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	s := &scenario.Scenario{ID: "ARG-SYN-001", Layers: []string{"HTTP Ingestion"},
		Trigger: scenario.Trigger{Method: "POST", URL: "${HUB_URL}/api/provision/argus",
			Payload: `{"env_name":"${ARGUS_TEST_ENV_NAME}","request_id":"${cid}"}`},
		Expect: []string{"status=200"}}
	got := mustProps(t, c, s, "tr-run-ARG-SYN-001-0badf00d")["trigger.payload"]
	want := `{"env_name":"argus-dev","request_id":"tr-run-ARG-SYN-001-0badf00d"}`
	if got != want {
		t.Errorf("trigger.payload must carry the resolved body, got %q", got)
	}
	// A payload with no placeholder is byte-identical to today.
	s.Trigger.Payload = `{"items":[{"sku":"A","qty":1}]}`
	if got := mustProps(t, c, s, "tr-2")["trigger.payload"]; got != s.Trigger.Payload {
		t.Errorf("placeholder-free payload must pass through unchanged, got %q", got)
	}
}

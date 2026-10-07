package toolcore

import (
	"os"
	"path/filepath"
	"testing"
)

// T3.1 (E3 export hosted-Loki target): lokiFor must thread the RESOLVED
// observability.loki.credential into obsquery.Loki.Credential — the wiring that makes the
// executor's own get_sagas/tail_logs able to authenticate against a hosted Loki, same pattern as
// TestBackendFor_SelectsBetterStackWhenConfigured threads BetterStack's credential.

func lokiCredEnv(t *testing.T, lokiBlock string) Env {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "argus-config.yaml")
	body := "project:\n  name: t\ntargets:\n  http:\n    base_url: http://sut:8080\n" + lokiBlock
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Env{ConfigPath: p, ScenariosDir: dir, Loki: "http://loki:3100"}
}

func TestLokiFor_ThreadsResolvedCredential(t *testing.T) {
	t.Setenv("LOKI_EXPORT_CREDENTIAL", "hosted-user:hosted-pass")
	e := lokiCredEnv(t, `observability:
  loki:
    url: https://logs.example.grafana.net
    push_url: https://logs.example.grafana.net/loki/api/v1/push
    credential: ${LOKI_EXPORT_CREDENTIAL}
`)
	e.Loki = "https://logs.example.grafana.net" // export/adopt: --loki IS the configured url
	l := lokiFor(e)
	if l.Credential != "hosted-user:hosted-pass" {
		t.Errorf("lokiFor must thread the RESOLVED observability.loki.credential, got %q", l.Credential)
	}
}

// the hosted credential belongs to the hosted Loki. A bundled or shared executor that
// loads a config declaring one must NOT send it to the Loki its --loki names — that hands the hosted
// password to a different server.
func TestLokiFor_NoCredentialForAnotherLoki(t *testing.T) {
	t.Setenv("LOKI_EXPORT_CREDENTIAL", "hosted-user:hosted-pass")
	e := lokiCredEnv(t, `observability:
  loki:
    url: https://logs.example.grafana.net
    push_url: https://logs.example.grafana.net/loki/api/v1/push
    credential: ${LOKI_EXPORT_CREDENTIAL}
`) // e.Loki = http://loki:3100, the bundled default
	if got := lokiFor(e).Credential; got != "" {
		t.Errorf("the hosted credential must not be sent to a bundled Loki, got a non-empty Credential")
	}
}

// A config with no observability.loki.credential (bundled/adopt, every pre-T3.1 config) must
// leave obsquery.Loki.Credential empty — the "no Authorization header at all" case.
func TestLokiFor_NoCredentialDeclared_EmptyCredential(t *testing.T) {
	e := lokiCredEnv(t, "observability:\n  loki:\n    url: http://loki:3100\n")
	l := lokiFor(e)
	if l.Credential != "" {
		t.Errorf("lokiFor must leave Credential empty when observability.loki.credential is undeclared, got %q", l.Credential)
	}
}

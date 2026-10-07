package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T3.3 (E3 shared ingest): the CLI side — the --obs-shared-url and --loki-tenant flags, the
// ARGUS_LOKI_TENANT env, the empty-tenant refusal at start, render-k8s --obs shared end to end,
// and the render-obs-shared subcommand that renders the environment's ONE Loki.

func TestSharedTenantGuard(t *testing.T) {
	if err := sharedTenantGuard("shared", ""); err == nil || !strings.Contains(err.Error(), "unscoped") {
		t.Errorf("--obs shared with no tenant must be refused as unscoped, got %v", err)
	}
	if err := sharedTenantGuard("shared", "inst-a"); err != nil {
		t.Errorf("--obs shared with a tenant must pass, got %v", err)
	}
	for _, mode := range []string{"", "bundled", "adopt", "export"} {
		if err := sharedTenantGuard(mode, ""); err != nil {
			t.Errorf("--obs %q with no tenant is today's behaviour and must pass, got %v", mode, err)
		}
	}
}

// Every runner.ExecConfig literal must carry the tenant, because each one is a real path to Loki: the
// direct run (cmdRunDirect) and the federated one (cmdServeMode), which is how a k8s executor runs
// what the control plane assigns it. A literal without it would be refused by the shared Loki on every
// query and push, and the run would report "no saga" rather than an error. A behavioural test would
// need a whole serve process, so this reads the source, in the style of
// TestServeMode_ExecConfigUsesTheResolvedGrafanaNotTheRawFlag. Found by mutation, not by reading.
func TestEveryExecConfigCarriesTheLokiTenant(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	rest, n := string(src), 0
	for {
		k := strings.Index(rest, "runner.ExecConfig{")
		if k < 0 {
			break
		}
		rest = rest[k+len("runner.ExecConfig{"):]
		n++
		end := strings.Index(rest, "}")
		if end < 0 {
			t.Fatal("unterminated runner.ExecConfig literal")
		}
		if !strings.Contains(rest[:end], "LokiTenant: cf.lokiTenant") {
			t.Errorf("runner.ExecConfig #%d does not set LokiTenant: cf.lokiTenant. Under --obs shared its runs "+
				"would read and write the shared Loki unscoped, and be refused.\n  literal: %s", n, strings.TrimSpace(rest[:end]))
		}
	}
	if n < 2 {
		t.Fatalf("found %d runner.ExecConfig literals, want at least the direct and the federated one — this test has lost its subject", n)
	}
}

// A serve with --obs shared and no tenant must refuse BEFORE it serves anything.
func TestDispatch_SharedWithoutTenantRefusedAtStart(t *testing.T) {
	t.Setenv("ARGUS_LOKI_TENANT", "")
	rc := exitOK
	out := captureStdout(t, func() {
		rc = dispatch([]string{"logs", "--obs", "shared", "--correlation-id", "tr-1", "--config", "/nonexistent.yaml"})
	})
	if rc == exitOK || !strings.Contains(out, "unscoped") {
		t.Fatalf("--obs shared with no tenant must refuse at start (rc=%d): %s", rc, out)
	}
}

func TestLokiTenantFlag_DefaultsFromEnvAndReachesEnv(t *testing.T) {
	t.Setenv("ARGUS_LOKI_TENANT", "from-env")
	cf := &commonFlags{}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	cf.bind(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if got := env(cf).LokiTenant; got != "from-env" {
		t.Errorf("ARGUS_LOKI_TENANT must reach toolcore.Env.LokiTenant, got %q", got)
	}
	if err := fs.Parse([]string{"--loki-tenant", "from-flag"}); err != nil {
		t.Fatal(err)
	}
	if got := env(cf).LokiTenant; got != "from-flag" {
		t.Errorf("--loki-tenant must win over the env, got %q", got)
	}
}

func TestRenderK8s_ObsShared_EndToEnd(t *testing.T) {
	rc, got, out := runRenderK8sWithConfig(t, noObsConfig, "--obs", "shared")
	if rc != exitOK {
		t.Fatalf("--obs shared: exit %d, want exitOK: %+v", rc, got)
	}
	if got.ObsMode != "shared" {
		t.Errorf("obs_mode = %q, want shared", got.ObsMode)
	}
	obs, _ := os.ReadFile(filepath.Join(out, "obs.yaml"))
	if !strings.Contains(string(obs), "tenant_id: obs-modes-test\n") {
		t.Errorf("obs.yaml must carry promtail tenant_id: obs-modes-test")
	}
	if !strings.Contains(string(obs), "- url: http://loki.argus-obs.svc.cluster.local:3100/loki/api/v1/push\n") {
		t.Errorf("obs.yaml must push to the default shared URL")
	}
	exec, _ := os.ReadFile(filepath.Join(out, "executor.yaml"))
	if !strings.Contains(string(exec), "- --loki-tenant\n            - \"obs-modes-test\"\n") {
		t.Errorf("executor.yaml must pass --loki-tenant obs-modes-test")
	}
}

func TestRenderK8s_ObsShared_URLFlagHonoured(t *testing.T) {
	rc, got, out := runRenderK8sWithConfig(t, noObsConfig, "--obs", "shared", "--obs-shared-url", "http://shared-loki.example:3100")
	if rc != exitOK {
		t.Fatalf("exit %d: %+v", rc, got)
	}
	obs, _ := os.ReadFile(filepath.Join(out, "obs.yaml"))
	if !strings.Contains(string(obs), "- url: http://shared-loki.example:3100/loki/api/v1/push\n") {
		t.Errorf("--obs-shared-url must be the promtail push base")
	}
}

func TestRenderObsShared_WritesOneLoki(t *testing.T) {
	out := t.TempDir()
	rc := exitOK
	stdout := captureStdout(t, func() {
		rc = dispatch([]string{"render-obs-shared", "--tier", "k3d", "--out", out})
	})
	if rc != exitOK {
		t.Fatalf("render-obs-shared: exit %d: %s", rc, stdout)
	}
	var got struct {
		Rendered  bool   `json:"rendered"`
		Namespace string `json:"namespace"`
		Path      string `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if got.Namespace != "argus-obs" || got.Path != filepath.Join(out, "obs-shared.yaml") {
		t.Errorf("unexpected output %+v", got)
	}
	b, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "auth_enabled: true") {
		t.Error("obs-shared.yaml must carry the auth_enabled Loki")
	}
}

func TestRenderObsShared_ComposeRefused(t *testing.T) {
	rc := exitOK
	stdout := captureStdout(t, func() {
		rc = dispatch([]string{"render-obs-shared", "--tier", "compose", "--out", t.TempDir()})
	})
	if rc == exitOK || !strings.Contains(stdout, "--obs=shared") || !strings.Contains(stdout, "compose") {
		t.Fatalf("render-obs-shared --tier compose must refuse naming mode and tier (rc=%d): %s", rc, stdout)
	}
}

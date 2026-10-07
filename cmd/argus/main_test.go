package main

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FINDING-6 (2026-06-19): `run` must expose --tag/--layer. The runner-core already
// filters by layer/tag (collectScenarios/RunAll); the CLI never registered the flags,
// so bind() must add both and they must land on commonFlags for cmdRun to pass through.
func TestCommonFlags_BindsLayerTag(t *testing.T) {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cf.bind(fs)
	if err := fs.Parse([]string{"--layer", "Rate Limiting", "--tag", "critical"}); err != nil {
		t.Fatalf("parse --layer/--tag: %v", err)
	}
	if cf.layer != "Rate Limiting" {
		t.Errorf("--layer not bound: %q", cf.layer)
	}
	if cf.tag != "critical" {
		t.Errorf("--tag not bound: %q", cf.tag)
	}
}

// ── VR10-R3 (V28-007): `--config` has NO default and every config-reading subcommand refuses without it ──
//
// `argus run` typed inside the executor with the flag forgotten used to fall back to the bundled
// OrderService demo config — a different SUT — print `run_begin: ok`, and fail later in the background.
// The owner's ruling (option A): the demo default is deleted; a subcommand that needs a config and was
// not given one refuses at flag parsing, names the flag, and mints no run id. Option B (keep the demo
// default, announce it) is rejected.

// The refusal every config-reading subcommand prints, prefixed by its own name (SA §0.14 R3-a). It is
// spelled out here rather than derived from the helper so the wording is pinned, not self-certified.
func wantMissingConfig(cmd string) string {
	return cmd + ": --config is required (the argus-config.yaml of the SUT under test); there is no default"
}

// SA §0.14 R3-c: the flag's default is EMPTY. A parse with no arguments must leave configPath empty —
// nothing in the binary points at the demo any more.
func TestConfigFlag_HasNoDefault(t *testing.T) {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cf.bind(fs)
	f := fs.Lookup("config")
	if f == nil {
		t.Fatal("--config is not bound on the common flagset")
	}
	if f.DefValue != "" {
		t.Fatalf("--config has a default %q; VR10-R3 says there is none", f.DefValue)
	}
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse with no arguments: %v", err)
	}
	if cf.configPath != "" {
		t.Fatalf("configPath after an empty parse = %q, want empty", cf.configPath)
	}
}

// VR10-R3-1/2/3 through the real `dispatch`: each subcommand that reads the SUT's argus-config.yaml,
// given no --config, exits non-zero with the refusal that names the flag — BEFORE any `run_begin` is
// emitted and before any run id lands on disk. The token pair is configured so the token gate is not
// what refuses; a refusal for any other reason (a missing demo file, an unreachable SUT, a listen
// error) is the bug this test exists to catch.
func TestDispatch_ConfigReadingSubcommandsRefuseWithoutConfig(t *testing.T) {
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_TOKEN", "author-test-token")
	t.Setenv("ARGUS_CP_URL", "")
	t.Setenv("ARGUS_MODE", "")
	results := t.TempDir()
	// run-direct minted its identity and set cache under ./results by default; keep them in the sandbox.
	t.Setenv("ARGUS_IDENTITY_PATH", filepath.Join(results, "identity.key"))
	t.Setenv("ARGUS_SET_CACHE", filepath.Join(results, "set-cache.json"))

	cases := []struct {
		cmd   string
		extra []string
	}{
		{"run", nil},
		{"run-direct", nil},
		{"validate-config", nil},
		{"capabilities", nil},
		{"render-obs", nil},
		{"render-k8s", nil},
		{"secrets-scan", nil},
		{"onboard-guard", nil},
		{"preflight-auth", nil},
		{"select-image", nil},
		// an address that cannot be listened on: if the refusal does not come first, serve must
		// still return instead of blocking the test on a live listener.
		{"serve", []string{"--addr", "bad:addr:x"}},
		// no --server-url: the endpoint has to come from the config, so the config is required.
		{"mcp-call", []string{"--tool", "t"}},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			args := append([]string{c.cmd, "--results", results, "--instance-id", "vr10r3"}, c.extra...)
			rc := exitOK
			out := captureStdout(t, func() { rc = dispatch(args) })
			if rc == exitOK {
				t.Errorf("%s without --config exited 0; want a non-zero refusal\ngot: %s", c.cmd, out)
			}
			if !strings.Contains(out, wantMissingConfig(c.cmd)) {
				t.Errorf("%s without --config did not print the refusal naming the flag.\nwant: %s\ngot:  %s",
					c.cmd, wantMissingConfig(c.cmd), out)
			}
			if strings.Contains(out, "run_begin") || strings.Contains(out, "run_id") {
				t.Errorf("%s without --config reached a run (run_begin/run_id in the output); the refusal must come at flag parsing\ngot: %s", c.cmd, out)
			}
		})
	}

	// VR10-R3-3, the executor-disk half: a refusal mints no run id, so nothing appears under
	// results/<instance>/runs/ and no report is written.
	for _, pat := range []string{
		filepath.Join(results, "*", "runs", "*.json"),
		filepath.Join(results, "*", "report.json"),
	} {
		if m, _ := filepath.Glob(pat); len(m) != 0 {
			t.Errorf("a refused run left artefacts on disk: %v", m)
		}
	}
}

// The subcommands that are gated, and the ones that are not, as ONE table — so the list is a decision
// that can be read, not a side effect of where each command happens to load its config.
//
//   - `serve --mode control` is the control plane: it has no SUT and no argus-config, and the CP image's
//     ENTRYPOINT carries no --config (deploy/control/Dockerfile). Gating it would take the CP down.
//   - `mcp-call --server-url …` drives the SUT purely by flags (its documented flags-only mode); without
//     --server-url the endpoint has to come from the config, so the config is required.
//   - get-report / get-sagas / tail-logs / get-dashboard-url read the config OPPORTUNISTICALLY (the
//     log-field translation, the tier's Grafana base) and fall back by design; byo-smoke.sh calls two of
//     them without --config and must keep working.
func TestConfigRequired_Table(t *testing.T) {
	for _, cmd := range []string{"run", "run-direct", "validate-config", "capabilities", "render-obs", "render-k8s",
		"secrets-scan", "onboard-guard", "preflight-auth", "select-image", "serve", "mcp-call"} {
		if !configRequired(cmd, &commonFlags{}) {
			t.Errorf("%s reads the SUT's config and must require --config", cmd)
		}
	}
	if !configRequired("serve", &commonFlags{mode: "runner"}) {
		t.Error("serve --mode runner runs the SUT and must require --config")
	}
	if configRequired("serve", &commonFlags{mode: "control"}) {
		t.Error("serve --mode control is the control plane; it has no SUT config and must NOT require --config")
	}
	t.Setenv("ARGUS_MODE", "control")
	if configRequired("serve", &commonFlags{}) {
		t.Error("serve with ARGUS_MODE=control is the control plane; it must NOT require --config")
	}
	t.Setenv("ARGUS_MODE", "")
	if configRequired("mcp-call", &commonFlags{serverURL: "http://sut:8080/mcp"}) {
		t.Error("mcp-call --server-url drives the SUT purely by flags and must NOT require --config")
	}
	for _, cmd := range []string{"get-report", "get-sagas", "tail-logs", "get-dashboard-url", "mark-deployment",
		"cloud-login", "cloud-deregister", "version", "init", "router", "keygen", "runner-state", "package-check",
		"validate-scenario", "propose-scenario", "list-scenarios", "read-scenario", "write-scenario", "delete-scenario"} {
		if configRequired(cmd, &commonFlags{}) {
			t.Errorf("%s does not need the SUT's config and must NOT be gated on --config", cmd)
		}
	}
}

// VR10-R3-4: the demo path is GONE from the binary's defaults under cmd/, not merely bypassed — the
// PO's acceptance grep for it over cmd/ returns nothing. The needle is concatenated so this file is not
// its own hit (and the literal appears nowhere else in this file for the same reason).
func TestNoDemoConfigDefaultUnderCmd(t *testing.T) {
	needle := "demo-packaging/" + "argus-config.yaml"
	var hits []string
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), needle) {
			hits = append(hits, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("the demo config path is still a default under cmd/: %v", hits)
	}
}

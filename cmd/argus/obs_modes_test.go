package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// obsRenderOutput is render-k8s's JSON output, read by the --obs tests below (a local superset of
// renderK8sOutput in render_next_test.go — this file needs the obs-specific fields too).
type obsRenderOutput struct {
	Rendered  bool   `json:"rendered"`
	Executor  string `json:"executor"`
	Obs       string `json:"obs"`
	ObsMode   string `json:"obs_mode"`
	ObsLoki   string `json:"obs_loki_url"`
	ObsPushGW string `json:"obs_pushgateway_url"`
	// ObsCredentialVar (T3.1) is the bare ${VAR} NAME behind the export credential — never the
	// value — onboard.sh reads to create the argus-obs-credential Secret.
	ObsCredentialVar string `json:"obs_credential_var"`
	Error            string `json:"error"`
}

// runRenderK8sWithConfig is runRenderK8s (render_next_test.go) with a CALLER-SUPPLIED
// argus-config.yaml body, needed for the --obs=adopt tests: they must declare (or omit)
// observability.loki.url / observability.pushgateway.url, which the fixed config in
// runRenderK8s never carries.
func runRenderK8sWithConfig(t *testing.T, configBody string, extra ...string) (int, obsRenderOutput, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_ENROLLMENT_TOKEN", "enrollment-test-token")
	t.Setenv("ARGUS_CP_URL", "https://cp.example.invalid")
	t.Setenv("ARGUS_WORKSPACE_ID", "ws_test")
	out := filepath.Join(dir, "out")

	args := append([]string{"render-k8s", "--config", cfg, "--instance-id", "obs-modes-test",
		"--sut-namespace", "sut", "--image", "registry.invalid/argus@sha256:" + strings.Repeat("0", 64),
		"--tier", "aks", "--replicas", "1", "--out", out,
		// --collect-sut-logs default OFF; this file's tests predate the flag and were written
		// against the old always-on behaviour (a later --collect-sut-logs=false in extra wins).
		"--collect-sut-logs"}, extra...)
	rc := exitOK
	stdout := captureStdout(t, func() { rc = dispatch(args) })
	var got obsRenderOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("render-k8s output is not the JSON object it should be: %v\n%s", err, stdout)
	}
	return rc, got, out
}

const noObsConfig = "project:\n  name: obs-modes-test\ntargets:\n  http:\n    base_url: https://example.invalid\n"

const adoptConfig = "project:\n  name: obs-modes-test\ntargets:\n  http:\n    base_url: https://example.invalid\n" +
	"observability:\n  loki:\n    url: http://operator-loki.example:3100\n  pushgateway:\n    url: http://operator-pushgateway.example:9091\n"

const adoptConfigNoPushgateway = "project:\n  name: obs-modes-test\ntargets:\n  http:\n    base_url: https://example.invalid\n" +
	"observability:\n  loki:\n    url: http://operator-loki.example:3100\n"

// ── T3.1: the flag itself, default, and refusal shapes ──────────────────────────────────────────

// TestRenderK8s_DefaultObsModeIsBundled: no --obs flag at all must behave exactly as --obs bundled.
func TestRenderK8s_DefaultObsModeIsBundled(t *testing.T) {
	rcNoFlag, gotNoFlag, outNoFlag := runRenderK8sWithConfig(t, noObsConfig)
	if rcNoFlag != exitOK {
		t.Fatalf("no --obs flag: exit %d, want exitOK: %+v", rcNoFlag, gotNoFlag)
	}
	if gotNoFlag.ObsMode != "bundled" {
		t.Fatalf("no --obs flag: obs_mode = %q, want bundled", gotNoFlag.ObsMode)
	}
	rcExplicit, gotExplicit, outExplicit := runRenderK8sWithConfig(t, noObsConfig, "--obs", "bundled")
	if rcExplicit != exitOK {
		t.Fatalf("--obs bundled: exit %d, want exitOK: %+v", rcExplicit, gotExplicit)
	}

	execNoFlag, err := os.ReadFile(filepath.Join(outNoFlag, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml (no flag): %v", err)
	}
	execExplicit, err := os.ReadFile(filepath.Join(outExplicit, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml (--obs bundled): %v", err)
	}
	if string(execNoFlag) != string(execExplicit) {
		t.Fatal("render-k8s with no --obs flag and with --obs bundled must write byte-identical executor.yaml")
	}
	obsNoFlag, err := os.ReadFile(filepath.Join(outNoFlag, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml (no flag): %v", err)
	}
	obsExplicit, err := os.ReadFile(filepath.Join(outExplicit, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml (--obs bundled): %v", err)
	}
	if string(obsNoFlag) != string(obsExplicit) {
		t.Fatal("render-k8s with no --obs flag and with --obs bundled must write byte-identical obs.yaml")
	}
	if len(obsNoFlag) == 0 {
		t.Fatal("bundled obs.yaml must not be empty")
	}
}

// TestRenderK8s_ObsExportWithNoBackendRefused (T3.1): export IS built now, but --obs export with
// neither accepted shape declared (noObsConfig has no observability block at all) is still
// refused — naming BOTH accepted shapes, never "not built" (that message described the OLD,
// pre-T3.1 behaviour; export itself changed, so the refusal for a MISCONFIGURED export run had to
// change with it — see internal/k8srender's TestInstanceValidate_exportWithNoBackendRefused).
func TestRenderK8s_ObsExportWithNoBackendRefused(t *testing.T) {
	rc, got, out := runRenderK8sWithConfig(t, noObsConfig, "--obs", "export")
	if rc == exitOK {
		t.Fatalf("--obs export with no backend declared must be refused with a non-zero exit, got exitOK: %+v", got)
	}
	for _, want := range []string{"observability.loki.url", "push_url", "credential", "observability.betterstack", "bundled", "adopt"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("export-with-no-backend refusal message must contain %q, got %q", want, got.Error)
		}
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("--obs export must write NOTHING before refusing, found %d entries in --out", len(entries))
	}
}

// TestRenderK8s_ObsUnknownValueRefused (T3.1 item 1): any value besides the three is refused and
// the message lists all three.
func TestRenderK8s_ObsUnknownValueRefused(t *testing.T) {
	rc, got, out := runRenderK8sWithConfig(t, noObsConfig, "--obs", "sideways")
	if rc == exitOK {
		t.Fatalf("--obs sideways must be refused with a non-zero exit, got exitOK: %+v", got)
	}
	for _, want := range []string{"bundled", "adopt", "export"} {
		if !strings.Contains(got.Error, want) {
			t.Errorf("unknown-value refusal must list %q, got %q", want, got.Error)
		}
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("--obs sideways must write NOTHING before refusing, found %d entries in --out", len(entries))
	}
}

// Both spellings must parse identically: --obs=<mode> and --obs <mode>. Go's flag package already
// gives StringVar both spellings for free; this pins that render-k8s does not accidentally
// override it (e.g. by hand-parsing argv elsewhere).
func TestRenderK8s_ObsFlagBothSpellings(t *testing.T) {
	rcSpace, gotSpace, _ := runRenderK8sWithConfig(t, noObsConfig, "--obs", "bundled")
	if rcSpace != exitOK {
		t.Fatalf("--obs bundled: exit %d: %+v", rcSpace, gotSpace)
	}
	rcEq, gotEq, _ := runRenderK8sWithConfig(t, noObsConfig, "--obs=bundled")
	if rcEq != exitOK {
		t.Fatalf("--obs=bundled: exit %d: %+v", rcEq, gotEq)
	}
	if gotSpace.ObsMode != gotEq.ObsMode {
		t.Fatalf("--obs bundled vs --obs=bundled: obs_mode differs (%q vs %q)", gotSpace.ObsMode, gotEq.ObsMode)
	}
}

// ── T3.2: adopt ──────────────────────────────────────────────────────────────────────────────

// TestRenderK8s_AdoptRequiresLokiURL: no observability.loki.url declared -> refused, naming the key,
// never silently falling back to the bundled default.
func TestRenderK8s_AdoptRequiresLokiURL(t *testing.T) {
	rc, got, out := runRenderK8sWithConfig(t, noObsConfig, "--obs", "adopt")
	if rc == exitOK {
		t.Fatalf("--obs adopt with no observability.loki.url must be refused, got exitOK: %+v", got)
	}
	if !strings.Contains(got.Error, "observability.loki.url") {
		t.Errorf("refusal must name observability.loki.url, got %q", got.Error)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("--obs adopt (no loki url) must write NOTHING before refusing, found %d entries", len(entries))
	}
}

// TestRenderK8s_AdoptRendersConfiguredEndpointsAndNoObsPlane is the end-to-end T3.2 promise
// exercised through the REAL CLI: with observability.loki.url + observability.pushgateway.url
// declared, render-k8s writes an executor.yaml carrying those two endpoints and an EMPTY obs.yaml.
func TestRenderK8s_AdoptRendersConfiguredEndpointsAndNoObsPlane(t *testing.T) {
	rc, got, out := runRenderK8sWithConfig(t, adoptConfig, "--obs", "adopt")
	if rc != exitOK {
		t.Fatalf("--obs adopt (loki + pushgateway declared): exit %d: %+v", rc, got)
	}
	if got.ObsMode != "adopt" {
		t.Errorf("obs_mode = %q, want adopt", got.ObsMode)
	}
	if got.ObsLoki != "http://operator-loki.example:3100" {
		t.Errorf("obs_loki_url = %q, want the declared operator Loki", got.ObsLoki)
	}
	if got.ObsPushGW != "http://operator-pushgateway.example:9091" {
		t.Errorf("obs_pushgateway_url = %q, want the declared operator Pushgateway", got.ObsPushGW)
	}
	exec, err := os.ReadFile(filepath.Join(out, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(exec), "operator-loki.example") {
		t.Errorf("executor.yaml must carry the operator's Loki url:\n%s", exec)
	}
	if !strings.Contains(string(exec), "operator-pushgateway.example") {
		t.Errorf("executor.yaml must carry the operator's Pushgateway url:\n%s", exec)
	}
	if strings.Contains(string(exec), "http://loki:3100") || strings.Contains(string(exec), "http://pushgateway:9091") {
		t.Errorf("adopt executor.yaml must not carry any bundled endpoint:\n%s", exec)
	}
	obs, err := os.ReadFile(filepath.Join(out, "obs.yaml"))
	if err != nil {
		t.Fatalf("read obs.yaml: %v", err)
	}
	if len(strings.TrimSpace(string(obs))) != 0 {
		t.Errorf("adopt obs.yaml must be empty, got %d bytes:\n%s", len(obs), obs)
	}
}

// TestRenderK8s_AdoptNoPushgatewayDeclared: the OPTIONAL key, absent — render-k8s must still
// succeed (never require it) and must not report an obs_pushgateway_url field at all.
func TestRenderK8s_AdoptNoPushgatewayDeclared(t *testing.T) {
	rc, got, out := runRenderK8sWithConfig(t, adoptConfigNoPushgateway, "--obs", "adopt")
	if rc != exitOK {
		t.Fatalf("--obs adopt (no pushgateway declared) must succeed: exit %d: %+v", rc, got)
	}
	if got.ObsPushGW != "" {
		t.Errorf("obs_pushgateway_url should be empty/absent when undeclared, got %q", got.ObsPushGW)
	}
	exec, err := os.ReadFile(filepath.Join(out, "executor.yaml"))
	if err != nil {
		t.Fatalf("read executor.yaml: %v", err)
	}
	if !strings.Contains(string(exec), "- --pushgateway\n            - \"\"\n") {
		t.Errorf("executor.yaml must carry an EXPLICIT empty --pushgateway when none is configured:\n%s", exec)
	}
}

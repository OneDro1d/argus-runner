package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `validate-scenario` parses a local file (and optionally a local config). It reaches no control plane and
// no SUT, so like `validate-config` it needs no ARGUS_RUNNER_TOKEN / ARGUS_EXECUTOR_SECRET and no --token.
// A --token that is still passed (old invocations, the docs of before) is accepted and ignored.

// a scenario the shape-only check accepts: the fixture of validate_scenario_schema_test.go, seq filled in
var validNoTokenScenario = strings.Replace(pingSchemaScenarioMD, "%s", "7", 1)

// runValidateScenarioBare runs the command with NO token variable in the environment at all.
func runValidateScenarioBare(t *testing.T, body string, extra ...string) (rc int, stdout string) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "scenario.md")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"ARGUS_RUNNER_TOKEN", "ARGUS_EXECUTOR_SECRET", "ARGUS_AUTHOR_TOKEN", "ARGUS_TOKEN"} {
		t.Setenv(v, "")
	}
	args := append([]string{"validate-scenario", "--file", f}, extra...)
	stdout = captureStdout(t, func() { rc = dispatch(args) })
	return rc, stdout
}

func TestValidateScenario_NeedsNoTokenAndNoEnv(t *testing.T) {
	rc, stdout := runValidateScenarioBare(t, validNoTokenScenario)
	if rc != exitOK {
		t.Fatalf("validate-scenario with no env vars and no --token: rc=%d, want exitOK: %s", rc, stdout)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if v, _ := out["valid"].(bool); !v {
		t.Fatalf("valid must be true: %s", stdout)
	}
}

func TestValidateScenario_InvalidStillFailsWithoutToken(t *testing.T) {
	rc, stdout := runValidateScenarioBare(t, "# not a scenario\n")
	if rc == exitOK || rc == exitDenied {
		t.Fatalf("a malformed scenario must fail validation (not exitOK, not exitDenied); rc=%d: %s", rc, stdout)
	}
	if strings.Contains(stdout, "auth not configured") || strings.Contains(stdout, "denied") {
		t.Fatalf("the auth gate answered, not the validator: %s", stdout)
	}
	if !strings.Contains(stdout, `"valid"`) {
		t.Fatalf("no validation verdict in the output: %s", stdout)
	}
}

func TestValidateScenario_MissingFileStillUsageWithoutToken(t *testing.T) {
	for _, v := range []string{"ARGUS_RUNNER_TOKEN", "ARGUS_EXECUTOR_SECRET", "ARGUS_AUTHOR_TOKEN", "ARGUS_TOKEN"} {
		t.Setenv(v, "")
	}
	var rc int
	stdout := captureStdout(t, func() { rc = dispatch([]string{"validate-scenario"}) })
	if rc != exitUsage || !strings.Contains(stdout, "validate-scenario needs --file") {
		t.Fatalf("rc=%d out=%s; want exitUsage naming --file", rc, stdout)
	}
}

// Old invocations pass --token local-author: accepted, ignored — even one that matches no configured hat,
// and even the runner hat, which the scenario-side gate used to refuse ("not permitted for the product scope").
func TestValidateScenario_AnyPassedTokenIsAcceptedAndIgnored(t *testing.T) {
	for name, extra := range map[string][]string{
		"unrelated token": {"--token", "local-author"},
		"empty token":     {"--token", ""},
	} {
		t.Run(name, func(t *testing.T) {
			rc, stdout := runValidateScenarioBare(t, validNoTokenScenario, extra...)
			if rc != exitOK {
				t.Fatalf("rc=%d: %s", rc, stdout)
			}
		})
	}
	// with a hat map configured and the RUNNER hat presented: formerly denied, now just validates
	f := filepath.Join(t.TempDir(), "scenario.md")
	if err := os.WriteFile(f, []byte(validNoTokenScenario), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_EXECUTOR_SECRET", "author-test-token")
	var rc int
	stdout := captureStdout(t, func() {
		rc = dispatch([]string{"validate-scenario", "--file", f, "--token", "runner-test-token"})
	})
	if rc != exitOK {
		t.Fatalf("runner hat presented: rc=%d: %s", rc, stdout)
	}
}

func TestValidateScenario_UsageAndHelpSayNoTokenIsNeeded(t *testing.T) {
	var rc int
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { rc = dispatch([]string{"validate-scenario", "--help"}) })
	})
	if rc != exitOK {
		t.Fatalf("--help rc=%d", rc)
	}
	if !strings.Contains(stdout+stderr, "no token") {
		t.Errorf("`validate-scenario --help` must say it needs no token:\n%s%s", stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "-config") {
		t.Errorf("`validate-scenario --help` must list --config, which the command reads:\n%s%s", stdout, stderr)
	}
	out := captureStdout(t, func() { rc = dispatch([]string{"help"}) })
	if !strings.Contains(out, "validate-scenario --file") || !strings.Contains(out, "no token needed") {
		t.Errorf("the usage listing does not describe validate-scenario as tokenless:\n%s", out)
	}
}

package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// up_token_redaction_test.go — #44: a credential handed to `up` or `hub register` through
// ARGUS_CP_TOKEN / ARGUS_HUB_TOKEN must never appear in --help or a flag-error's usage text, and
// --token must actually reach onboarding.

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("close stderr pipe: %v", cerr)
	}
	os.Stderr = prev
	return <-done
}

const (
	secretUpToken        = "UP-SECRET-DO-NOT-LEAK-9f3a"
	secretHubToken       = "HUB-SECRET-DO-NOT-LEAK-1e7c"
	secretDiscoveryToken = "DISCOVERY-SECRET-DO-NOT-LEAK-4b2d"
)

func TestUp_Help_DoesNotPrintTokenFromEnv(t *testing.T) {
	t.Setenv("ARGUS_CP_TOKEN", secretUpToken)
	var code int
	stderr := captureStderr(t, func() { code = cmdUp([]string{"--help"}) })
	// F-CLI-HELP-1: `--help` is a clean request answered, not a usage error — it now exits 0
	// (flag.ErrHelp is no longer folded into the generic flag-error path). The token-redaction
	// assertion below is this test's real point and is unaffected by which code carries it.
	if code != exitOK {
		t.Fatalf("cmdUp(--help) exit = %d, want %d", code, exitOK)
	}
	if strings.Contains(stderr, secretUpToken) {
		t.Fatalf("`up --help` leaked ARGUS_CP_TOKEN into its usage text:\n%s", stderr)
	}
}

func TestUp_FlagError_DoesNotPrintTokenFromEnv(t *testing.T) {
	t.Setenv("ARGUS_CP_TOKEN", secretUpToken)
	var code int
	stderr := captureStderr(t, func() { code = cmdUp([]string{"--not-a-real-flag"}) })
	if code != exitUsage {
		t.Fatalf("cmdUp(--not-a-real-flag) exit = %d, want %d", code, exitUsage)
	}
	if strings.Contains(stderr, secretUpToken) {
		t.Fatalf("an `up` flag error leaked ARGUS_CP_TOKEN into its usage text:\n%s", stderr)
	}
}

// TestUp_TokenFlag_PassesThroughToOnboardingEnv_NeverArgv proves --token reaches onboard.sh's own
// environment (the same variable it already reads unprompted, onboard.sh:962) rather than being
// dropped on the floor (the old bug: `a.Token` had exactly one reference, its own flag default) or
// rendered into argv, which onboard.sh's own comment says a token must never do (visible via ps).
func TestUp_TokenFlag_PassesThroughToOnboardingEnv_NeverArgv(t *testing.T) {
	a := upArgs{Token: "explicit-cli-token-value"}
	env := onboardEnv(a, []string{"PATH=/bin"})
	var found bool
	for _, kv := range env {
		if kv == "ARGUS_CP_TOKEN=explicit-cli-token-value" {
			found = true
		}
	}
	if !found {
		t.Fatalf("onboardEnv did not thread --token through as ARGUS_CP_TOKEN: %v", env)
	}
}

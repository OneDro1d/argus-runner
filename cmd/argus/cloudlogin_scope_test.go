package main

import (
	"os"
	"strings"
	"testing"
)

// cloudlogin_scope_test.go — msgbus tester follow-up item 2 (2026-09-28): `cloud-login --scope`
// used to default to "author" (CLI help: `-scope … (default "author")`), the same foot-gun P0-2
// closed server-side for an unscoped OAuth sign-in. Decision: NO default. Without --scope,
// cloud-login refuses, naming both accepted values and what each is for, exits non-zero, and
// writes nothing (no session file, no --token-out file).

// runCloudLogin drives dispatch(["cloud-login", args...]) with a clean token env (same posture
// runHelp/help_every_command_test.go uses) and returns (exit code, combined stdout+stderr).
func runCloudLogin(t *testing.T, args ...string) (int, string) {
	t.Helper()
	for _, v := range []string{
		"ARGUS_TOKEN", "ARGUS_RUNNER_TOKEN", "ARGUS_EXECUTOR_SECRET", "ARGUS_AUTHOR_TOKEN",
		"ARGUS_CP_AUTHOR_TOKEN", "ARGUS_CP_TOKEN", "ARGUS_HUB_TOKEN",
	} {
		t.Setenv(v, "")
	}
	var rc int
	var out, errOut string
	errOut = captureStderr(t, func() {
		out = captureStdout(t, func() {
			rc = dispatch(append([]string{"cloud-login"}, args...))
		})
	})
	return rc, out + errOut
}

func TestCloudLogin_NoScopeFlag_RefusesNamingBothValuesAndWritesNothing(t *testing.T) {
	sessionDir := t.TempDir()
	sessionFile := sessionDir + "/session.json"
	t.Setenv("ARGUS_SESSION_FILE", sessionFile)
	t.Setenv("ARGUS_CP_URL", "http://127.0.0.1:1") // never actually dialed if the refusal is early enough
	rc, out := runCloudLogin(t, "--control-plane", "http://127.0.0.1:1")
	if rc == exitOK {
		t.Fatalf("cloud-login with no --scope: exit %d, want non-zero. output:\n%s", rc, out)
	}
	for _, want := range []string{"--scope", "author", "runner"} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-login refusal does not mention %q:\n%s", want, out)
		}
	}
	// "author" and "runner" must each be explained, not just named.
	if !strings.Contains(out, "operator") && !strings.Contains(out, "tester") {
		t.Errorf("cloud-login refusal does not explain what --scope author is for:\n%s", out)
	}
	if !strings.Contains(out, "builder") {
		t.Errorf("cloud-login refusal does not explain what --scope runner is for:\n%s", out)
	}
	if _, err := os.Stat(sessionFile); err == nil {
		t.Errorf("cloud-login with no --scope must write nothing, but a session file exists")
	}
}

func TestCloudLogin_EmptyScopeFlag_AlsoRefuses(t *testing.T) {
	rc, out := runCloudLogin(t, "--control-plane", "http://127.0.0.1:1", "--scope", "")
	if rc == exitOK {
		t.Fatalf("cloud-login with --scope '': exit %d, want non-zero. output:\n%s", rc, out)
	}
	if !strings.Contains(out, "--scope") {
		t.Errorf("cloud-login refusal does not mention --scope:\n%s", out)
	}
}

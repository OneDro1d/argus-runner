package main

// credential_banner_test.go — F-CRED-1: a tester (msgbus, 2026-09-27) acted in the wrong Argus
// workspace without noticing, because sessionForCommand's credential resolution (--token, then
// ARGUS_CP_AUTHOR_TOKEN/ARGUS_CP_TOKEN, then the session file `argus cloud-login` wrote) was entirely
// silent. These tests drive the REAL CLI command (`cloud-list-workspaces`, against an unreachable
// control plane so no network flakiness enters) and assert on the stderr banner it prints — never on
// an internal helper's return value alone, so a wiring regression (the banner computed but never
// printed) fails the same way a logic regression would.

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
	"github.com/OneDro1d/argus-runner/internal/retry"
)

// credFakeJWT builds a bare unsigned JWT carrying only a "workspace" claim — enough for
// onboard.WorkspaceFromToken, nothing else needs to be real.
func credFakeJWT(workspace string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"workspace":"` + workspace + `"}`))
	return header + "." + payload + ".sig"
}

// unreachableCP is a control-plane URL nothing listens on — every setup below hits it and fails on
// the network call, which is fine: the banner prints BEFORE that call, so the assertion is on stderr
// captured around the whole dispatch, not on the (deliberately failing) result.
const unreachableCP = "http://127.0.0.1:1"

// fastRetry collapses the VR-F2b retry policy's inter-attempt waits (1s/3s/8s) to ~0 for the
// duration of one test — these tests only care about the credential banner printed BEFORE the
// (deliberately failing) network call, not about proving the retry policy itself, and paying its
// real wall-clock budget four times over would make this file needlessly slow.
func fastRetry(t *testing.T) {
	t.Helper()
	orig := retry.Waits
	retry.Waits = []time.Duration{0, 0, 0}
	t.Cleanup(func() { retry.Waits = orig })
}

func TestCredentialBanner_SessionFileOnly_NamesFileAndWorkspace(t *testing.T) {
	fastRetry(t)
	path := filepath.Join(t.TempDir(), "session.json")
	if err := onboard.SaveSession(path, onboard.SessionData{
		ControlPlane: unreachableCP,
		AccessToken:  credFakeJWT("ws_from_session"),
		Workspace:    "ws_from_session",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	t.Setenv(onboard.SessionEnvOverride, path)
	t.Setenv(envname.CPAuthorToken, "")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")

	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			dispatch([]string{"cloud-list-workspaces", "--control-plane", unreachableCP})
		})
	})
	if !strings.Contains(stderr, "credential: session file "+path) {
		t.Errorf("banner did not name the session file. stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "workspace: ws_from_session") {
		t.Errorf("banner did not name the workspace. stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, unreachableCP) {
		t.Errorf("banner did not name the control plane. stderr:\n%s", stderr)
	}
}

func TestCredentialBanner_EnvToken_NamesVarAndHonestlyUnknownWorkspace(t *testing.T) {
	fastRetry(t)
	t.Setenv(envname.CPAuthorToken, "FAKESENTINELVALUE")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")

	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			dispatch([]string{"cloud-list-workspaces", "--control-plane", unreachableCP})
		})
	})
	if !strings.Contains(stderr, "credential: "+envname.CPAuthorToken) {
		t.Errorf("banner did not name the env var. stderr:\n%s", stderr)
	}
	// An opaque (non-JWT) token's workspace cannot be decoded locally — the banner must say so
	// honestly rather than guess (PROMISE #1).
	if !strings.Contains(stderr, unknownWorkspace) {
		t.Errorf("banner did not say the workspace is unknown for an opaque token. stderr:\n%s", stderr)
	}
	if strings.Contains(stderr, "FAKESENTINELVALUE") {
		t.Errorf("the token VALUE must never be printed. stderr:\n%s", stderr)
	}
}

func TestCredentialBanner_EmptyEnvVar_WarnsLoudlyAndFallsBackWithoutRefusing(t *testing.T) {
	fastRetry(t)
	// The onboarding kit's own shape (onboard.sh/teardown.sh): ARGUS_CP_TOKEN="${ARGUS_CP_TOKEN:-}"
	// exports the var SET but EMPTY and relies on the session-file fallback. Refusing here would
	// break onboarding (PROMISE #1) — this must warn, not refuse.
	path := filepath.Join(t.TempDir(), "session.json")
	if err := onboard.SaveSession(path, onboard.SessionData{
		ControlPlane: unreachableCP,
		AccessToken:  credFakeJWT("ws_fallback"),
		Workspace:    "ws_fallback",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	t.Setenv(onboard.SessionEnvOverride, path)
	t.Setenv(envname.CPAuthorToken, "") // SET but empty — os.LookupEnv sees ok=true, v==""
	t.Setenv(envname.CPAuthorTokenDeprecated, "")

	var rc int
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			rc = dispatch([]string{"cloud-list-workspaces", "--control-plane", unreachableCP})
		})
	})
	if !strings.Contains(stderr, "WARNING: "+envname.CPAuthorToken+" is set but empty") {
		t.Errorf("no loud warning for the empty var. stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "credential: session file "+path) {
		t.Errorf("did not fall back to the session file. stderr:\n%s", stderr)
	}
	// It must not be treated as a refusal at the flag-gate level (exitUsage=2/exitDenied=3): the
	// command still RUNS (and then fails on the network, exitErr=1, which is fine here).
	if rc == exitUsage || rc == exitDenied {
		t.Errorf("empty-var fallback was refused (rc=%d) instead of warned-and-continued", rc)
	}
}

func TestCredentialBanner_FlagToken_TakesPrecedenceOverEnv(t *testing.T) {
	fastRetry(t)
	t.Setenv(envname.CPAuthorToken, "shouldnotbeused")
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			dispatch([]string{"cloud-list-workspaces", "--control-plane", unreachableCP, "--token", credFakeJWT("ws_from_flag")})
		})
	})
	if !strings.Contains(stderr, "credential: --token flag") {
		t.Errorf("banner did not credit the --token flag. stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "workspace: ws_from_flag") {
		t.Errorf("banner did not decode the flag token's workspace. stderr:\n%s", stderr)
	}
}

func TestDescribeEnvToken_PrecedenceMatchesLookup(t *testing.T) {
	t.Setenv(envname.CPAuthorToken, "")
	t.Setenv(envname.CPAuthorTokenDeprecated, "old-value")
	tok, src := describeEnvToken()
	if tok != "old-value" {
		t.Errorf("token = %q, want %q (deprecated fallback)", tok, "old-value")
	}
	if !strings.Contains(src, envname.CPAuthorTokenDeprecated) {
		t.Errorf("source = %q, want it to name %q", src, envname.CPAuthorTokenDeprecated)
	}

	t.Setenv(envname.CPAuthorToken, "new-value")
	tok, src = describeEnvToken()
	if tok != "new-value" || src != envname.CPAuthorToken {
		t.Errorf("new-name-wins: got (%q, %q)", tok, src)
	}
}

func TestExplicitlyEmptyTokenVar(t *testing.T) {
	t.Setenv(envname.CPAuthorToken, "")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")
	if got := explicitlyEmptyTokenVar(); got != envname.CPAuthorToken {
		t.Errorf("got %q, want the new name reported first", got)
	}

	// A genuinely UNSET var (not exported at all) is not "explicitly empty" — nothing to warn about.
	os.Unsetenv(envname.CPAuthorToken)
	os.Unsetenv(envname.CPAuthorTokenDeprecated)
	if got := explicitlyEmptyTokenVar(); got != "" {
		t.Errorf("got %q, want \"\" for a genuinely unset var", got)
	}
}

func TestWorkspaceOf_OpaqueTokenIsHonestlyUnknown(t *testing.T) {
	if got := workspaceOf("odts_notAJwtAtAll"); got != unknownWorkspace {
		t.Errorf("workspaceOf(opaque) = %q, want %q", got, unknownWorkspace)
	}
	if got := workspaceOf(credFakeJWT("ws_real")); got != "ws_real" {
		t.Errorf("workspaceOf(jwt) = %q, want %q", got, "ws_real")
	}
}

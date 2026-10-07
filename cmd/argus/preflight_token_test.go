package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// TestResolveCPToken pins the ONE behaviour this check exists for: preflight must find a credential
// wherever a cloud-* command would find one. The regression it guards is real and was measured on
// example-cluster 2026-09-23 — a machine whose author calls were succeeding was told
// `control-plane-token: missing`, `verdict: blocked`, `fix: argus cloud-login`, because preflight
// read ARGUS_CP_TOKEN and nothing else while every other command reads the session file.
//
// ⛔ Every case asserts the SOURCE as well as the verdict. "Missing" is only honest if the report
// also says where it looked; a bare false is how the old behaviour looked correct.
func TestResolveCPToken(t *testing.T) {
	const cp = "https://argus-dev.onedroid.ai"

	write := func(t *testing.T, d onboard.SessionData) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "session.json")
		blob, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("marshal session: %v", err)
		}
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			t.Fatalf("write session: %v", err)
		}
		return path
	}

	t.Run("ARGUS_CP_TOKEN wins, and names itself", func(t *testing.T) {
		t.Setenv(cpTokenEnv, "a-token-value-that-must-never-be-echoed")
		t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "nothing.json"))
		present, source := resolveCPToken(cp)
		if !present {
			t.Fatalf("an explicit %s was reported absent", cpTokenEnv)
		}
		if source != cpTokenEnv {
			t.Fatalf("source = %q, want %q", source, cpTokenEnv)
		}
	})

	// The rename: a credential only in the DEPRECATED name is still found, and the report names
	// that name — not the new one it is not in.
	t.Run("the deprecated ARGUS_CP_TOKEN alone is found, and named as itself", func(t *testing.T) {
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "a-token-value-that-must-never-be-echoed")
		t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "nothing.json"))
		present, source := resolveCPToken(cp)
		if !present {
			t.Fatalf("a credential in %s was reported absent", envname.CPAuthorTokenDeprecated)
		}
		if source != envname.CPAuthorTokenDeprecated {
			t.Fatalf("source = %q, want %q", source, envname.CPAuthorTokenDeprecated)
		}
	})

	// THE REGRESSION. A logged-in machine, no env var: every cloud-* command works here, so
	// preflight must not send the operator back through cloud-login.
	t.Run("the session file alone is a credential", func(t *testing.T) {
		path := write(t, onboard.SessionData{ControlPlane: cp, AccessToken: "x", RefreshToken: "y", Scope: "author"})
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "")
		t.Setenv(onboard.SessionEnvOverride, path)
		present, source := resolveCPToken(cp)
		if !present {
			t.Fatalf("a session file at %s was reported as no credential — this is the bug this test exists for", path)
		}
		if source != path {
			t.Fatalf("source = %q, want the session path %q", source, path)
		}
	})

	t.Run("a trailing slash is not a different control plane", func(t *testing.T) {
		path := write(t, onboard.SessionData{ControlPlane: cp + "/", AccessToken: "x"})
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "")
		t.Setenv(onboard.SessionEnvOverride, path)
		if present, _ := resolveCPToken(cp); !present {
			t.Fatal("a session whose URL differs only by a trailing slash was refused")
		}
	})

	// A session is issued BY one control plane and refused by every other, so holding one for a
	// different CP is not holding a credential for this one.
	t.Run("a session for another control plane is not a credential", func(t *testing.T) {
		path := write(t, onboard.SessionData{ControlPlane: "https://argus-other.example.com", AccessToken: "x"})
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "")
		t.Setenv(onboard.SessionEnvOverride, path)
		present, source := resolveCPToken(cp)
		if present {
			t.Fatal("a session for another control plane was accepted as a credential for this one")
		}
		if !strings.Contains(source, "argus-other.example.com") {
			t.Fatalf("source = %q — it must NAME the control plane the session is for, or the operator cannot tell this apart from having never logged in", source)
		}
	})

	t.Run("no session file: the report names both places it looked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session.json")
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "")
		t.Setenv(onboard.SessionEnvOverride, path)
		present, source := resolveCPToken(cp)
		if present {
			t.Fatal("a credential was reported with neither an env var nor a session file")
		}
		if !strings.Contains(source, cpTokenEnv) || !strings.Contains(source, path) {
			t.Fatalf("source = %q, want both %s and %s named", source, cpTokenEnv, path)
		}
	})

	// The `unknown` rule (DEPLOY-ARGUS.md §1): a probe that could not run is not evidence of
	// presence. A session file that will not parse blocks exactly like an absent one.
	t.Run("an unreadable session file blocks, and says so", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "session.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "")
		t.Setenv(onboard.SessionEnvOverride, path)
		present, source := resolveCPToken(cp)
		if present {
			t.Fatal("an unparseable session file was accepted as a credential")
		}
		if !strings.Contains(source, "could not be read") {
			t.Fatalf("source = %q — an unreadable file must not read as 'never logged in'", source)
		}
	})

	t.Run("a session with no access token is not a credential", func(t *testing.T) {
		path := write(t, onboard.SessionData{ControlPlane: cp})
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "")
		t.Setenv(onboard.SessionEnvOverride, path)
		present, source := resolveCPToken(cp)
		if present {
			t.Fatal("a session file with an empty access token was accepted")
		}
		if !strings.Contains(source, "no access token") {
			t.Fatalf("source = %q, want it to say the session holds no access token", source)
		}
	})

	// --control-plane is optional (preflight reports an empty one rather than rejecting it), so an
	// absent URL must not turn a real session into a mismatch.
	t.Run("no --control-plane: the session still counts", func(t *testing.T) {
		path := write(t, onboard.SessionData{ControlPlane: cp, AccessToken: "x"})
		t.Setenv(cpTokenEnv, "")
		t.Setenv(envname.CPAuthorTokenDeprecated, "")
		t.Setenv(onboard.SessionEnvOverride, path)
		if present, _ := resolveCPToken(""); !present {
			t.Fatal("a session was refused because no --control-plane was given")
		}
	})
}

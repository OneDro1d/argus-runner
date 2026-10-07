package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cloud_switch_tokenout_test.go — issue #340: a bare `cloud-switch-workspace` used to write a live
// bearer token to ./cp-session.token in the caller's working directory. Now, without --token-out, it
// goes under the session directory (beside session.json), named for the workspace, absolute path in
// the emitted JSON. An explicit --token-out is unchanged.
//
// (cloud-login needs no change: at this SHA it writes NO token file unless --token-out is typed;
// main.go's `if cf.tokenOutSet`. The shared flag default "cp-session.token" is never written by it.)

const fakeSwitchToken = "odts_FAKE_switch_token_not_real"

func runSwitch(t *testing.T, args ...string) (int, string, string, string) {
	t.Helper()
	for _, v := range []string{
		"ARGUS_TOKEN", "ARGUS_RUNNER_TOKEN", "ARGUS_EXECUTOR_SECRET", "ARGUS_AUTHOR_TOKEN",
		"ARGUS_CP_AUTHOR_TOKEN", "ARGUS_CP_TOKEN", "ARGUS_HUB_TOKEN",
	} {
		t.Setenv(v, "")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces/switch" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"` + fakeSwitchToken + `"}`))
	}))
	t.Cleanup(srv.Close)

	cwd := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "cfgdir", "argus") // deliberately does not exist yet
	sess := filepath.Join(cfg, "session.json")
	t.Setenv("ARGUS_SESSION_FILE", sess)
	old, _ := os.Getwd()
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	full := append([]string{"cloud-switch-workspace", "--control-plane", srv.URL, "--token", "odts_FAKE_login", "--workspace", "ws-42"}, args...)
	var rc int
	var out string
	errOut := captureStderr(t, func() {
		out = captureStdout(t, func() { rc = dispatch(full) })
	})
	return rc, out + errOut, cwd, cfg
}

func TestCloudSwitchWorkspace_NoTokenOut_WritesUnderSessionDirNotCwd(t *testing.T) {
	rc, out, cwd, cfg := runSwitch(t)
	if rc != exitOK {
		t.Fatalf("exit %d, output:\n%s", rc, out)
	}
	want := filepath.Join(cfg, "cp-session.ws-42.token")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("token not written at %s: %v\noutput:\n%s", want, err, out)
	}
	if string(got) != fakeSwitchToken {
		t.Errorf("token content mismatch")
	}
	if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o700 {
		t.Errorf("created dir mode %v, want 0700", fi.Mode().Perm())
	}
	if !strings.Contains(out, `"token_file": "`+want+`"`) {
		t.Errorf("emitted JSON must carry the absolute path %s:\n%s", want, out)
	}
	if ents, _ := os.ReadDir(cwd); len(ents) != 0 {
		t.Errorf("working directory must stay empty, found %d entries (first: %s)", len(ents), ents[0].Name())
	}
	if strings.Contains(out, fakeSwitchToken) {
		t.Errorf("output leaks the token")
	}
}

func TestCloudSwitchWorkspace_ExplicitTokenOut_Unchanged(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "kit-cp-session.token")
	rc, out, cwd, cfg := runSwitch(t, "--token-out", dest)
	if rc != exitOK {
		t.Fatalf("exit %d, output:\n%s", rc, out)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != fakeSwitchToken {
		t.Fatalf("explicit --token-out not honoured: %v", err)
	}
	if !strings.Contains(out, `"token_file": "`+dest+`"`) {
		t.Errorf("emitted token_file must be the explicit path:\n%s", out)
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Errorf("session dir %s must not be created when --token-out is explicit", cfg)
	}
	if ents, _ := os.ReadDir(cwd); len(ents) != 0 {
		t.Errorf("working directory must stay empty")
	}
}

func TestCloudSwitchWorkspace_ExplicitRelativeDefaultName_StaysInCwd(t *testing.T) {
	// Typing the old default name explicitly is still an explicit choice: it lands in the cwd.
	rc, out, cwd, _ := runSwitch(t, "--token-out", "cp-session.token")
	if rc != exitOK {
		t.Fatalf("exit %d, output:\n%s", rc, out)
	}
	if _, err := os.Stat(filepath.Join(cwd, "cp-session.token")); err != nil {
		t.Errorf("explicit --token-out cp-session.token must land in cwd: %v", err)
	}
}

func TestDefaultSwitchTokenPath_SanitisesWorkspace(t *testing.T) {
	t.Setenv("ARGUS_SESSION_FILE", filepath.Join(t.TempDir(), "session.json"))
	p, err := defaultSwitchTokenPath("../evil/ws 1")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p) || filepath.Dir(p) != filepath.Dir(os.Getenv("ARGUS_SESSION_FILE")) {
		t.Errorf("path %s escapes the session dir", p)
	}
	if base := filepath.Base(p); base != "cp-session.___evil_ws_1.token" {
		t.Errorf("unexpected base %s", base)
	}
}

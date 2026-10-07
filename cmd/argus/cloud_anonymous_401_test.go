package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// REPLAY 2026-09-28 fresh-user trial, error 1, through the real command: no sign-in, no token, and a
// control plane that refuses the anonymous call. The error said "this token came from --token or
// ARGUS_CP_AUTHOR_TOKEN", and the user had neither. It must say that no credential was presented.
func TestCloudListWorkspaces_NoCredential401_NamesTheMissingCredential(t *testing.T) {
	fastRetry(t)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("an anonymous call sent Authorization: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer cp.Close()
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	for _, k := range []string{envname.CPAuthorToken, envname.CPAuthorTokenDeprecated} {
		t.Setenv(k, "") // restored after the test
		os.Unsetenv(k)  // unset, not empty: the trial's shell had neither
	}

	var rc int
	var stdout string
	_ = captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			rc = dispatch([]string{"cloud-list-workspaces", "--control-plane", cp.URL})
		})
	})
	if rc != exitErr {
		t.Errorf("rc = %d, want exitErr (%d)", rc, exitErr)
	}
	if !strings.Contains(stdout, "no credential was presented") || !strings.Contains(stdout, "argus cloud-login") {
		t.Errorf("the error does not say no credential was presented, with the fix: %s", stdout)
	}
	if strings.Contains(stdout, "came from --token") {
		t.Errorf("the error blames --token / ARGUS_CP_AUTHOR_TOKEN, and neither was set: %s", stdout)
	}
}

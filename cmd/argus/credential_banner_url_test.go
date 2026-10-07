package main

// credential_banner_url_test.go — credentialBanner printed the control-plane URL it
// was given RAW on stderr, so `https://user:password@host/…`, `?token=…` and a fragment reached logs
// and transcripts. Every branch of the banner (--token flag, env token, session file, no session,
// unresolvable session path) must render the URL through displayControlPlane.
//
// All secrets below are obviously fake.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

const (
	bannerFakePassword = "FAKEPASSWORD-not-a-real-secret-7x"
	bannerFakeToken    = "FAKEQUERYTOKEN-not-a-real-secret-9y"
	bannerFakeFragment = "FAKEFRAGMENT-not-a-real-secret-3z"
	bannerHost         = "cp.banner-test.invalid:8443"
)

// bannerLeakyCP is a control-plane URL carrying a credential in each of the three places a URL can.
var bannerLeakyCP = "https://alice:" + bannerFakePassword + "@" + bannerHost + "/base?token=" + bannerFakeToken + "#" + bannerFakeFragment

func assertBannerClean(t *testing.T, stderr string) {
	t.Helper()
	for name, secret := range map[string]string{"password": bannerFakePassword, "query token": bannerFakeToken, "fragment": bannerFakeFragment} {
		if strings.Contains(stderr, secret) {
			t.Errorf("banner leaked the URL %s %q. stderr:\n%s", name, secret, stderr)
		}
	}
	if !strings.Contains(stderr, bannerHost) {
		t.Errorf("banner lost the host %q. stderr:\n%s", bannerHost, stderr)
	}
	if !strings.Contains(stderr, "control plane") {
		t.Errorf("banner printed no control-plane line. stderr:\n%s", stderr)
	}
}

func TestCredentialBannerURL_FlagToken_NoURLSecrets(t *testing.T) {
	stderr := captureStderr(t, func() { credentialBanner(bannerLeakyCP, credFakeJWT("ws_flag")) })
	assertBannerClean(t, stderr)
}

func TestCredentialBannerURL_EnvToken_NoURLSecrets(t *testing.T) {
	t.Setenv(envname.CPAuthorToken, "FAKESENTINELVALUE")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")
	stderr := captureStderr(t, func() { credentialBanner(bannerLeakyCP, "") })
	assertBannerClean(t, stderr)
}

func TestCredentialBannerURL_SessionFile_NoURLSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := onboard.SaveSession(path, onboard.SessionData{ControlPlane: unreachableCP, AccessToken: credFakeJWT("ws_s"), Workspace: "ws_s"}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	t.Setenv(onboard.SessionEnvOverride, path)
	t.Setenv(envname.CPAuthorToken, "")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")
	stderr := captureStderr(t, func() { credentialBanner(bannerLeakyCP, "") })
	assertBannerClean(t, stderr)
	if !strings.Contains(stderr, "session file "+path) {
		t.Errorf("did not reach the session-file branch. stderr:\n%s", stderr)
	}
}

func TestCredentialBannerURL_NoSession_NoURLSecrets(t *testing.T) {
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv(envname.CPAuthorToken, "")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")
	stderr := captureStderr(t, func() { credentialBanner(bannerLeakyCP, "") })
	assertBannerClean(t, stderr)
	if !strings.Contains(stderr, "no session at") {
		t.Errorf("did not reach the no-session branch. stderr:\n%s", stderr)
	}
}

func TestCredentialBannerURL_SessionPathUnresolvable_NoURLSecrets(t *testing.T) {
	t.Setenv(onboard.SessionEnvOverride, "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(envname.CPAuthorToken, "")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")
	stderr := captureStderr(t, func() { credentialBanner(bannerLeakyCP, "") })
	assertBannerClean(t, stderr)
	if !strings.Contains(stderr, "could not be resolved") {
		t.Errorf("did not reach the unresolvable-path branch. stderr:\n%s", stderr)
	}
}

// Through the real command path: the URL arrives as --control-plane and the banner is printed by
// dispatch, not by a direct call.
func TestCredentialBannerURL_ThroughDispatch_NoURLSecrets(t *testing.T) {
	fastRetry(t)
	t.Setenv(envname.CPAuthorToken, "")
	t.Setenv(envname.CPAuthorTokenDeprecated, "")
	leaky := "http://alice:" + bannerFakePassword + "@127.0.0.1:1/?token=" + bannerFakeToken + "#" + bannerFakeFragment
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			dispatch([]string{"cloud-list-workspaces", "--control-plane", leaky, "--token", credFakeJWT("ws_d")})
		})
	})
	for _, secret := range []string{bannerFakePassword, bannerFakeToken, bannerFakeFragment} {
		if strings.Contains(stderr, secret) {
			t.Errorf("dispatch stderr leaked %q:\n%s", secret, stderr)
		}
	}
	if !strings.Contains(stderr, "127.0.0.1:1") {
		t.Errorf("banner lost the host. stderr:\n%s", stderr)
	}
}

func TestCredentialBannerURL_UnparseableNotEchoed(t *testing.T) {
	bad := "ht tp://%zz" + bannerFakePassword
	stderr := captureStderr(t, func() { credentialBanner(bad, credFakeJWT("ws_x")) })
	if strings.Contains(stderr, bannerFakePassword) || strings.Contains(stderr, "%zz") {
		t.Errorf("an unparseable control-plane value was echoed. stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "<control-plane-url>") {
		t.Errorf("no placeholder for the unparseable value. stderr:\n%s", stderr)
	}
}

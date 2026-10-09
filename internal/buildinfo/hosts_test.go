package buildinfo

import (
	"strings"
	"testing"
)

// The defaults are OneDroid's own addresses. This test pins them so that changing one is a deliberate
// act: a reviewer sees this file change. A build that sets none of the -X flags must behave exactly
// as every build before this setting existed.
func TestHosts_DefaultsArePinnedToToday(t *testing.T) {
	for name, c := range map[string]struct{ got, want string }{
		"ControlPlaneURL": {ControlPlaneURL, "https://argus-dev.onedroid.ai"},
		"DocsURL":         {DocsURL, "https://docs.onedroid.ai"},
		"GrafanaURL":      {GrafanaURL, "https://grafana.example.com"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (the value every release has shipped)", name, c.got, c.want)
		}
	}
	if err := ValidateHosts(); err != nil {
		t.Errorf("the defaults must pass their own validation: %v", err)
	}
	if got, want := DocsRef("argus-tester-guide"), "docs.onedroid.ai/argus-tester-guide"; got != want {
		t.Errorf("DocsRef = %q, want %q (the hint has always been written without the scheme)", got, want)
	}
}

func setHosts(t *testing.T, cp, docs, grafana string) {
	t.Helper()
	oc, od, og := ControlPlaneURL, DocsURL, GrafanaURL
	ControlPlaneURL, DocsURL, GrafanaURL = cp, docs, grafana
	t.Cleanup(func() { ControlPlaneURL, DocsURL, GrafanaURL = oc, od, og })
}

// The rule: each address is an absolute https URL with a host and nothing else. Empty is refused.
func TestHosts_ValidationRule(t *testing.T) {
	const ok = "https://argus.example.com"
	bad := map[string]string{
		"empty":              "",
		"plain http":         "http://argus.example.com",
		"no scheme":          "argus.example.com",
		"no host":            "https://",
		"whitespace":         "https://argus.example.com /x",
		"user info":          "https://user:pw@argus.example.com",
		"query":              "https://argus.example.com/?a=1",
		"fragment":           "https://argus.example.com/#top",
		"other scheme":       "ftp://argus.example.com",
		"scheme only slash":  "https:///path",
		"trailing space":     "https://argus.example.com ",
		"bare question mark": "https://argus.example.com?",
	}
	for name, v := range bad {
		for field := 0; field < 3; field++ {
			cp, docs, gr := ok, ok, ok
			want := ""
			switch field {
			case 0:
				cp, want = v, "ControlPlaneURL"
			case 1:
				docs, want = v, "DocsURL"
			default:
				gr, want = v, "GrafanaURL"
			}
			setHosts(t, cp, docs, gr)
			err := ValidateHosts()
			if err == nil {
				t.Errorf("%s in %s: accepted %q, want a refusal", name, want, v)
				continue
			}
			if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "-X") {
				t.Errorf("%s in %s: the refusal %q must name the setting and say how to set it", name, want, err)
			}
		}
	}
	for _, v := range []string{ok, ok + "/", "https://argus.example.com:8443", "https://docs.example.com/base/path"} {
		setHosts(t, v, v, v)
		if err := ValidateHosts(); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
}

// A trailing slash is trimmed so a hint never prints "https://host//path".
func TestHosts_TrailingSlashIsTrimmed(t *testing.T) {
	setHosts(t, "https://cp.example.com/", "https://docs.example.com/", "https://g.example.com/")
	if got := DefaultControlPlane(); got != "https://cp.example.com" {
		t.Errorf("DefaultControlPlane = %q", got)
	}
	if got := DefaultGrafana(); got != "https://g.example.com" {
		t.Errorf("DefaultGrafana = %q", got)
	}
	if got := DocsRef("/guide"); got != "docs.example.com/guide" {
		t.Errorf("DocsRef = %q", got)
	}
}

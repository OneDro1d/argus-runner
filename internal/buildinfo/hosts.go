package buildinfo

import (
	"fmt"
	"net/url"
	"strings"
)

// Addresses built into the binary.
//
// Each variable holds one address of the estate that built this binary. The default is the value
// OneDroid has always used, so a plain `go build` behaves exactly as before. Another organisation sets
// its own value at link time:
//
//	go build -ldflags "-X <pkg>.ControlPlaneURL=https://argus.example.com \
//	                   -X <pkg>.DocsURL=https://docs.example.com \
//	                   -X <pkg>.GrafanaURL=https://grafana.example.com"
//
// <pkg> is github.com/OneDro1d/argus-runner/internal/buildinfo.
//
// Every place that used to carry the literal reads the variable instead (help text, fix hints,
// generated text). One setting changes them all.
//
// These are plain string variables with a constant default, which is the only shape `-X` can set.
// Do not turn one into a function call or a computed value: `-X` would silently stop working.
var (
	// ControlPlaneURL is the control plane a tester session talks to when nothing else says so.
	ControlPlaneURL = "https://argus-dev.onedroid.ai"
	// DocsURL is the base of the published docs site. Hints add a path to it.
	DocsURL = "https://docs.onedroid.ai"
	// GrafanaURL is the example managed-tier Grafana address shown in generated config text.
	GrafanaURL = "https://grafana.example.com"
)

// ValidateHosts checks every built-in address once, at start.
//
// The rule: each value must be an absolute https URL with a host, and nothing else. No user info, no
// query, no fragment, no spaces. A trailing slash is allowed (the accessors below trim it). An EMPTY
// value is refused too: an empty `-X` would otherwise print "" into help text and hints and look like
// a working build. The build scripts treat an unset or empty input as "use the default", so the only
// way to get an empty value here is a hand-written `-X Name=`.
//
// The caller prints the error and exits. A binary with a broken address must not run at all.
func ValidateHosts() error {
	for _, h := range []struct{ name, value string }{
		{"ControlPlaneURL", ControlPlaneURL},
		{"DocsURL", DocsURL},
		{"GrafanaURL", GrafanaURL},
	} {
		if err := validateHTTPS(h.value); err != nil {
			return fmt.Errorf("buildinfo.%s is %q: %v. Set it at build time with "+
				"-ldflags \"-X github.com/OneDro1d/argus-runner/internal/buildinfo.%s=https://host\"",
				h.name, h.value, err, h.name)
		}
	}
	return nil
}

func validateHTTPS(v string) error {
	if v == "" {
		return fmt.Errorf("it is empty")
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("it contains whitespace")
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("it is not a URL (%v)", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("it must start with https://")
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("it has no host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(v, "?") || strings.Contains(v, "#") {
		return fmt.Errorf("it must be a plain address: no user info, query or fragment")
	}
	return nil
}

// DefaultControlPlane returns the built-in control-plane URL without a trailing slash.
func DefaultControlPlane() string { return strings.TrimRight(ControlPlaneURL, "/") }

// DefaultGrafana returns the built-in example Grafana URL without a trailing slash.
func DefaultGrafana() string { return strings.TrimRight(GrafanaURL, "/") }

// DocsRef returns a docs reference for a hint, written without the scheme: "docs.onedroid.ai/path".
// That is how the hints have always read.
func DocsRef(path string) string {
	base := strings.TrimRight(DocsURL, "/")
	base = strings.TrimPrefix(base, "https://")
	return base + "/" + strings.TrimLeft(path, "/")
}

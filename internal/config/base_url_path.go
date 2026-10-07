package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// BaseURLPathWarnings names every http target whose base_url carries a path other than "/"
// .
//
// The runner keeps only host and port of a base_url (HTTPTarget.HostPort) and takes the PATH from each
// scenario's trigger URL (PathFromURL). So `base_url: http://documenso:3100/api/v2` sends every request
// to `http://documenso:3100/<scenario path>` — a 404 on the first run — while validate-config said
// "valid, no warnings" (a tester's Documenso onboarding, 2026-10-02).
//
// A WARNING that names the path and the cure, and nothing else: request building is deliberately NOT
// changed (no prepend). A config that has worked until now — a base_url path that was always ignored,
// with scenarios that already spell their full path — must behave exactly as before; a prepend would
// silently double every such path.
func (c *Config) BaseURLPathWarnings() []string {
	var out []string
	check := func(t *HTTPTarget, label string) {
		if t == nil {
			return
		}
		u, err := url.Parse(strings.TrimSpace(t.BaseURL))
		if err != nil || u.Path == "" || u.Path == "/" {
			return
		}
		// A base_url that is still a ${VAR} has no scheme or host to judge (its value is not known
		// here — `argus doctor` reads the config unresolved): not a path, so not warned about.
		if u.Scheme == "" || u.Host == "" {
			return
		}
		out = append(out, fmt.Sprintf("%s has the path %s, which the runner DROPS (it uses only host and port, and takes the path from each scenario's trigger URL) — "+
			"requests go to %s://%s<scenario path>, not under %s. Put the path in each scenario's trigger, e.g. `${INGESTION_URL}%s/<endpoint>`, and set base_url to %s://%s only",
			label, u.Path, u.Scheme, u.Host, u.Path, strings.TrimRight(u.Path, "/"), u.Scheme, u.Host))
	}
	check(c.Targets.HTTP, "targets.http.base_url")
	names := make([]string, 0, len(c.Targets.HTTPTargets))
	for n := range c.Targets.HTTPTargets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		check(c.Targets.HTTPTargets[n], "targets.http_targets."+n+".base_url")
	}
	return out
}

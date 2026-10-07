package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// TriggerHostWarnings names every scenario whose TRIGGER URL spells out a host the run will not use
// (AC-D21).
//
// The runner keeps only the PATH of a trigger URL (PathFromURL); the host always comes from
// targets.http.base_url, or from the targets.http_targets entry a **Target** names. So a scenario
// written as `GET https://utils.example/health` in a config whose base_url is retail, with no
// **Target**, probes retail's /health — and its claims then fail (or, worse, pass) against the
// wrong app. Measured on Shop dev 09-18: three "failures" that were all requests to the wrong
// service, and validate-config said nothing.
//
// A WARNING, not an error: the fix is almost always one **Target** line, and a shipped kit whose
// URLs name a laptop host (localhost) must not stop validating over it. Placeholder bases
// (`${VAR}/path`), bare paths and mcp/chain scenarios write no host and are never named.
func (c *Config) TriggerHostWarnings(scenariosDir string) []string {
	var out []string
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		s := d.Scenario
		if isMCPScenario(s.Tags) {
			continue
		}
		written, ok := hostOf(s.Trigger.URL)
		if !ok {
			continue
		}
		base := ""
		if c.Targets.HTTP != nil {
			base = c.Targets.HTTP.BaseURL
		}
		if sel, err := c.SelectTarget(s); err == nil && sel != nil && sel.HTTP != nil {
			base = sel.HTTP.BaseURL
		}
		used, ok := hostOf(base)
		if !ok || used == written {
			continue
		}
		fix := "add `- **Target**: <name>` naming the targets.http_targets entry for " + written
		if s.Target != "" {
			fix = "its **Target** " + s.Target + " points at " + used + "; fix the URL or the Target"
		}
		out = append(out, fmt.Sprintf("%s: TRIGGER URL names host %s, but the request goes to %s (only the URL's path is used) — %s",
			s.ID, written, used, fix))
	}
	return out
}

// hostOf returns a URL's host:port, lower-cased, with the scheme's default port filled in. It
// answers false for anything that does not spell out a concrete host: a bare path, a `${VAR}`
// base, or a host that is itself a placeholder.
func hostOf(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") || strings.HasPrefix(raw, "${") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || strings.Contains(u.Host, "${") {
		return "", false
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return strings.ToLower(u.Hostname()) + ":" + port, true
}

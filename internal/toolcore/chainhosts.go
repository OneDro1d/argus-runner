package toolcore

import (
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// UnprobedHost is a host:port that chain steps dial by their own full URL and that no target in the config
// names, so the executor's SUT probe (ProbeTargets, which dials exactly probeItems) never dials it.
type UnprobedHost struct {
	Addr      string   // host:port, the scheme's port when the URL names none
	Scenarios []string // IDs of the scenarios that dial it, in discovery order
}

// UnprobedChainHosts lists, sorted by address, every host a chain scenario under scenariosDir dials directly —
// an http step's url, an mcp step's server_url — that is not one of the addresses c's targets enumerate.
//
// WHY. A chain step is self-contained (chainStepCoverage): a url that does not start with ${INGESTION_URL}
// reaches whatever it names, and a chain cannot name a second http target any other way. So a catalogue that
// compares two deployments (a tester's machine, 2026-10-08: one target, 216 of 364 scenarios calling two more engines) has a
// sut_state of "ready" that says nothing about the hosts most of its scenarios call.
//
// Not judged, so not listed: a url starting with ${INGESTION_URL} (the configured base URL), a host still
// holding a ${…} reference (known only at run time), a scheme other than http/https, and a url that does not
// parse — validate-scenario owns that.
func UnprobedChainHosts(c *config.Config, scenariosDir string) []UnprobedHost {
	if c == nil || strings.TrimSpace(scenariosDir) == "" {
		return nil
	}
	probed := map[string]bool{}
	for _, it := range probeItems(c) {
		if it.addr != "" {
			probed[strings.ToLower(it.addr)] = true
		}
	}
	byAddr := map[string]*UnprobedHost{}
	for _, d := range scenario.DiscoverFiles(scenariosDir) {
		s := d.Scenario
		if s == nil || scenario.NativeEngineTag(s) != scenario.ChainTag {
			continue
		}
		steps, err := scenario.ParseChainSteps(s.Trigger.Payload)
		if err != nil {
			continue
		}
		seen := map[string]bool{} // a scenario is counted once per host, however many steps call it
		for _, st := range steps {
			raw := ""
			switch st.Type {
			case "http":
				raw = st.URL
			case "mcp":
				raw = st.ServerURL
			}
			addr := directAddr(raw)
			if addr == "" || probed[addr] || seen[addr] {
				continue
			}
			seen[addr] = true
			h := byAddr[addr]
			if h == nil {
				h = &UnprobedHost{Addr: addr}
				byAddr[addr] = h
			}
			h.Scenarios = append(h.Scenarios, s.ID)
		}
	}
	out := make([]UnprobedHost, 0, len(byAddr))
	for _, h := range byAddr {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// directAddr is the host:port a step url dials, lower-cased, or "" when it is not judged (see UnprobedChainHosts).
func directAddr(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, scenario.IngestionURLToken) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || strings.ContainsAny(u.Host, "${}") {
		return ""
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return ""
		}
	}
	return strings.ToLower(net.JoinHostPort(u.Hostname(), port))
}

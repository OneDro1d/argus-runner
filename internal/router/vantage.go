package router

import (
	"net"
	"net/url"
	"os"
	"strings"
)

// ── vantage (VR-R1) ──────────────────────────────────────────────────────────────────────────────
//
// Every upstream URL in the routing table is written by ONBOARDING, which knows the machine from the
// HOST's point of view: an executor is `http://localhost:8765/sse`, because that is the port compose
// published and the port an agent's `.mcp.json` used to name.
//
// When the router runs as a container (VR-R1's `argus-router`, `restart: unless-stopped`), that
// URL means something else entirely — `localhost` is the router's own container, which serves the
// router. Every forwarded call would loop back into the router instead of reaching the executor.
//
// HostAlias is the one value that reconciles the two vantages, and it is DECLARED rather than
// detected. A router that guessed whether it was containerised would be wrong exactly once, quietly,
// on somebody else's machine.
//
// WHY NOT CONTAINER-NETWORK ADDRESSING, which SA §2.6 prefers: the router is MACHINE-scoped and one
// per host, while executors live in PER-INSTANCE compose projects (`argus-inst-<id>`) each with
// its own network. Addressing `executor:8080` would require the router to join every instance's
// network — which cannot be expressed in a static compose file and would have to be redone on every
// onboard, by editing and recreating the very container every agent on the machine depends on. The
// published port is already there, already stable, and already what onboarding recorded.
const HostAliasEnv = "ARGUS_ROUTER_HOST_ALIAS"

// HostAliasFromEnv reads the declared alias. Empty means "no rewrite" — the correct answer for a
// router running as a host process, where the recorded URLs are already from its own vantage.
func HostAliasFromEnv() string { return strings.TrimSpace(os.Getenv(HostAliasEnv)) }

// isLoopbackHost reports whether a URL host names THIS machine from the writer's point of view.
// Only these are rewritten: any other host is either a real name that resolves the same everywhere,
// or a name the operator chose deliberately, and silently redirecting either would be worse than
// failing to connect.
func isLoopbackHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// ResolveFromVantage rewrites a loopback upstream URL so it resolves from the router's own vantage.
// Scheme, port, path and query are preserved exactly; only the host is replaced.
//
// A URL that cannot be parsed is returned UNCHANGED rather than repaired. The failure then surfaces
// where it means something — the connection attempt — instead of here, where a "fixed" URL would be
// a guess about what the operator meant.
func ResolveFromVantage(raw, hostAlias string) string {
	if hostAlias == "" || raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if !isLoopbackHost(u.Hostname()) {
		return raw
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(hostAlias, port)
	} else {
		u.Host = hostAlias
	}
	return u.String()
}

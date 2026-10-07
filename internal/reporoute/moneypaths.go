package reporoute

import "strings"

// moneyProbeLastSegment: GET paths ending in one of these segments are always propose-eligible under
// money_handling (T5.4, spec item 4) — no depth restriction, since none of these names collide with
// user-scoped data the way "status"/"info"/"version" can.
var moneyProbeLastSegment = map[string]bool{
	"health": true, "healthz": true, "livez": true, "readyz": true,
	"ready": true, "live": true, "liveness": true, "readiness": true,
	"probe": true, "ping": true, "metrics": true,
}

// isMoneyHandlingAllowedGet reports whether a GET on path may be proposed under money_handling —
// spec item 4: "propose ONLY GETs whose last path segment (or whole path) is one of: health,
// healthz, livez, readyz, ready, live, liveness, readiness, probe, ping, status (only as `/status`
// or `.../health/status`-style, not `/kyc/status`-style user data), metrics, version, info (only
// `/info`, `/version`, `/.well-known/...` at the root or directly under a service prefix)."
func isMoneyHandlingAllowedGet(path string) bool {
	if strings.HasPrefix(path, "/.well-known/") {
		return true
	}
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return false
	}
	segs := strings.Split(trimmed, "/")
	last := strings.ToLower(segs[len(segs)-1])
	if moneyProbeLastSegment[last] {
		return true
	}
	switch last {
	case "status":
		// Bare /status, or a .../health/status shape — never /kyc/status-style user data.
		if len(segs) == 1 {
			return true
		}
		return strings.ToLower(segs[len(segs)-2]) == "health"
	case "version", "info":
		// Root, or directly under ONE service prefix (at most 2 segments total): /info, /api/info,
		// /v1/version — not /api/v1/user/info (that is user-scoped, several segments deep).
		return len(segs) <= 2
	}
	return false
}

package envcapture

import "fmt"

// CompareCaptures reports whether two load runs' environments may be treated as the SAME one, and
// — whenever they may not — a plain-words note a human or a report can show as-is. It is the one
// primitive every run-comparing surface (a future trend/regression view, a CLI diff, an MCP tool
// that reads two reports) MUST go through before comparing two load runs' pass/fail, so that a
// pass-vs-fail difference is never read as "the code changed" when it might just be "the
// environment changed" (the operator's principle this whole feature exists for).
//
// same=false whenever the comparison itself is not trustworthy — including when EITHER side never
// captured its environment at all. A missing capture is NEVER treated as "assume it matched": that
// would silently manufacture agreement out of an absence of evidence, the exact failure mode
// Capture.Reason exists to name instead.
func CompareCaptures(a, b Capture) (same bool, note string) {
	switch {
	case !a.Captured && !b.Captured:
		return false, fmt.Sprintf("cannot compare: neither run captured its environment (%s; %s)", reasonOrUnset(a), reasonOrUnset(b))
	case !a.Captured:
		return false, fmt.Sprintf("cannot compare: one run captured no environment (%s)", reasonOrUnset(a))
	case !b.Captured:
		return false, fmt.Sprintf("cannot compare: one run captured no environment (%s)", reasonOrUnset(b))
	case a.Fingerprint == "" || b.Fingerprint == "":
		// Defensive: a Captured=true reading with no fingerprint should not happen (CaptureNamespace
		// always sets one), but treating an empty string as a match would be the exact silent-agreement
		// bug this function exists to prevent, so it is refused by name rather than assumed benign.
		return false, "cannot compare: a captured environment carries no fingerprint (internal inconsistency)"
	case a.Fingerprint != b.Fingerprint:
		return false, "environments differ (fingerprint mismatch): resources, replica counts, node capacity, or image digests are not the same between these runs — a pass/fail difference may reflect the environment, not the code"
	default:
		return true, "environments match"
	}
}

func reasonOrUnset(c Capture) string {
	if c.Reason != "" {
		return c.Reason
	}
	return "no reason recorded"
}

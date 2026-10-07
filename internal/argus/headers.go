package argus

import (
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// resolvedHeaders is VR12-T3's runner half for paths 2 and 3: the author's declared TRIGGER
// headers with ${VAR} / ${cid} / ${correlation_id} resolved, ready to hand to an mcp.Client.
//
// ⛔ AN UNRESOLVED ${…} MUST NEVER REACH THE WIRE (T3.d). resolveVars leaves an unknown ${VAR}
// as-is, so a literal placeholder would be sent as a header value and come back as a confusing
// SUT-side complaint — the same misattribution the whole round exists to prevent. Dropping such a
// header is not right either (the author asked for it), so it is REPORTED: the value is emitted
// and validate names the variable. See headerUnresolved.
func resolvedHeaders(s *scenario.Scenario, corr string) map[string]string {
	hs := scenario.AuthorHeaders(s)
	if len(hs) == 0 {
		return nil
	}
	out := make(map[string]string, len(hs))
	for _, h := range hs {
		out[h[0]] = resolveVars(h[1], corr)
	}
	return out
}

package obsquery

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// — the correlation id is the ONE caller-supplied value that reaches a log query
// (get_sagas / get_tail_logs, on the CLI, the in-env MCP server and the control-plane relay, for
// every role including the builder). It used to be concatenated raw into a LogQL line filter after
// only "" and a `run-` prefix were refused, so (1) any substring matched (`tr-` = every line of
// every run in the window, certification runs included, whose ids a builder is never given) and
// (2) a `"` or `\` broke out of the literal.
//
// corrIDRe is EXACTLY what argus mints, tr-<run_id>-<scenario_id>-<8 hex>
// (internal/argus newScenarioCorrelationID):
//   - run_id: NewRunID is YYYYMMDDThhmmssSSS (18 chars). Ledgers still carry the two older forms —
//     bare seconds (15) and seconds + 6 random hex (21) — so the tail is 0..9 hex/digit chars.
//   - scenario_id: scenario/validate.go idFormat, `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` — it may itself
//     contain '-', which is why the hash is anchored at the END, not split on the first dashes.
//   - hash: 4 random bytes, lower-case hex.
//
// The whole string must match (no unanchored substring, no suffix): a prefix is the widening bug.
var corrIDRe = regexp.MustCompile(`^tr-[0-9]{8}T[0-9]{6}[0-9a-f]{0,9}-[A-Za-z0-9][A-Za-z0-9_-]{0,63}-[0-9a-f]{8}$`)

// ValidateCorrelationID refuses anything that is not a minted correlation id. The message names the
// expected shape so a caller can fix it.
func ValidateCorrelationID(cid string) error {
	if !corrIDRe.MatchString(cid) {
		return fmt.Errorf("correlation_id %q is not a correlation id — it must be the full tr-<run_id>-<scenario_id>-<8 hex> id from get_report, not a prefix or fragment", clipEcho(cid))
	}
	return nil
}

// clipEcho bounds a caller-supplied value echoed back in an error, and keeps control characters out of
// it. (Not openshell.go's clip, which only caps the length of sandbox text.)
func clipEcho(s string) string {
	const max = 80
	s = strconv.QuoteToASCII(s)
	s = s[1 : len(s)-1]
	if len(s) > max {
		s = s[:max] + "…"
	}
	return strings.ReplaceAll(s, `\"`, `'`)
}

// logqlString renders s as a LogQL double-quoted string literal (quotes included). LogQL strings use
// Go escapes, so strconv.Quote is the right encoder: `"`, `\` and control characters cannot end the
// literal early. Defence in depth — ValidateCorrelationID already refuses every such value.
func logqlString(s string) string { return strconv.Quote(s) }

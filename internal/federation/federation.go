// Package federation defines the CP↔executor federation link: HTTPS long-poll (register · long-poll ·
// results-push), owner-locked D-FED.1 / ADR-1 — outbound-only, bounded, idempotent, watchdog'd. It is
// the explicit, versioned context-to-context contract that authorizes the async-by-default deviation
// (plan §5.4 item 2; cat-310 transport-replaceable boundary doctrine).
//
// THIS IS THE I.0 SKELETON layout placeholder. The wire schemas land FINAL from day one at I.1 (plan
// §5.2 I.1): the register/poll/results-push message shapes, the D-FED.2 distinguished rejection
// reasons, and the D-FED.4 version fields — no later protocol churn once written.
package federation

// ProtocolVersion is the federation wire version, exchanged both directions on every poll (D-FED.4
// version exchange → outdated-blocked). Bumped only on a breaking change to the register/poll/push
// schemas.
const ProtocolVersion = "m3.fed.v1"

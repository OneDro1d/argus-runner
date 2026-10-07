package mcp

import (
	"crypto/rand"
	"encoding/hex"
)

// PerCallRequestID is the `_meta.request_id` for ONE tools/call: the correlation id, a dot, and 8
// random hex characters.
//
// ⛔ A request id is per REQUEST. Until 2026-09-24 every step of a chain sent the chain's one
// correlation id as its request id (it doubled as the join key), and a throttle re-fire sent the
// same id again. A SUT that enforces uniqueness — social MCP does, and is right to — refused every
// step after the first with "request_id reused". Reported by the social MCP session.
//
// The correlation id is kept as the PREFIX, so the join still works: the Loki lookup is a substring
// line filter (`|= "<corr>"`, obsquery.queryLines), and a SUT that logs request_id carries the
// correlation id inside it. What no longer works is an EXACT `request_id = <corr>` — nothing shipped
// relies on that; key a lookup on the prefix instead.
func PerCallRequestID(correlationID string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return correlationID + "." + hex.EncodeToString(b[:])
}

package scenario

import (
	"regexp"
	"strconv"
	"strings"
)

// AC-D16 — A MESSAGE FLOW SCENARIO CAN DECLARE A BROKER REFUSAL.
//
// Some publishes must be REFUSED by the broker itself — a message whose `user_id` property does
// not match the connection's authenticated user (406 PRECONDITION_FAILED), or one the connection's
// user has no permission to send (403 ACCESS_REFUSED). A `- broker refuses with <code>` bullet, on
// `### Runnable`, declares the EXACT AMQP 0-9-1 reply code the scenario must observe: PASS is that
// refusal and nothing else — an accepted publish, a timeout, or a refusal with a different code all
// FAIL (enforced by the sampler, jmeter-plugins/amqp's AmqpPublishSampler).
//
// RefusalCodeNames is the closed set of names accepted alongside a bare numeric code — the AMQP
// 0-9-1 reply codes a client-side publish can provoke:
//   - 403 ACCESS_REFUSED      — the user may not perform this operation
//   - 404 NOT_FOUND           — the addressed exchange/queue does not exist
//   - 405 RESOURCE_LOCKED     — the resource is exclusively locked by another connection
//   - 406 PRECONDITION_FAILED — a property/argument does not satisfy a broker precondition
//     (e.g. `user_id` set to something other than the connection's authenticated user)
var RefusalCodeNames = map[string]int{
	"ACCESS_REFUSED":      403,
	"NOT_FOUND":           404,
	"RESOURCE_LOCKED":     405,
	"PRECONDITION_FAILED": 406,
}

// refusalRe — anchored the same way dbNegativeRe is (VR12-E9): the clause must BE the assertion,
// not merely mention refusal in passing. <code> is a bare 3-digit AMQP reply code (100-599) or one
// of RefusalCodeNames' names, case-insensitive.
var refusalRe = regexp.MustCompile(`(?i)^broker\s+refuses(?:\s+the\s+publish)?\s+with\s+([A-Za-z_]+|[1-5][0-9]{2})\s*$`)

// ParseRefusal reads the declared AMQP refusal code from EXPECT bullets. Callers pass
// s.RunnableExpect() (VR12-E7/E9: RUNNABLE bullets only — a sentence filed under
// `### Non-runnable` is documentation, not a check). ok is false when no bullet declares one.
func ParseRefusal(expect []string) (code int, ok bool) {
	for _, b := range expect {
		for _, raw := range dbClauseSplit.Split(b, -1) {
			cl := strings.TrimSpace(raw)
			m := refusalRe.FindStringSubmatch(cl)
			if m == nil {
				continue
			}
			tok := strings.ToUpper(strings.TrimSpace(m[1]))
			if n, err := strconv.Atoi(tok); err == nil {
				return n, true
			}
			if n, known := RefusalCodeNames[tok]; known {
				return n, true
			}
		}
	}
	return 0, false
}

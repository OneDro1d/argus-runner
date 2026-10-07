package scenario

import (
	"encoding/json"
	"strings"
)

// ⛔ DISPATCH — THE ONE PLACE THAT ANSWERS "WHICH ENGINE WILL RUN THIS SCENARIO?"
//
// V29-016's ruling turns on a single sentence the owner wrote: *"Keying validation on anything other
// than what the runtime keys on is how this class of defect is created."*
//
// The runtime keys on the TAG (internal/argus/argus.go:493-505, first match wins):
//
//	chain → runChainScenario · mcp → runMCPScenario · ui → runUIScenario · otherwise → JMeter
//
// …and then, INSIDE the JMeter path, on the primary LAYER: a content layer (or the saga-presence
// tag) is judged by the .jtl success flag, everything else by the response code (argus.go:551-556).
// Only that last set — the response-code judges — is where `expect.status` decides anything.
//
// The predicates live HERE, in internal/scenario, because tier 2 (Validate) has to ask exactly the
// same question tier 1 and the runtime ask, and internal/scenario cannot import internal/argus
// (argus imports scenario). argus aliases these rather than keeping a second copy: two copies of a
// dispatch rule is how a validator ends up refusing scenarios the runner would have run natively —
// the 83-scenario false-refusal the owner caught in this row's own measurement.

const (
	// ChainTag marks a multi-step scenario sequenced in Go (specs/17-chain-scenarios.md).
	ChainTag = "chain"
	// MCPTag marks a runner-native MCP-call scenario judged in Go, never by JMeter.
	MCPTag = "mcp"
	// UITag marks a runner-driven Playwright scenario; its verdict is the process exit code.
	UITag = "ui"
	// SagaPresenceTag marks a scenario judged by an in-JMeter Loki assertion, not by status.
	SagaPresenceTag = "saga-presence"
)

// contentLayer lists the layers whose JMX embeds its own assertion and whose verdict is therefore
// the JMeter `success` flag — NOT the response code. Mirrors argus.contentLayers.
var contentLayer = map[string]bool{
	"Database State":    true,
	"Message Flow":      true,
	"External Delivery": true,
	// the ramp's own verdict (internal/amqpload), never a response code. This is also
	// half of the old-executor proof (design §3.4): an executor that does not list the layer judges it
	// by response code, finds no `status=` bullet, and refuses -- and this build refuses such a bullet.
	AMQPLoadLayer: true,
	// the HTTP ramp is judged by its own verdict (internal/httpload) too, never by one
	// response code, and an older executor refuses it the same way.
	HTTPLoadLayer: true,
}

// statusLayer lists the layers the ORIGINAL specs define as "make an HTTP call and verify its
// response" (specs/06-scenarios-orderservice.md:19-25). ⚠ It is NOT the key of the V29-016 rule —
// the tag is, temporarily, until V28-018 makes the layer label trustworthy. It is recorded here so
// that re-keying is a one-line change in one file with the set already written down.
var statusLayer = map[string]bool{
	"HTTP Ingestion": true,
	"Error Path":     true,
	"Rate Limiting":  true,
	"Permissions":    true,
}

// IsStatusLayer reports whether a layer label is one the specs define as response-verifying.
func IsStatusLayer(layer string) bool { return statusLayer[layer] }

// IsContentLayer reports whether a layer's verdict comes from its JMX's own assertion.
func IsContentLayer(layer string) bool { return contentLayer[layer] }

// PrimaryLayer returns the layer that drives execution and template selection: the TERMINAL layer of
// a chained declaration, which is the one the runner keys on (toolcore.go:156, :542 say the same).
func PrimaryLayer(s *Scenario) string {
	if s == nil || len(s.Layers) == 0 {
		return ""
	}
	return s.Layers[len(s.Layers)-1]
}

// NativeEngineTag returns the dispatch tag that will take this scenario AWAY from JMeter, or "" when
// it reaches JMeter. First match wins, in the runner's own order.
func NativeEngineTag(s *Scenario) string {
	if s == nil {
		return ""
	}
	for _, t := range []string{ChainTag, MCPTag, UITag} {
		if contains(s.Tags, t) {
			return t
		}
	}
	return ""
}

// JudgedByResponseCode reports whether this scenario's verdict will be decided by comparing HTTP
// response codes to the declared `status=` — i.e. whether it lands on argus.judge().
//
// It is TRUE only when the scenario reaches JMeter (no native engine tag), is not saga-presence, and
// its primary layer is not a content layer. That is the exact set in which an undeclared status used
// to be fabricated as 202, and therefore the exact set the V29-016 rule may refuse.
func JudgedByResponseCode(s *Scenario) bool {
	if s == nil {
		return false
	}
	if NativeEngineTag(s) != "" || contains(s.Tags, SagaPresenceTag) {
		return false
	}
	return !contentLayer[PrimaryLayer(s)]
}

// UINamesItsOwnSpec reports whether a `ui` scenario names the spec file its assertions live in —
// either a `spec` field in the TRIGGER payload JSON or a TRIGGER url, the same two places
// internal/argus reads it from (ui_scenario.go:108-113).
//
// ⭐ EXPORTED, and that is the point (V31-002). The same question is asked by the AUTHOR path
// (W2: may this file be saved?) and by the RUN path (R10: is there anything to compare?). One
// definition, called from both, so the two can never drift into a file that saves and then errors —
// or worse, one that is refused but would have run.
//
// It is a predicate over a PARSED FILE, never a validator rule: the run needs the answer, and DEC-1
// says the run holds no copy of the validator's rules. It answers false when the payload is absent
// or is not JSON, and it accepts a URL wrapped in backticks.
func UINamesItsOwnSpec(s *Scenario) bool {
	if s == nil {
		return false
	}
	var p struct {
		Spec string `json:"spec"`
	}
	if json.Unmarshal([]byte(s.Trigger.Payload), &p) == nil && strings.TrimSpace(p.Spec) != "" {
		return true
	}
	return strings.TrimSpace(strings.Trim(s.Trigger.URL, "`")) != ""
}

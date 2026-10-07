package argus

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// fillCorrelationID replaces ${cid} and ${correlation_id} — two names for the scenario's correlation id — and NOTHING else, plus
// ${cid8} (AC-D20), its short derived form. It is the only substitution a CHECK ever receives (V31-003): an environment
// ${VAR} is never filled into a check, because a check's value is printed in the report (assertions_enforced, failure.expected).
func fillCorrelationID(s, corr string) string {
	for _, p := range scenario.CorrelationIDPlaceholders {
		s = strings.ReplaceAll(s, p, corr)
	}
	return strings.ReplaceAll(s, scenario.Cid8Placeholder, cid8(corr))
}

// cid8 derives an 8-LOWERCASE-HEX-CHARACTER token from the correlation id (AC-D20) — short enough
// for a resource name a SUT bounds tightly (a Coder workspace name is at most 32 chars, `[a-z0-9-]`;
// the full `tr-<run_id>-<scenario_id>-<hex>` id routinely runs past 40). A correlation id is not
// itself hex throughout (the run id and scenario id segments are not), so this hashes it rather than
// truncating it — sha256 rather than a weaker hash because the token doubles as the resource's
// COLLISION GUARD (two scenarios racing the same run must not name the same workspace).
// Deterministic: the same correlation id always yields the same token, in the same run AND on a
// later run against the same fixture, so a create step and a cleanup step never need to save it.
func cid8(corr string) string {
	sum := sha256.Sum256([]byte(corr))
	return hex.EncodeToString(sum[:4]) // 4 bytes = 8 hex chars, always [0-9a-f]
}

// fillCorrelationIDInChecks returns a COPY of want with the correlation id filled into every body check's value. A copy, because
// a chain can be re-run and must never carry one run's id into the next (VR10-S2-11, the rule bindExpect follows).
func fillCorrelationIDInChecks(want mcp.Expect, corr string) mcp.Expect {
	if len(want.Body) == 0 {
		return want
	}
	out := want
	out.Body = resolveBodyAssertsCorrelationID(want.Body, corr)
	return out
}

// resolveBodyAssertsCorrelationID is fillCorrelationIDInChecks' body-only core, pulled out so every chain
// step type gets IDENTICAL treatment, not three copies of the same rule. mcp.Expect wraps this for the
// mcp step (fillCorrelationIDInChecks, above — the single-call scenario path and the chain mcp step both
// use it); the chain http step's bodyWant and the chain amqp step's consume Body call it directly
// (chain_scenario.go) — all three are `[]mcp.BodyAssert` under the hood (scenario.BodyAssert is a type
// alias, bodyassert.go:51), so one function is correct for all three, including a matching-op value
// getting the SAME regexp.QuoteMeta treatment (checkValueWithCorrelationID) everywhere.
//
// ⛔ Fix for the cid-in-chain-claims defect: before this, only the mcp step's parsed `want` went through
// fillCorrelationIDInChecks (chain_scenario.go, mcp case) — an http body/status claim and an amqp
// consume body claim carried ${cid}/${cid8}/${correlation_id} LITERAL into the assertion the runner
// enforced (and into the report's assertions_enforced / Failure.Expected), so a check like `content
// contains "argus-lab NLB-001 ${cid8}"` never matched a real message even when the SUT behaved
// correctly. Resolved post-parse (not on the raw `## EXPECT` bullet text, before chainStepExpect /
// httpStepExpect / scenario.AMQPStepClaims run) so the op-aware quoting above still applies: quoting
// needs to know a check is a `matching` (regex) assertion, which is only known once the claim has been
// classified into a BodyAssert — resolving any earlier would either skip the quoting (a `matching` claim
// whose corr contains `.`/`(`/`|` would then compile as regex metacharacters, not literal text) or
// require re-deriving the op from raw text, a second, divergent classifier.
func resolveBodyAssertsCorrelationID(asserts []mcp.BodyAssert, corr string) []mcp.BodyAssert {
	if len(asserts) == 0 {
		return asserts
	}
	out := make([]mcp.BodyAssert, len(asserts))
	copy(out, asserts)
	for i := range out {
		out[i].Value = checkValueWithCorrelationID(out[i].Op, out[i].Value, corr)
	}
	return out
}

// checkValueWithCorrelationID fills the correlation id into ONE check value. In a `matching` pattern the id is
// QUOTED, so it matches only itself: the run path never applies the id rule, so an id may carry `.`, `(` or `|`.
func checkValueWithCorrelationID(op, value, corr string) string {
	if op == mcp.BodyMatchesOp {
		return fillCorrelationID(value, regexp.QuoteMeta(corr))
	}
	return fillCorrelationID(value, corr)
}

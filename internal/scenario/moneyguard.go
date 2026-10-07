package scenario

import (
	"fmt"
	"regexp"
	"strings"
)

// moneyguard.go — T5.4 (MVP2-SPRINT.md), the money-path guard.
//
// "If the SUT is declared money-handling, generation emits GET-only scenarios against
// probe/health/metrics paths and refuses order/quote/swap/rebalance/claim/config-write paths.
// Enforced in code, not by convention." Until this file the rule lived only in a runbook paragraph
// (docs/runbooks/CONTINUOUS-TESTING.md "If your system moves money, every scenario is read-only")
// that an agent writing scenarios was free not to read.
//
// MoneyGuardViolations is PURE and knows nothing about config: the caller decides whether the SUT
// is money-handling (config.Config.MoneyHandling) and calls it at EVERY door — CP-side authoring
// (control.moneyGuardRefusal, cloudtools.go), validation (config.Validate, which `argus
// validate-config` and onboarding run), and execution (argus.runOneScenario, the one point every
// engine passes through). A file can reach a run without passing through the validator, so the run
// refuses too — the same two-door rule the AMQP step and the SQL VERIFY guard already follow.
//
// THE RULE — a money-handling SUT may only be READ, and only over plain HTTP:
//
//   - the request is GET. An absent method is NOT GET: the JMeter path defaults it to POST
//     (argus.DeriveProps), so a scenario that says nothing would write.
//   - no engine whose effect cannot be read off the file: MCP tool calls, AMQP (publish and declare
//     write; consume removes a message another consumer was owed), and UI drives. Chains are allowed
//     only when EVERY step is a GET http step.
//
// ── EXCEPT an EXACT (method, path) the SUT's own `money_writes.allow` names (money_writes, the
// 2026-09-26 follow-up, moneywrites.go) ── Argus is a monitoring kit and must be able to exercise
// REAL writes — including real trades — when the SUT's TESTER has declared exactly which ones and
// bounded them: an allow entry exempts ONLY that method+path from the GET-only and money-verb-path
// rules below, nothing else (MCP/UI/AMQP/CLEANUP stay refused; a non-matching method or path stays
// refused). A `spends: true` entry is enforced FURTHER at execution and, when literal, at validation
// too (scenario.EvaluateSpendAmount, scenario.MoneySpendLedger) — this function alone decides only
// whether the request may be ATTEMPTED, never whether its amount may be SENT.
//   - no CLEANUP that runs anything: a cleanup exists to write. Only the declared N/A exemption.
//   - no path that names a money verb, EVEN ON A GET: order, quote, swap, rebalance, claim. A GET can
//     still act — a quote endpoint prices against live liquidity, and some APIs create on GET.
//     Config and withdrawal paths are deliberately NOT in this list: a GET of either is a read (the
//     live Shop kit checks `GET /api/v1/trading/config` and `/user/withdrawal-pin`), and every
//     WRITE to them is already refused by GET-only.
//
// Each violation is one sentence naming what was found and why it is refused, like VerifySQLErrors.

// moneyVerbPath matches a path SEGMENT (or a -/_ separated word inside one) naming a money verb.
// Word-bounded on path punctuation so "/api/v1/ordering-docs" style near-misses are still caught
// only when the verb stands as its own word, and "border" or "unquoted" never match.
var moneyVerbPath = regexp.MustCompile(`(?i)(^|[/_.\-=?&])(orders?|quotes?|swaps?|rebalanc(e|es|ing)|claims?)([/_.\-=?&]|$)`)

// MoneyVerbInPath reports the money verb a URL's path/query names, or "".
func MoneyVerbInPath(url string) string {
	m := moneyVerbPath.FindStringSubmatch(url)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[2])
}

// MoneyGuardViolations returns why s may not run against a money-handling SUT; empty = allowed.
// allow is the SUT's `money_writes.allow` block (moneywrites.go); nil/empty behaves exactly as
// before this parameter existed — nothing is exempted.
func MoneyGuardViolations(s *Scenario, allow MoneyWriteAllowlist) []string {
	var out []string
	switch {
	case contains(s.Tags, MCPTag):
		out = append(out, "it is an MCP scenario — a tool call's effect cannot be read off the file, so "+
			"against a money-handling system every scenario must be a plain HTTP GET")
	case contains(s.Tags, UITag):
		out = append(out, "it is a UI scenario — a browser drive can submit anything the page offers, so "+
			"against a money-handling system every scenario must be a plain HTTP GET")
	case contains(s.Tags, ChainTag):
		steps, err := ParseChainSteps(s.Trigger.Payload)
		if err != nil {
			out = append(out, "its chain steps could not be read ("+err.Error()+"), and an unreadable "+
				"chain cannot be shown to be read-only")
			break
		}
		for i, st := range steps {
			name := st.Name
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			switch st.Type {
			case "http":
				out = append(out, httpMoneyViolations("chain step "+name, st.Method, st.URL, allow)...)
			default:
				out = append(out, fmt.Sprintf("chain step %s is a %q step — against a money-handling system "+
					"only GET http steps may run", name, st.Type))
			}
		}
	default:
		out = append(out, httpMoneyViolations("the TRIGGER", s.Trigger.Method, s.Trigger.URL, allow)...)
		// ⛔ A SPENDING plain TRIGGER MUST FIRE EXACTLY ONCE. A plain TRIGGER is sent by JMeter, not by
		// Go, and the per-run ledger (MoneySpendLedger) reserves ONE spend per scenario before handing
		// it over. A `## LOAD` profile makes JMeter send it users × loops times for its whole duration,
		// and a declared `status2=` selects the 2-sampler template, which sends it twice — either way
		// real spends the ledger never counted, multiplying past max_per_run. Refused rather than
		// counted: this door cannot know how many requests JMeter will actually send.
		if e := allow.Match(s.Trigger.Method, s.Trigger.URL); e != nil && e.Spends {
			if s.Load != nil {
				out = append(out, fmt.Sprintf("the TRIGGER is a spends: true money_writes request (%s %s) with a ## LOAD "+
					"profile — load sends it many times and each is a real spend the per-run limit cannot count; "+
					"a spending request must fire exactly once", e.Method, e.Path))
			}
			if _, second := DeclaredStatuses(s.RunnableExpect()); second > 0 {
				out = append(out, fmt.Sprintf("the TRIGGER is a spends: true money_writes request (%s %s) that declares "+
					"status2 — the 2-sampler template sends it twice; a spending request must fire exactly once",
					e.Method, e.Path))
			}
		}
	}
	if !s.Cleanup.IsNA() {
		for _, b := range s.Cleanup.Blocks {
			out = append(out, fmt.Sprintf("its ## CLEANUP runs a %s block — a cleanup exists to write, and a "+
				"money-handling system may only be read. Declare `N/A` instead", b.Form))
		}
	}
	return out
}

func httpMoneyViolations(where, method, url string, allow MoneyWriteAllowlist) []string {
	// money_writes: an EXACT (method, path) allow entry exempts this one request from BOTH rules
	// below — the GET-only rule and the money-verb-path rule — and nothing else. A near-miss
	// (wrong method, wrong path, a path Match refuses to treat as the same one) falls straight
	// through to the same refusals this function has always produced.
	if allow.Match(method, url) != nil {
		return nil
	}
	var out []string
	m := strings.ToUpper(strings.TrimSpace(method))
	if m != "GET" {
		shown := m
		if shown == "" {
			shown = "no method (which runs as POST)"
		}
		out = append(out, fmt.Sprintf("%s is %s — against a money-handling system only GET may run", where, shown))
	}
	if v := MoneyVerbInPath(url); v != "" {
		out = append(out, fmt.Sprintf("%s addresses a %q path (%s) — order, quote, swap, rebalance and claim "+
			"paths are refused on a money-handling system even as a GET", where, v, url))
	}
	return out
}

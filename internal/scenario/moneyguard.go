package scenario

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// moneyguard.go — T5.4 (MVP2-SPRINT.md), the money-path guard; made the OWNER'S switch by
// .
//
// Argus is system-agnostic: it has no opinion about what a system's checks may do, and it does not
// care whether a system moves money. `money_handling: true` is a switch the system's OWNER sets in the
// owner's own config, and it means exactly one thing — "Argus refuses what my `money_writes.allow`
// list does not name". The list is the owner's decision (moneywrites.go, moneyengines.go).
//
// MoneyGuardViolations is PURE and knows nothing about config: the caller decides whether the SUT
// is money-handling (config.Config.MoneyHandling) and calls it at EVERY door — CP-side authoring
// (control.moneyGuardRefusal, cloudtools.go), validation (config.Validate, which `argus
// validate-config` and onboarding run), and execution (argus.runOneScenario, the one point every
// engine passes through). A file can reach a run without passing through the validator, so the run
// refuses too — the same two-door rule the AMQP step and the SQL VERIFY guard already follow.
//
// WHAT THE SWITCH REFUSES, until an entry names it:
//
//   - an HTTP request that is not a GET. An absent method is NOT GET: the JMeter path defaults it to
//     POST (argus.DeriveProps), so a scenario that says nothing would write.
//   - an HTTP request whose path names a money verb, EVEN ON A GET: order, quote, swap, rebalance,
//     claim. A GET can still act — a quote endpoint prices against live liquidity, and some APIs
//     create on GET. Config and withdrawal paths are deliberately NOT in this list: a GET of either is
//     a read (a kit may check `GET /api/v1/config` or `/user/withdrawal-settings`), and
//     every WRITE to them is already refused by GET-only.
//   - every engine whose effect cannot be read off the file: an MCP tool call, an AMQP op (publish and
//     declare write; consume removes a message another consumer was owed), a database step, a logs
//     step. A chain is judged step by step, so one unlisted step is refused and its listed neighbours
//     are not. A UI drive (scenario or chain step) is judged by its spec, a fault step by one `fault`
//     entry; a step type no entry kind exists for stays refused.
//   - a CLEANUP that runs anything: a cleanup exists to write. The declared N/A exemption, or one
//     `cleanup` entry per form (sql | bash) the scenario's blocks use.
//
// ── WHAT AN ENTRY ALLOWS ── exactly what it names, nothing else. An HTTP entry (method + path, the path
// optionally holding whole-segment `{name}` placeholders) exempts that one route from the first two
// rules; an `amqp` entry names one op and its names, an `mcp` entry one target and tool, a `db` entry
// one database target, a `logs` entry the logs step. An entry never widens to a near miss: another
// queue, another routing key, another op, another tool, another path is refused as if unlisted.
// A `spends: true` HTTP entry is enforced FURTHER at execution and, when literal, at validation too
// (scenario.EvaluateSpendAmount, scenario.MoneySpendLedger) — this function alone decides only whether
// the request may be ATTEMPTED, never whether its amount may be SENT. An amqp or mcp entry is
// all-or-nothing: Argus cannot cap a bus publish or a tool call the way `max_amount` caps an HTTP body.
//
// ── THE RESOLVED REQUEST IS WHAT IS ALLOWED ── this function reads the FILE. A chain http step's url may
// still hold `${saved.<var>}`, bound at run time from what the system answered, so the door that sends
// the request judges it AGAIN on its resolved method and path (ResolvedRequestViolations below), with the
// same rule and the same list.
//
// Each violation is one sentence naming what was found and why it is refused, like VerifySQLErrors. The
// sentences of the base rule are the PREFIX of the ones here: a clause may be appended that tells the
// owner which entry would allow the thing, never inserted before them.

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
	_, out := MoneyGuardJudgement(s, allow)
	return out
}

// MoneyGuardJudgement is MoneyGuardViolations that also says WHICH runner the scenario was judged as.
//
// ⛔ THE GUARD JUDGES THE SCENARIO AS THE RUNNER IT WILL GET. The kind is RunnerKindOf —
// the one function the run dispatch switches on too — never a tag order of this function's own. It used to
// read the tags mcp, ui, chain and ignore the layer, so `ui, chain` was judged as a ui entry and RUN as a
// chain (every POST step sent), and a load layer was judged as its tag while the load runner fired the
// TRIGGER many times.
func MoneyGuardJudgement(s *Scenario, allow MoneyWriteAllowlist) (RunnerKind, []string) {
	var out []string
	kind := RunnerKindOf(s)
	switch kind {
	case RunnerAMQPLoad:
		// What the AMQP load runner fires is a ramp of sessions that declare, publish and consume on the
		// broker for the whole step duration (amqpload_run.go): ops no single `amqp` entry names and a
		// message count no entry can cap. The existing rules cannot judge that from the file, so it is
		// refused by name rather than by a rule invented here.
		out = append(out, "it is an AMQP Load scenario — its ramp declares, publishes and consumes on a broker "+
			"queue at a rate for the whole step, which no money_writes.allow entry can name or cap, so against "+
			"a money-handling system it is refused")
	case RunnerHTTPLoad:
		// The HTTP load runner fires the scenario's TRIGGER users × duration times (httpload_run.go). Each
		// request is judged by the same rules as a plain TRIGGER, and a spending entry is refused: load sends
		// it many times and each is a real spend the per-run limit cannot count.
		out = append(out, httpMoneyViolations("the TRIGGER", s.Trigger.Method, s.Trigger.URL, allow)...)
		if e := allow.Match(s.Trigger.Method, s.Trigger.URL); e != nil && e.Spends {
			out = append(out, fmt.Sprintf("the TRIGGER is a spends: true money_writes request (%s %s) in an HTTP Load ramp — "+
				"load sends it many times and each is a real spend the per-run limit cannot count; "+
				"a spending request must fire exactly once", e.Method, e.Path))
		}
	case RunnerMCP:
		if ok, hint := allow.mcpScenarioAllowed(s); !ok {
			out = append(out, "it is an MCP scenario — a tool call's effect cannot be read off the file, so "+
				"against a money-handling system every scenario must be a plain HTTP GET"+hint)
		}
	case RunnerUI:
		if ok, hint := allow.uiScenarioAllowed(s); !ok {
			out = append(out, "it is a UI scenario — a browser drive can submit anything the page offers, so "+
				"against a money-handling system every scenario must be a plain HTTP GET"+hint)
		}
	case RunnerChain:
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
				// Every other step type asks the list: an entry of its kind may allow it. A type the list has
				// no kind for (anything unknown) is refused as it always was.
				if ok, hint := allow.chainStepAllowedInChain(st, steps); !ok {
					out = append(out, fmt.Sprintf("chain step %s is a %q step — against a money-handling system "+
						"only GET http steps may run", name, st.Type)+hint)
				}
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
			if b.Form != CleanupNA && allow.MatchCleanup(string(b.Form)) != nil {
				continue
			}
			hint := ""
			if b.Form == CleanupSQL || b.Form == CleanupBash {
				hint = cleanupHint(string(b.Form))
			}
			out = append(out, fmt.Sprintf("its ## CLEANUP runs a %s block — a cleanup exists to write, and a "+
				"money-handling system may only be read. Declare `N/A` instead", b.Form)+hint)
		}
	}
	return kind, out
}

func httpMoneyViolations(where, method, url string, allow MoneyWriteAllowlist) []string {
	// money_writes: an allow entry for this (method, path) exempts this one request from BOTH rules
	// below — the GET-only rule and the money-verb-path rule — and nothing else. A near-miss
	// (wrong method, wrong path, a path Match refuses to treat as the same one) falls straight
	// through to the same refusals this function has always produced.
	if allow.Match(method, url) != nil {
		return nil
	}
	return moneyRuleViolations(where, method, url, url)
}

// moneyRuleViolations is THE RULE for one HTTP request, with no list in it: the request is a GET, and
// its path names no money verb. It is the one body behind httpMoneyViolations (the file, judged at
// config validate, the control plane and the run door) and ResolvedRequestViolations (the request about
// to be sent), so "the resolved request satisfies the SAME rule the file did" is one function, not two
// that agree. check is the text the rule reads; shown is the text a sentence prints (they differ when
// check is a resolved url whose saved values must not be echoed).
func moneyRuleViolations(where, method, shown, check string) []string {
	var out []string
	m := strings.ToUpper(strings.TrimSpace(method))
	if m != "GET" {
		what := m
		if what == "" {
			what = "no method (which runs as POST)"
		}
		out = append(out, fmt.Sprintf("%s is %s — against a money-handling system only GET may run", where, what))
	}
	if v := MoneyVerbInPath(check); v != "" {
		out = append(out, fmt.Sprintf("%s addresses a %q path (%s) — order, quote, swap, rebalance and claim "+
			"paths are refused on a money-handling system even as a GET", where, v, shown))
	}
	return out
}

// ResolvedRequestViolations judges the request that is about to be SENT — method and url after every
// `${saved.<var>}` is bound — with the SAME rule and the SAME list the file was judged with
// (httpMoneyViolations). empty = it may be sent.
//
// WHY IT EXISTS. The file is judged before a value the SUT answered with is known. A
// chain step `GET /api/v1/items/${saved.id}` names no money word, so the file passes; if `id` was saved
// as `orders` or `../orders/1` the request that goes out addresses an order path. And once a path may
// hold a `{name}` placeholder, the same gap lets a saved `1/cancel` carry a request from the route an
// entry allowed to another one. So the door that sends calls this with u = the request's own URL.
//
// HOW IT TELLS "NO ENTRY NEEDED" FROM "THE ENTRY THAT ALLOWED THIS NO LONGER MATCHES". It does not
// try. It asks the list about the resolved request (MatchResolved); if an entry covers it, it is
// allowed, exactly as in the file. If none does, the request must pass the rule with no list at all —
// a GET of a path naming no money verb — which is what a request needing no entry passes and what a
// request whose entry stopped matching does not. The rule is read on the escaped path and on the
// decoded one (a `%6frders` is `orders` to the server), with the query, and never on the host.
//
// shown is the text the sentences print: pass the url AS WRITTEN (the template), not u, because a
// resolved url carries saved values and a saved value may be a credential.
func ResolvedRequestViolations(where, method, shown string, u *url.URL, allow MoneyWriteAllowlist) []string {
	if u == nil {
		return []string{where + " has no readable url — refused (fail-closed)"}
	}
	if allow.MatchResolved(method, u) != nil {
		return nil
	}
	escaped, decoded := u.EscapedPath(), u.Path
	if u.RawQuery != "" {
		escaped += "?" + u.RawQuery
		q, err := url.QueryUnescape(u.RawQuery)
		if err != nil {
			q = u.RawQuery
		}
		decoded += "?" + q
	}
	out := moneyRuleViolations(where, method, shown, escaped)
	if len(out) == 0 && decoded != escaped {
		out = moneyRuleViolations(where, method, shown, decoded)
	}
	if len(out) > 0 {
		out = append(out, where+" was judged on the url it resolved to at run time, which no money_writes.allow entry covers")
	}
	return out
}

package scenario

import (
	"strings"
	"testing"
)

// moneywrites_test.go — "money writes" (T5.4 follow-up, 2026-09-26): the pure allowlist matcher, the
// resolved-body spend check, the per-run ledger, and the validation-time literal-amount check.

func orderEntry() MoneyWriteAllow {
	return MoneyWriteAllow{Method: "POST", Path: "/api/v1/trading/order", Spends: true,
		AmountField: "source_amount", MaxAmount: 25, MaxPerRun: 4}
}

func quoteEntry() MoneyWriteAllow {
	return MoneyWriteAllow{Method: "POST", Path: "/api/v1/trading/quote", Spends: false}
}

// ── the pure matcher (spec item 5) ──────────────────────────────────────────────────────────────

func TestMoneyWriteAllowlist_ExactMatch(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry(), quoteEntry()}
	e := al.Match("POST", "https://sut.example/api/v1/trading/order")
	if e == nil || e.Path != "/api/v1/trading/order" {
		t.Fatalf("exact match failed: %+v", e)
	}
}

func TestMoneyWriteAllowlist_WrongMethodDoesNotMatch(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	if e := al.Match("PUT", "https://sut.example/api/v1/trading/order"); e != nil {
		t.Fatalf("PUT matched an entry declared for POST: %+v", e)
	}
}

func TestMoneyWriteAllowlist_MethodIsCaseInsensitive(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	if e := al.Match("post", "https://sut.example/api/v1/trading/order"); e == nil {
		t.Fatal("lowercase \"post\" did not match a POST entry — method should be case-insensitive")
	}
}

// ⛔ THE MUTATION GUARD (spec item 5, mutation check c): path matching must be EXACT. If Match were
// implemented with strings.HasPrefix instead of equality, EVERY case below would incorrectly match
// and this test would go GREEN when it should be RED — i.e. these are exactly the cases a HasPrefix
// bug would let through.
func TestMoneyWriteAllowlist_ExactMatchOnly_NearMissesRefused(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	for _, tc := range []struct {
		name, url string
	}{
		{"prefix (extra path segment after)", "https://sut.example/api/v1/trading/order/123"},
		{"suffix (extra characters before, same tail)", "https://sut.example/api/v1/trading/xorder"},
		{"suffix (declared path is a suffix of a longer one)", "https://sut.example/api/v1/trading/orders"},
		{"trailing slash", "https://sut.example/api/v1/trading/order/"},
		{"wrong case", "https://sut.example/API/v1/trading/ORDER"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := al.Match("POST", tc.url); e != nil {
				t.Fatalf("%s matched (%q) — path matching must be EXACT, not HasPrefix/HasSuffix/case-insensitive", tc.name, tc.url)
			}
		})
	}
}

// A query string on the REQUEST is not a near-miss: item 1/2 say matching is on the URL's path
// only, so a request that hits the declared path with extra query params must still match.
func TestMoneyWriteAllowlist_QueryStringOnRequestIsStripped(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	e := al.Match("POST", "https://sut.example/api/v1/trading/order?idempotency_key=abc")
	if e == nil {
		t.Fatal("a request URL carrying a query string did not match its bare declared path — the query must be stripped before comparing")
	}
}

func TestMoneyWriteAllowlist_ConfigVarPlaceholderStripped(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	e := al.Match("POST", "${INGESTION_URL}/api/v1/trading/order")
	if e == nil {
		t.Fatal("a leading ${VAR} base-URL placeholder was not stripped before matching")
	}
}

func TestMoneyWriteAllowlist_EmptyAllowlistNeverMatches(t *testing.T) {
	var al MoneyWriteAllowlist
	if e := al.Match("POST", "https://sut.example/api/v1/trading/order"); e != nil {
		t.Fatalf("a nil allowlist matched: %+v", e)
	}
}

// ── the exemption inside MoneyGuardViolations ───────────────────────────────────────────────────

func TestMoneyGuardViolations_AllowedWriteIsExempt(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	if v := MoneyGuardViolations(s, al); len(v) != 0 {
		t.Fatalf("an allowlisted POST was refused: %v", v)
	}
}

func TestMoneyGuardViolations_UnlistedWriteStillRefused(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()} // only /order is listed
	s := httpScenario("POST", "https://sut.example/api/v1/trading/rebalance", naCleanup)
	v := MoneyGuardViolations(s, al)
	if len(v) == 0 {
		t.Fatal("an unlisted POST to a money-verb path was allowed")
	}
}

func TestMoneyGuardViolations_MCPStillRefusedWithAllowlistPresent(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := &Scenario{Tags: []string{MCPTag}, Cleanup: naCleanup}
	v := MoneyGuardViolations(s, al)
	if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "MCP scenario") {
		t.Fatalf("an MCP scenario was not refused even with an allowlist present: %v", v)
	}
}

func TestMoneyGuardViolations_ChainGETPlusAllowedPOSTAllowed(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup, Trigger: Trigger{Payload: `{"steps":[` +
		`{"type":"http","name":"a","method":"GET","url":"https://sut.example/health"},` +
		`{"type":"http","name":"b","method":"POST","url":"https://sut.example/api/v1/trading/order"}]}`}}
	if v := MoneyGuardViolations(s, al); len(v) != 0 {
		t.Fatalf("a chain mixing a GET and an allowlisted POST was refused: %v", v)
	}
}

func TestMoneyGuardViolations_ChainWithAnUnlistedPOSTRefused(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()} // /order only — NOT /rebalance
	s := &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup, Trigger: Trigger{Payload: `{"steps":[` +
		`{"type":"http","name":"a","method":"GET","url":"https://sut.example/health"},` +
		`{"type":"http","name":"b","method":"POST","url":"https://sut.example/api/v1/trading/rebalance"}]}`}}
	v := MoneyGuardViolations(s, al)
	if len(v) == 0 {
		t.Fatal("a chain step POSTing to an UNLISTED path was allowed")
	}
}

// ── EvaluateSpendAmount (item 3) ─────────────────────────────────────────────────────────────────

func TestEvaluateSpendAmount_UnderCapPasses(t *testing.T) {
	e := orderEntry()
	if msg := EvaluateSpendAmount(&e, `{"source_amount": 10}`); msg != "" {
		t.Fatalf("an under-cap amount was refused: %q", msg)
	}
}

func TestEvaluateSpendAmount_AtCapPasses(t *testing.T) {
	e := orderEntry()
	if msg := EvaluateSpendAmount(&e, `{"source_amount": 25}`); msg != "" {
		t.Fatalf("an amount exactly at max_amount was refused: %q", msg)
	}
}

func TestEvaluateSpendAmount_OverCapRefused(t *testing.T) {
	e := orderEntry()
	msg := EvaluateSpendAmount(&e, `{"source_amount": 25.01}`)
	if msg == "" || !strings.Contains(msg, "25.01") || !strings.Contains(msg, "25") {
		t.Fatalf("an over-cap amount was not refused by name: %q", msg)
	}
}

func TestEvaluateSpendAmount_MissingFieldRefused(t *testing.T) {
	e := orderEntry()
	msg := EvaluateSpendAmount(&e, `{"other_field": 5}`)
	if msg == "" {
		t.Fatal("a missing amount_field was not refused (fail-closed)")
	}
}

func TestEvaluateSpendAmount_NonNumericFieldRefused(t *testing.T) {
	e := orderEntry()
	for _, body := range []string{
		`{"source_amount": "10"}`, // a numeric-LOOKING string is still not a JSON number
		`{"source_amount": true}`,
		`{"source_amount": null}`,
		`{"source_amount": [1,2]}`,
		`not json at all`,
	} {
		if msg := EvaluateSpendAmount(&e, body); msg == "" {
			t.Errorf("body %q: non-numeric amount was not refused (fail-closed)", body)
		}
	}
}

func TestEvaluateSpendAmount_DottedPath(t *testing.T) {
	e := orderEntry()
	e.AmountField = "order.amount"
	if msg := EvaluateSpendAmount(&e, `{"order":{"amount":5}}`); msg != "" {
		t.Fatalf("a dotted-path amount_field was not read: %q", msg)
	}
	if msg := EvaluateSpendAmount(&e, `{"order":{"amount":500}}`); msg == "" {
		t.Fatal("a dotted-path amount over cap was not refused")
	}
}

func TestEvaluateSpendAmount_SpendsFalseEntryAlwaysPasses(t *testing.T) {
	e := quoteEntry() // spends: false
	if msg := EvaluateSpendAmount(&e, `not even json`); msg != "" {
		t.Fatalf("a spends:false entry was checked for an amount at all: %q", msg)
	}
}

func TestEvaluateSpendAmount_NilEntryPasses(t *testing.T) {
	if msg := EvaluateSpendAmount(nil, `{}`); msg != "" {
		t.Fatalf("a nil entry produced a violation: %q", msg)
	}
}

// ── MoneySpendLedger (item 3, "across all scenarios") ───────────────────────────────────────────

func TestMoneySpendLedger_EnforcesMaxPerRun(t *testing.T) {
	e := orderEntry() // max_per_run: 4
	l := &MoneySpendLedger{}
	for i := 0; i < 4; i++ {
		ok, n := l.Reserve(&e)
		if !ok {
			t.Fatalf("reservation %d refused before the cap was reached", i+1)
		}
		if n != i+1 {
			t.Errorf("reservation %d reported count %d", i+1, n)
		}
	}
	if ok, _ := l.Reserve(&e); ok {
		t.Fatal("a 5th reservation against max_per_run=4 was allowed")
	}
}

// The scenario in the test name is item 3's own acceptance case: two DIFFERENT scenarios sharing
// ONE ledger must share the SAME budget.
func TestMoneySpendLedger_SharedAcrossTwoScenarios(t *testing.T) {
	e := orderEntry() // max_per_run: 4
	l := &MoneySpendLedger{}
	for i := 0; i < 3; i++ { // "scenario A" spends 3
		if ok, _ := l.Reserve(&e); !ok {
			t.Fatalf("scenario A's reservation %d was refused early", i+1)
		}
	}
	if ok, n := l.Reserve(&e); !ok || n != 4 { // "scenario B" spends the 4th
		t.Fatalf("scenario B's reservation (the 4th overall) = ok=%v n=%d, want ok=true n=4", ok, n)
	}
	if ok, _ := l.Reserve(&e); ok { // "scenario B" tries a 5th overall
		t.Fatal("scenario B was allowed a 5th spend against a budget scenario A had already used 3 of")
	}
}

func TestMoneySpendLedger_DifferentEntriesHaveSeparateBudgets(t *testing.T) {
	order := orderEntry()
	other := orderEntry()
	other.Path = "/api/v1/trading/order2"
	l := &MoneySpendLedger{}
	for i := 0; i < 4; i++ {
		l.Reserve(&order)
	}
	if ok, _ := l.Reserve(&order); ok {
		t.Fatal("order's budget was not exhausted")
	}
	if ok, _ := l.Reserve(&other); !ok {
		t.Fatal("a DIFFERENT entry's budget was refused because of order's exhausted one")
	}
}

func TestMoneySpendLedger_NilLedgerAlwaysReserves(t *testing.T) {
	var l *MoneySpendLedger
	e := orderEntry()
	for i := 0; i < 10; i++ {
		if ok, _ := l.Reserve(&e); !ok {
			t.Fatal("a nil ledger refused a reservation")
		}
	}
}

// ── MoneyWriteAmountViolations (item 3's "at VALIDATION" literal early-check) ───────────────────

func TestMoneyWriteAmountViolations_LiteralOverCapRefused(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	s.Trigger.Payload = `{"source_amount": 999}`
	v := MoneyWriteAmountViolations(s, al)
	if len(v) == 0 {
		t.Fatal("a literal amount over max_amount was not caught at validation time")
	}
}

func TestMoneyWriteAmountViolations_LiteralUnderCapPasses(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	s.Trigger.Payload = `{"source_amount": 5}`
	if v := MoneyWriteAmountViolations(s, al); len(v) != 0 {
		t.Fatalf("a literal amount under max_amount was refused: %v", v)
	}
}

// A body carrying ANY "${…}" placeholder is left to execution — validation must not guess.
func TestMoneyWriteAmountViolations_TemplatedBodyLeftToExecution(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	s.Trigger.Payload = `{"source_amount": ${AMOUNT}}`
	if v := MoneyWriteAmountViolations(s, al); len(v) != 0 {
		t.Fatalf("a templated body was checked at validation time (should be deferred to execution): %v", v)
	}
}

func TestMoneyWriteAmountViolations_ChainStepLiteralOverCapRefused(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup, Trigger: Trigger{Payload: `{"steps":[` +
		`{"type":"http","name":"spend","method":"POST","url":"https://sut.example/api/v1/trading/order","body":{"source_amount":999}}]}`}}
	v := MoneyWriteAmountViolations(s, al)
	if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "spend") {
		t.Fatalf("a chain step's literal over-cap amount was not caught, or not named: %v", v)
	}
}

func TestMoneyWriteAmountViolations_EmptyAllowlistNoop(t *testing.T) {
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	s.Trigger.Payload = `{"source_amount": 999999}`
	if v := MoneyWriteAmountViolations(s, nil); len(v) != 0 {
		t.Fatalf("an empty allowlist produced a violation: %v", v)
	}
}

// ── a spending plain TRIGGER fires exactly once (review of #262, 2026-09-26) ─────────────────────
//
// A plain TRIGGER is sent by JMeter, and the per-run ledger reserves ONE spend per scenario. A
// ## LOAD profile (users × loops for a duration) or a declared status2 (the 2-sampler template)
// would make JMeter send the same real spend several times, uncounted. Both are refused.

func TestMoneyGuardViolations_SpendingTriggerWithLoadRefused(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	s.Load = &LoadProfile{Users: 5}
	v := MoneyGuardViolations(s, al)
	if len(v) != 1 || !strings.Contains(v[0], "## LOAD") {
		t.Fatalf("a spending TRIGGER with a LOAD profile was not refused (exactly once, naming LOAD): %v", v)
	}
}

func TestMoneyGuardViolations_SpendingTriggerWithStatus2Refused(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	s.ExpectRunnable = []string{"status=202", "status2=409"}
	v := MoneyGuardViolations(s, al)
	if len(v) != 1 || !strings.Contains(v[0], "status2") {
		t.Fatalf("a spending TRIGGER declaring status2 was not refused (exactly once, naming status2): %v", v)
	}
}

// Controls: the rule is about SPENDING requests that fire more than once — nothing wider.
func TestMoneyGuardViolations_SpendingTriggerFiredOnceAllowed(t *testing.T) {
	al := MoneyWriteAllowlist{orderEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/order", naCleanup)
	s.ExpectRunnable = []string{"status=202"}
	if v := MoneyGuardViolations(s, al); len(v) != 0 {
		t.Fatalf("a spending TRIGGER that fires once was refused: %v", v)
	}
}

func TestMoneyGuardViolations_NonSpendingTriggerWithLoadAllowed(t *testing.T) {
	al := MoneyWriteAllowlist{quoteEntry()}
	s := httpScenario("POST", "https://sut.example/api/v1/trading/quote", naCleanup)
	s.Load = &LoadProfile{Users: 5}
	if v := MoneyGuardViolations(s, al); len(v) != 0 {
		t.Fatalf("a spends: false TRIGGER under LOAD was refused — the once-only rule is for spending requests only: %v", v)
	}
}

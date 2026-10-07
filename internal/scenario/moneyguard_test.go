package scenario

import (
	"strings"
	"testing"
)

// moneyguard_test.go — T5.4, the money-path guard as a pure rule. The two doors that call it are
// pinned in internal/config (Validate) and internal/argus (runOneScenario).

func httpScenario(method, url string, cleanup Cleanup) *Scenario {
	return &Scenario{ID: "S", Trigger: Trigger{Method: method, URL: url}, Cleanup: cleanup}
}

var naCleanup = Cleanup{Blocks: []CleanupBlock{{Form: CleanupNA}}}

func TestMoneyGuard_AllowsWhatTheLiveMoneyKitsRun(t *testing.T) {
	// Every one of these is a TRIGGER the live shop-dev and shop-uni-arb kits run hourly today
	// (two recorded runs, read
	// 2026-09-24). Turning the guard on must not refuse a single one.
	for _, u := range []string{
		"https://shop-dev.example/api/v1/strategies",
		"https://shop-dev.example/kyc/health",
		"https://shop-dev.example/api/v1/user/withdrawal-pin",
		"https://shop-dev.example/api/v1/trading/config?strategy_pair_id=not-a-uuid",
		"https://shop-dev.example/api/v1/trading/config",
		"https://shop-dev.example/ready",
		"https://shop-dev.example/internal/custody/address-health",
		"https://shop-dev.example/api/v1/user/transactions",
		"https://shop-dev.example/api/admin/holdings",
		"https://shop-dev.example/api/v1/portfolio-token-contracts",
		"https://shop-dev.example/api/v1/user/balances",
		"http://arb-engine.shop-uni-arb.svc.cluster.local:9090/metrics",
		"http://control-plane.shop-uni-arb.svc.cluster.local:80/readyz",
	} {
		if v := MoneyGuardViolations(httpScenario("GET", u, naCleanup), nil); len(v) != 0 {
			t.Errorf("GET %s was refused: %v — a read the live kit relies on", u, v)
		}
	}
}

func TestMoneyGuard_RefusesAnythingButARead(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *Scenario
		want string
	}{
		{"POST", httpScenario("POST", "https://x.example/api/v1/strategies", naCleanup), "is POST"},
		{"PUT", httpScenario("PUT", "https://x.example/api/v1/trading/config", naCleanup), "is PUT"},
		{"DELETE", httpScenario("DELETE", "https://x.example/api/v1/user", naCleanup), "is DELETE"},
		// The JMeter path turns an absent method into POST (argus.DeriveProps) — silence is a write.
		{"no method", httpScenario("", "https://x.example/health", naCleanup), "runs as POST"},
		{"GET on orders", httpScenario("GET", "https://x.example/api/v1/orders", naCleanup), `"orders" path`},
		{"GET on a quote", httpScenario("GET", "https://x.example/api/v1/quote?pair=OPS", naCleanup), `"quote" path`},
		{"GET on swap-intent", httpScenario("GET", "https://x.example/swap-intent/1", naCleanup), `"swap" path`},
		{"GET on rebalance", httpScenario("GET", "https://x.example/admin/rebalance", naCleanup), `"rebalance" path`},
		{"GET on claims", httpScenario("GET", "https://x.example/rewards/claims", naCleanup), `"claims" path`},
		{"SQL cleanup", httpScenario("GET", "https://x.example/health", Cleanup{Blocks: []CleanupBlock{{Form: CleanupSQL}}}), "CLEANUP runs a sql block"},
		{"bash cleanup", httpScenario("GET", "https://x.example/health", Cleanup{Blocks: []CleanupBlock{{Form: CleanupBash}}}), "CLEANUP runs a bash block"},
		{"MCP", &Scenario{Tags: []string{MCPTag}, Cleanup: naCleanup}, "MCP scenario"},
		{"UI", &Scenario{Tags: []string{UITag}, Cleanup: naCleanup}, "UI scenario"},
		{"chain with an mcp step", &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup,
			Trigger: Trigger{Payload: `{"steps":[{"type":"http","name":"a","method":"GET","url":"https://x/health"},{"type":"mcp","name":"b","tool":"t"}]}`}},
			`chain step b is a "mcp" step`},
		{"chain with a POST", &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup,
			Trigger: Trigger{Payload: `{"steps":[{"type":"http","name":"w","method":"POST","url":"https://x/api"}]}`}},
			"chain step w is POST"},
		{"chain with an amqp consume", &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup,
			Trigger: Trigger{Payload: `{"steps":[{"type":"amqp","name":"c","op":"consume"}]}`}},
			`"amqp" step`},
		// P3 #24c — a delete is a write: queue_delete is refused the SAME way every other amqp op
		// is (by TYPE, not by op) — the guard needs no new case for the cleanup ops.
		{"chain with an amqp queue_delete", &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup,
			Trigger: Trigger{Payload: `{"steps":[{"type":"amqp","name":"cleanup","op":"queue_delete","queue":"q"}]}`}},
			`"amqp" step`},
		{"unreadable chain", &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup, Trigger: Trigger{Payload: `{not json`}},
			"could not be read"},
	} {
		v := MoneyGuardViolations(tc.s, nil)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " | "), tc.want) {
			t.Errorf("%s: violations %v, want one containing %q", tc.name, v, tc.want)
		}
	}
}

func TestMoneyGuard_AllowsAnAllGETChain(t *testing.T) {
	s := &Scenario{Tags: []string{ChainTag}, Cleanup: naCleanup, Trigger: Trigger{Payload: `{"steps":[` +
		`{"type":"http","name":"a","method":"GET","url":"https://x/health"},` +
		`{"type":"http","name":"b","method":"get","url":"https://x/metrics"}]}`}}
	if v := MoneyGuardViolations(s, nil); len(v) != 0 {
		t.Fatalf("an all-GET chain on read paths was refused: %v", v)
	}
}

func TestMoneyVerbInPath_WordBoundaries(t *testing.T) {
	for url, want := range map[string]string{
		"https://x/api/v1/orders":              "orders",
		"https://x/api/v1/order/1":             "order",
		"https://x/quote":                      "quote",
		"https://x/api?op=swap":                "swap",
		"https://x/api/rebalancing-status":     "rebalancing",
		"https://x/border/health":              "", // "border" is not "order"
		"https://x/api/unquoted-names":         "", // "unquoted" is not "quote"
		"https://x/api/v1/trading/config":      "",
		"https://x/api/v1/user/withdrawal-pin": "",
	} {
		if got := MoneyVerbInPath(url); got != want {
			t.Errorf("MoneyVerbInPath(%q) = %q, want %q", url, got, want)
		}
	}
}

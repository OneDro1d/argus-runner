package config

import (
	"strings"
	"testing"
)

// moneyallow_test.go — load-time validation of every new field of a money_writes.allow entry
//. Every refusal is named, with the entry index, and the strict-key node still refuses
// a mistyped key.

const allowHead = "project:\n  name: p\nmoney_handling: true\n"
const allowTargets = "targets:\n  http:\n    base_url: http://sut.invalid\n" +
	"  mcp_targets:\n    trading:\n      base_url: http://mcp.invalid/mcp\n" +
	"  database_targets:\n    ledger:\n      type: postgres\n      jdbc_url: jdbc:postgresql://db.invalid:5432/ledger\n"

func loadAllow(t *testing.T, allowYAML string) (*Config, error) {
	t.Helper()
	return Load(writeMoneyWritesConfig(t, allowHead+"money_writes:\n  allow:\n"+allowYAML+allowTargets))
}

func TestMoneyAllow_EveryEntryKindLoadsAndConverts(t *testing.T) {
	c, err := loadAllow(t, ""+
		"    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n"+
		"    - method: GET\n      path: /api/v1/orders/{id}\n      spends: false\n"+
		"    - method: POST\n      path: /api/v1/orders/{id}/fill\n      spends: true\n      amount_field: amount\n      max_amount: 5\n      max_per_run: 2\n"+
		"    - engine: http\n      method: GET\n      path: /api/v1/quotes\n      spends: false\n"+
		"    - engine: amqp\n      op: consume\n      queue: reply.inbox\n"+
		"    - engine: amqp\n      op: publish\n      exchange: \"\"\n      routing_key: q.direct\n"+
		"    - engine: amqp\n      op: queue_unbind\n      queue: q\n      exchange: x\n      routing_key: k\n"+
		"    - engine: mcp\n      target: trading\n      tool: get_positions\n"+
		"    - engine: mcp\n      tool: ping\n"+
		"    - engine: db\n      target: ledger\n"+
		"    - engine: db\n"+
		"    - engine: logs\n")
	if err != nil {
		t.Fatalf("a valid block with every entry kind was refused: %v", err)
	}
	al := c.MoneyWriteAllowlist()
	if len(al) != 12 {
		t.Fatalf("converted %d entries, want 12", len(al))
	}
	// an explicit `engine: http` converts to the engine-less entry: it serialises exactly as before
	if al[3].Engine != "" || al[3].Method != "GET" || al[3].Path != "/api/v1/quotes" {
		t.Errorf("engine: http must convert to a plain HTTP entry: %+v", al[3])
	}
	if al[5].Op != "publish" || al[5].Exchange != "" || al[5].RoutingKey != "q.direct" || al[5].Engine != "amqp" {
		t.Errorf("default-exchange publish: %+v", al[5])
	}
	if al[7].Engine != "mcp" || al[7].Target != "trading" || al[7].Tool != "get_positions" || al[7].Method != "" || al[7].Path != "" {
		t.Errorf("mcp entry: %+v", al[7])
	}
	if al[11].Engine != "logs" {
		t.Errorf("logs entry: %+v", al[11])
	}
}

func TestMoneyAllow_LoadTimeRefusalsNamedWithTheEntryIndex(t *testing.T) {
	ok0 := "    - method: POST\n      path: /api/v1/trading/quote\n      spends: false\n"
	for _, tc := range []struct{ name, entry, want string }{
		{"unknown engine", "    - engine: kafka\n      op: consume\n",
			`money_writes.allow[1]: engine "kafka" is not one of http, amqp, mcp, db, logs`},
		{"unknown op", "    - engine: amqp\n      op: nack\n      queue: q\n",
			`money_writes.allow[1]: op "nack" is not an amqp op`},
		{"amqp with no op", "    - engine: amqp\n      queue: q\n",
			`money_writes.allow[1]: an amqp entry requires op`},
		{"consume missing its queue", "    - engine: amqp\n      op: consume\n",
			`money_writes.allow[1]: op: consume requires queue`},
		{"publish missing its routing key", "    - engine: amqp\n      op: publish\n      exchange: x\n",
			`money_writes.allow[1]: op: publish requires routing_key`},
		{"unbind missing exchange", "    - engine: amqp\n      op: queue_unbind\n      queue: q\n      routing_key: k\n",
			`money_writes.allow[1]: op: queue_unbind requires exchange`},
		{"surplus name field for the op", "    - engine: amqp\n      op: consume\n      queue: q\n      exchange: x\n",
			`money_writes.allow[1]: op: consume does not use exchange`},
		{"empty queue", "    - engine: amqp\n      op: consume\n      queue: \"\"\n",
			`money_writes.allow[1]: op: consume requires a non-empty queue`},
		{"empty routing key", "    - engine: amqp\n      op: publish\n      exchange: x\n      routing_key: \"\"\n",
			`money_writes.allow[1]: op: publish requires a non-empty routing_key`},
		{"a tool on an amqp entry", "    - engine: amqp\n      op: consume\n      queue: q\n      tool: t\n",
			`money_writes.allow[1]: op: consume does not use tool`},
		{"mcp with no tool", "    - engine: mcp\n      target: trading\n",
			`money_writes.allow[1]: an mcp entry requires tool`},
		{"mcp undeclared target", "    - engine: mcp\n      target: nowhere\n      tool: t\n",
			`money_writes.allow[1]: target "nowhere" is not declared under targets.mcp_targets`},
		{"db undeclared target", "    - engine: db\n      target: nowhere\n",
			`money_writes.allow[1]: target "nowhere" is not declared under targets.database_targets`},
		{"queue on an mcp entry", "    - engine: mcp\n      tool: t\n      queue: q\n",
			`money_writes.allow[1]: queue is not a field of an engine: mcp entry`},
		{"target on a logs entry", "    - engine: logs\n      target: ledger\n",
			`money_writes.allow[1]: target is not a field of an engine: logs entry`},
		{"method on a non-http entry", "    - engine: mcp\n      tool: t\n      method: POST\n",
			`money_writes.allow[1]: method is a field of an HTTP entry, and this entry is engine: mcp`},
		{"path on a non-http entry", "    - engine: logs\n      path: /x\n",
			`money_writes.allow[1]: path is a field of an HTTP entry, and this entry is engine: logs`},
		{"spends on a non-http entry", "    - engine: amqp\n      op: consume\n      queue: q\n      spends: false\n",
			`money_writes.allow[1]: spends, amount_field, max_amount and max_per_run apply to HTTP entries only`},
		{"a limit on a non-http entry", "    - engine: mcp\n      tool: t\n      max_amount: 5\n",
			`money_writes.allow[1]: spends, amount_field, max_amount and max_per_run apply to HTTP entries only`},
		{"engine fields on an http entry", "    - method: POST\n      path: /x\n      spends: false\n      queue: q\n",
			`money_writes.allow[1]: queue is a field of an engine entry (amqp, mcp, db, logs, cleanup, ui, fault), and this is an HTTP entry`},
		{"engine fields on an explicit http entry", "    - engine: http\n      method: GET\n      path: /x\n      spends: false\n      tool: t\n",
			`money_writes.allow[1]: tool is a field of an engine entry`},
		{"a name built at run time", "    - engine: amqp\n      op: consume\n      queue: \"q-${cid}\"\n",
			`money_writes.allow[1]: "q-${cid}": a name built at run time (${…}) cannot be listed`},
		{"a tool built at run time", "    - engine: mcp\n      tool: \"${T}\"\n",
			`money_writes.allow[1]: "${T}": a name built at run time (${…}) cannot be listed`},
		{"GET entry that spends", "    - method: GET\n      path: /api/v1/orders/{id}\n      spends: true\n      amount_field: a\n      max_amount: 1\n      max_per_run: 1\n",
			`money_writes.allow[1]: a GET entry must be spends: false`},
		{"partial-segment placeholder", "    - method: GET\n      path: /orders/x{id}\n      spends: false\n",
			`money_writes.allow[1]: path "/orders/x{id}" segment "x{id}": a placeholder must be a whole segment`},
		{"placeholder with a suffix", "    - method: GET\n      path: /orders/{id}x\n      spends: false\n",
			`segment "{id}x": a placeholder must be a whole segment`},
		{"placeholder with a bad name", "    - method: GET\n      path: /orders/{1id}\n      spends: false\n",
			`segment "{1id}": a placeholder must be a whole segment`},
		{"empty placeholder", "    - method: GET\n      path: /orders/{}\n      spends: false\n",
			`segment "{}": a placeholder must be a whole segment`},
		{"a wildcard is still refused", "    - method: GET\n      path: /orders/*\n      spends: false\n",
			`money_writes.allow[1]: path "/orders/*" must be an exact path`},
		{"a query is still refused", "    - method: GET\n      path: /orders?x=1\n      spends: false\n",
			`must be an exact path`},
		{"a run-time http path", "    - method: GET\n      path: /orders/${saved.id}\n      spends: false\n",
			`a path built at run time (${…}) cannot be listed`},
		{"duplicate http entry", ok0,
			`money_writes.allow[1]: duplicates money_writes.allow[0]`},
		{"duplicate placeholder shape", "    - method: GET\n      path: /a/{x}\n      spends: false\n    - method: GET\n      path: /a/{y}\n      spends: false\n",
			`money_writes.allow[2]: duplicates money_writes.allow[1]`},
		{"duplicate amqp entry", "    - engine: amqp\n      op: consume\n      queue: q\n    - engine: amqp\n      op: consume\n      queue: q\n",
			`money_writes.allow[2]: duplicates money_writes.allow[1]`},
		{"duplicate logs entry", "    - engine: logs\n    - engine: logs\n",
			`money_writes.allow[2]: duplicates money_writes.allow[1]`},
	} {
		_, err := loadAllow(t, ok0+tc.entry)
		if err == nil {
			t.Errorf("%s: the block was accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s:\n  got  %v\n  want a refusal containing %q", tc.name, err, tc.want)
		}
	}
}

func TestMoneyAllow_ADuplicateDoesNotRefuseDifferentMethodsOrPaths(t *testing.T) {
	if _, err := loadAllow(t, ""+
		"    - method: GET\n      path: /a/{x}\n      spends: false\n"+
		"    - method: POST\n      path: /a/{x}\n      spends: false\n"+
		"    - method: GET\n      path: /a/{x}/b\n      spends: false\n"+
		"    - engine: amqp\n      op: consume\n      queue: q1\n"+
		"    - engine: amqp\n      op: consume\n      queue: q2\n"); err != nil {
		t.Fatalf("distinct entries were refused as duplicates: %v", err)
	}
}

// The strict-key node must accept the new keys and still refuse a mistyped one.
func TestMoneyAllow_StrictNodeAcceptsNewKeysAndRefusesAMistypedOne(t *testing.T) {
	if _, err := loadAllow(t, "    - engine: mcp\n      target: trading\n      tool: get_positions\n"); err != nil {
		t.Fatalf("the strict node refused a new key: %v", err)
	}
	for _, key := range []string{"engin", "routing_keys", "tools", "targt", "exchnage", "qeue"} {
		_, err := loadAllow(t, "    - engine: logs\n      "+key+": x\n")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("a mistyped key %q was not refused by name: %v", key, err)
		}
	}
}

func TestMoneyAllow_ABlockWithoutMoneyHandlingStillRefusesEveryKind(t *testing.T) {
	p := writeMoneyWritesConfig(t, "project:\n  name: p\n"+
		"money_writes:\n  allow:\n    - engine: logs\n"+allowTargets)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "money_handling") {
		t.Fatalf("an engine entry without money_handling: true was accepted: %v", err)
	}
}

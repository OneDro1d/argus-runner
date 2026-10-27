package config

import "testing"

// The listen and queue_bind entry kinds load, convert, and are refused by name when malformed.

func TestMoneyAllow_ListenAndQueueBindEntriesLoadAndConvert(t *testing.T) {
	c, err := loadAllow(t, ""+
		"    - engine: amqp\n      op: listen\n      exchange: orders.events\n      routing_key: order.created\n"+
		"    - engine: amqp\n      op: queue_bind\n      queue: mine\n      exchange: orders.events\n      routing_key: order.created\n")
	if err != nil {
		t.Fatalf("listen / queue_bind entries were refused: %v", err)
	}
	al := c.MoneyWriteAllowlist()
	if len(al) != 2 || al[0].Op != "listen" || al[0].Exchange != "orders.events" || al[0].RoutingKey != "order.created" || al[0].Queue != "" ||
		al[1].Op != "queue_bind" || al[1].Queue != "mine" {
		t.Errorf("converted = %+v", al)
	}
}

// `engine: amqp, op: listen, stream: <name>` loads, converts (the stream reaches the wire type), and
// every malformed shape is refused by name.
func TestMoneyAllow_StreamListenEntryLoadsAndConverts(t *testing.T) {
	c, err := loadAllow(t, "    - engine: amqp\n      op: listen\n      stream: logs\n")
	if err != nil {
		t.Fatalf("a stream listen entry was refused: %v", err)
	}
	al := c.MoneyWriteAllowlist()
	if len(al) != 1 || al[0].Op != "listen" || al[0].Stream != "logs" || al[0].Exchange != "" || al[0].RoutingKey != "" {
		t.Errorf("converted = %+v", al)
	}
}

func TestMoneyAllow_StreamListenMalformedEntriesAreRefusedByName(t *testing.T) {
	for _, tc := range []struct{ name, entry, want string }{
		{"stream with an exchange", "    - engine: amqp\n      op: listen\n      stream: s\n      exchange: x\n",
			`money_writes.allow[1]: stream together with exchange or routing_key is ambiguous`},
		{"stream with a routing key", "    - engine: amqp\n      op: listen\n      stream: s\n      routing_key: k\n",
			`money_writes.allow[1]: stream together with exchange or routing_key is ambiguous`},
		{"stream with consume", "    - engine: amqp\n      op: consume\n      stream: s\n      queue: q\n",
			`money_writes.allow[1]: stream is valid only with op: listen`},
		{"stream with publish", "    - engine: amqp\n      op: publish\n      stream: s\n",
			`money_writes.allow[1]: stream is valid only with op: listen`},
		{"an empty stream", "    - engine: amqp\n      op: listen\n      stream: \"\"\n",
			`money_writes.allow[1]: op: listen with stream requires a non-empty stream`},
		{"a stream built at run time", "    - engine: amqp\n      op: listen\n      stream: \"${S}\"\n",
			`a name built at run time (${…}) cannot be listed`},
		{"stream with a queue", "    - engine: amqp\n      op: listen\n      stream: s\n      queue: q\n",
			`money_writes.allow[1]: op: listen does not use queue`},
		{"stream on an mcp entry", "    - engine: mcp\n      tool: t\n      stream: s\n",
			`money_writes.allow[1]: stream is not a field of an engine: mcp entry`},
		{"stream on an http entry", "    - method: GET\n      path: /x\n      spends: false\n      stream: s\n",
			`money_writes.allow[1]: stream is a field of an engine entry`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAllow(t, "    - method: POST\n      path: /api/v1/pay/quote\n      spends: false\n"+tc.entry)
			if err == nil || !contains(err.Error(), tc.want) {
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestMoneyAllow_StreamAndExchangeListenEntriesAreDistinct(t *testing.T) {
	// the same stream twice is a duplicate; a stream entry and an exchange + routing_key entry are two different entries
	if _, err := loadAllow(t, "    - engine: amqp\n      op: listen\n      stream: s\n    - engine: amqp\n      op: listen\n      exchange: x\n      routing_key: k\n"); err != nil {
		t.Errorf("a stream entry and an exchange entry must coexist: %v", err)
	}
	_, err := loadAllow(t, "    - engine: amqp\n      op: listen\n      stream: s\n    - engine: amqp\n      op: listen\n      stream: s\n")
	if err == nil || !contains(err.Error(), "duplicates money_writes.allow[0]") {
		t.Errorf("a repeated stream entry must be refused as a duplicate, got %v", err)
	}
}

func TestMoneyAllow_ListenAndQueueBindMalformedEntriesAreRefusedByName(t *testing.T) {
	for _, tc := range []struct{ name, entry, want string }{
		{"listen with a queue", "    - engine: amqp\n      op: listen\n      queue: q\n      exchange: x\n      routing_key: k\n",
			`money_writes.allow[1]: op: listen does not use queue`},
		{"listen without a routing key", "    - engine: amqp\n      op: listen\n      exchange: x\n",
			`money_writes.allow[1]: op: listen requires routing_key`},
		{"listen without an exchange", "    - engine: amqp\n      op: listen\n      routing_key: k\n",
			`money_writes.allow[1]: op: listen requires exchange`},
		{"queue_bind without a queue", "    - engine: amqp\n      op: queue_bind\n      exchange: x\n      routing_key: k\n",
			`money_writes.allow[1]: op: queue_bind requires queue`},
		{"the listen's queue cannot be listed (a run-time name)", "    - engine: amqp\n      op: consume\n      queue: \"${saved.q}\"\n",
			`a name built at run time (${…}) cannot be listed`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// entry 0 is a valid HTTP entry so the index in the message is 1
			_, err := loadAllow(t, "    - method: POST\n      path: /api/v1/pay/quote\n      spends: false\n"+tc.entry)
			if err == nil || !contains(err.Error(), tc.want) {
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
}

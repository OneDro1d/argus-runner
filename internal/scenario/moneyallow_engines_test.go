package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// moneyallow_engines_test.go — the amqp | mcp | db | logs entry kinds, through the ONE
// function every door calls, MoneyGuardViolations, with hand-written chain payloads.

func chainOf(steps ...string) *Scenario {
	return &Scenario{ID: "C", Tags: []string{ChainTag}, Cleanup: naCleanup,
		Trigger: Trigger{Method: "POST", URL: "chain", Payload: `{"steps":[` + strings.Join(steps, ",") + `]}`}}
}

func amqpStep(name, op, fields string) string {
	return `{"type":"amqp","name":"` + name + `","op":"` + op + `","url_env":"BROKER_URL"` + fields + `}`
}

func TestAMQP_EachOpIsAllowedByItsEntryAndRefusedByANearMiss(t *testing.T) {
	type c struct {
		op     string
		fields string // the step's name fields
		entry  MoneyWriteAllow
		near   []string // step field sets that must NOT match
	}
	for _, tc := range []c{
		{"publish", `,"exchange":"msgbus.inbox","routing_key":"inbox.agent.alpha"`,
			MoneyWriteAllow{Engine: "amqp", Op: "publish", Exchange: "msgbus.inbox", RoutingKey: "inbox.agent.alpha"},
			[]string{`,"exchange":"msgbus.inbox","routing_key":"inbox.agent.other"`,
				`,"exchange":"msgbus.other","routing_key":"inbox.agent.alpha"`,
				`,"exchange":"msgbus.inbox"`}},
		{"consume", `,"queue":"reply.inbox"`,
			MoneyWriteAllow{Engine: "amqp", Op: "consume", Queue: "reply.inbox"},
			[]string{`,"queue":"reply.inbox2"`, `,"queue":"Reply.inbox"`, ``}},
		{"declare_queue", `,"queue":"q.test"`,
			MoneyWriteAllow{Engine: "amqp", Op: "declare_queue", Queue: "q.test"},
			[]string{`,"queue":"q.test "`}},
		{"declare_exchange", `,"exchange":"x.test"`,
			MoneyWriteAllow{Engine: "amqp", Op: "declare_exchange", Exchange: "x.test"},
			[]string{`,"exchange":"x.tes"`}},
		{"queue_delete", `,"queue":"q.test","force":false`,
			MoneyWriteAllow{Engine: "amqp", Op: "queue_delete", Queue: "q.test"},
			[]string{`,"queue":"q.live"`}},
		{"queue_unbind", `,"queue":"q.test","exchange":"x.test","routing_key":"k"`,
			MoneyWriteAllow{Engine: "amqp", Op: "queue_unbind", Queue: "q.test", Exchange: "x.test", RoutingKey: "k"},
			[]string{`,"queue":"q.test","exchange":"x.test","routing_key":"j"`, `,"queue":"q.test","exchange":"x.other","routing_key":"k"`}},
		{"exchange_delete", `,"exchange":"x.test"`,
			MoneyWriteAllow{Engine: "amqp", Op: "exchange_delete", Exchange: "x.test"},
			[]string{`,"exchange":"x.live"`}},
	} {
		al := MoneyWriteAllowlist{tc.entry}
		step := amqpStep("s", tc.op, tc.fields)
		if v := MoneyGuardViolations(chainOf(step), al); len(v) != 0 {
			t.Errorf("%s: the entry did not allow its own step: %v", tc.op, v)
		}
		if v := MoneyGuardViolations(chainOf(step), nil); len(v) != 1 || !strings.HasPrefix(v[0], `chain step s is a "amqp" step — against a money-handling system only GET http steps may run`) {
			t.Errorf("%s: without the entry the base sentence must lead: %v", tc.op, v)
		}
		for _, near := range tc.near {
			if v := MoneyGuardViolations(chainOf(amqpStep("s", tc.op, near)), al); len(v) == 0 {
				t.Errorf("%s: the near miss %s was allowed", tc.op, near)
			}
		}
		// the same names under a different op are not allowed by this entry
		for _, other := range AMQPOps() {
			if other == tc.op {
				continue
			}
			if v := MoneyGuardViolations(chainOf(amqpStep("s", other, tc.fields)), al); len(v) == 0 {
				t.Errorf("%s entry allowed op %s with the same names", tc.op, other)
			}
		}
	}
}

func TestAMQP_AnEntryNeverMatchesANameBuiltAtRunTime(t *testing.T) {
	al := MoneyWriteAllowlist{{Engine: "amqp", Op: "consume", Queue: "reply.inbox"}}
	// even an entry that spells the literal text of the template does not cover it
	templ := MoneyWriteAllowlist{{Engine: "amqp", Op: "consume", Queue: "q-${cid}"}}
	for _, s := range []struct {
		al    MoneyWriteAllowlist
		field string
	}{
		{al, `,"queue":"${saved.q}"`},
		{al, `,"queue":"reply.${cid}"`},
		{al, `,"queue":"${QUEUE_NAME}"`},
		{templ, `,"queue":"q-${cid}"`},
	} {
		v := MoneyGuardViolations(chainOf(amqpStep("s", "consume", s.field)), s.al)
		if len(v) != 1 || !strings.Contains(v[0], "built at run time") {
			t.Errorf("%s: a run-time name must be refused with the sentence saying it cannot be listed: %v", s.field, v)
		}
	}
}

func TestAMQP_ADefaultExchangePublishIsAnEntryLikeAnyOther(t *testing.T) {
	al := MoneyWriteAllowlist{{Engine: "amqp", Op: "publish", Exchange: "", RoutingKey: "q.direct"}}
	if v := MoneyGuardViolations(chainOf(amqpStep("s", "publish", `,"routing_key":"q.direct"`)), al); len(v) != 0 {
		t.Errorf("a publish to the default exchange was refused by its entry: %v", v)
	}
	// ...but an entry with an empty QUEUE covers no consume: only a publish's exchange may be empty
	empty := MoneyWriteAllowlist{{Engine: "amqp", Op: "consume", Queue: ""}}
	if v := MoneyGuardViolations(chainOf(amqpStep("s", "consume", `,"queue":""`)), empty); len(v) == 0 {
		t.Error("an entry with an empty queue allowed a consume of an empty queue name")
	}
}

func TestAMQP_AChainMixingAnAllowedConsumeAndAnUnlistedPublishIsRefusedForThePublishOnly(t *testing.T) {
	al := MoneyWriteAllowlist{{Engine: "amqp", Op: "consume", Queue: "reply.inbox"}}
	s := chainOf(
		amqpStep("read", "consume", `,"queue":"reply.inbox"`),
		amqpStep("send", "publish", `,"exchange":"msgbus.inbox","routing_key":"inbox.agent.alpha"`),
	)
	v := MoneyGuardViolations(s, al)
	if len(v) != 1 || !strings.HasPrefix(v[0], `chain step send is a "amqp" step`) {
		t.Fatalf("want exactly the publish refused, got %v", v)
	}
}

func TestAMQP_AnEntryWithSurplusOrForeignFieldsMatchesNothing(t *testing.T) {
	// fail closed on the wire: a hand-built entry that names a field its op does not use, an http
	// field, or spends, covers nothing
	step := chainOf(amqpStep("s", "consume", `,"queue":"q"`))
	for _, e := range []MoneyWriteAllow{
		{Engine: "amqp", Op: "consume", Queue: "q", Exchange: "x"},
		{Engine: "amqp", Op: "consume", Queue: "q", Tool: "t"},
		{Engine: "amqp", Op: "consume", Queue: "q", Spends: true},
		{Engine: "amqp", Op: "bogus", Queue: "q"},
		{Engine: "future", Op: "consume", Queue: "q"},
	} {
		if v := MoneyGuardViolations(step, MoneyWriteAllowlist{e}); len(v) == 0 {
			t.Errorf("a malformed entry %+v allowed a step", e)
		}
	}
}

func mcpStep(name, fields string) string {
	return `{"type":"mcp","name":"` + name + `"` + fields + `}`
}

func TestMCP_ChainStepByTargetAndTool(t *testing.T) {
	al := MoneyWriteAllowlist{
		{Engine: "mcp", Target: "ledger", Tool: "get_balances"},
		{Engine: "mcp", Tool: "ping"},
	}
	for _, tc := range []struct {
		name, step string
		want       bool
	}{
		{"named target + tool", mcpStep("a", `,"target":"ledger","tool":"get_balances"`), true},
		{"the unnamed target", mcpStep("a", `,"tool":"ping"`), true},
		{"another tool on the named target", mcpStep("a", `,"target":"ledger","tool":"post_entry"`), false},
		{"the tool on another target", mcpStep("a", `,"target":"other","tool":"get_balances"`), false},
		{"the named tool with no target", mcpStep("a", `,"tool":"get_balances"`), false},
		{"the unnamed entry's tool on a named target", mcpStep("a", `,"target":"ledger","tool":"ping"`), false},
		{"case differs", mcpStep("a", `,"target":"ledger","tool":"Get_Balances"`), false},
		{"no tool", mcpStep("a", `,"target":"ledger"`), false},
		{"a tool built at run time", mcpStep("a", `,"target":"ledger","tool":"${saved.t}"`), false},
		{"a target built at run time", mcpStep("a", `,"target":"${T}","tool":"get_balances"`), false},
		// the endpoint rule: a step that sets its own server_url is NEVER matched
		{"server_url set, unnamed entry", mcpStep("a", `,"server_url":"https://elsewhere.example/mcp","tool":"ping"`), false},
		{"server_url set (even unresolved), unnamed entry", mcpStep("a", `,"server_url":"${MCP_URL}","tool":"ping"`), false},
		{"server_url beside a target", mcpStep("a", `,"target":"ledger","server_url":"https://elsewhere.example/mcp","tool":"get_balances"`), false},
	} {
		v := MoneyGuardViolations(chainOf(tc.step), al)
		if (len(v) == 0) != tc.want {
			t.Errorf("%s: allowed = %v, want %v (%v)", tc.name, len(v) == 0, tc.want, v)
		}
		if !tc.want && (len(v) != 1 || !strings.HasPrefix(v[0], `chain step a is a "mcp" step — against a money-handling system only GET http steps may run`)) {
			t.Errorf("%s: the base sentence must lead the refusal: %v", tc.name, v)
		}
	}
	if v := MoneyGuardViolations(chainOf(mcpStep("a", `,"target":"ledger","tool":"get_balances"`)), nil); len(v) != 1 {
		t.Errorf("without a list an mcp step must be refused as before: %v", v)
	}
}

func mcpScenario(target, payload, trigURL string) *Scenario {
	return &Scenario{ID: "M", Tags: []string{MCPTag}, Target: target, Cleanup: naCleanup,
		Trigger: Trigger{Method: "POST", URL: trigURL, Payload: payload}}
}

func TestMCP_TaggedScenarioIsAllowedByItsEntryAndRefusedWithout(t *testing.T) {
	al := MoneyWriteAllowlist{
		{Engine: "mcp", Target: "ledger", Tool: "get_balances"},
		{Engine: "mcp", Tool: "ping"},
	}
	for _, tc := range []struct {
		name string
		s    *Scenario
		want bool
	}{
		{"named target + tool", mcpScenario("ledger", `{"tool":"get_balances","args":{}}`, "${MCP_URL}"), true},
		{"another tool", mcpScenario("ledger", `{"tool":"post_entry","args":{}}`, "${MCP_URL}"), false},
		{"another target", mcpScenario("other", `{"tool":"get_balances"}`, "${MCP_URL}"), false},
		{"unnamed target, no endpoint of its own", mcpScenario("", `{"tool":"ping"}`, ""), true},
		{"unnamed target but a TRIGGER url (the runner uses it)", mcpScenario("", `{"tool":"ping"}`, "`https://elsewhere.example/mcp`"), false},
		{"unnamed target but the in-tree ${MCP_URL} shape", mcpScenario("", `{"tool":"ping"}`, "${MCP_URL}"), false},
		{"a payload server_url", mcpScenario("ledger", `{"tool":"get_balances","server_url":"https://elsewhere.example/mcp"}`, "${MCP_URL}"), false},
		{"an unreadable payload", mcpScenario("ledger", `not json`, "${MCP_URL}"), false},
		{"no tool", mcpScenario("ledger", `{"args":{}}`, "${MCP_URL}"), false},
	} {
		v := MoneyGuardViolations(tc.s, al)
		if (len(v) == 0) != tc.want {
			t.Errorf("%s: allowed = %v, want %v (%v)", tc.name, len(v) == 0, tc.want, v)
		}
		if !tc.want && (len(v) != 1 || !strings.HasPrefix(v[0], "it is an MCP scenario — a tool call's effect cannot be read off the file, so against a money-handling system every scenario must be a plain HTTP GET")) {
			t.Errorf("%s: the base sentence must lead the refusal: %v", tc.name, v)
		}
	}
	if v := MoneyGuardViolations(mcpScenario("ledger", `{"tool":"get_balances"}`, "${MCP_URL}"), nil); len(v) != 1 {
		t.Errorf("without a list an mcp scenario must be refused as before: %v", v)
	}
}

// replaced TestUIScenarioStaysRefusedWhateverTheListHolds: ui and fault now have entry kinds
// (moneyallow_cleanup_ui_fault_test.go pins them). A step type NO kind exists for still stays refused by
// exactly the base sentence, whatever the list holds.
func TestUnknownChainStepTypeStaysRefusedWhateverTheListHolds(t *testing.T) {
	al := MoneyWriteAllowlist{
		{Engine: "mcp", Tool: "x"}, {Engine: "logs"}, {Engine: "db"}, {Engine: "amqp", Op: "consume", Queue: "q"},
		{Engine: "ui", Spec: "tests/live/x.spec.ts"}, {Engine: "fault"}, {Engine: "cleanup", Form: "sql"},
		{Method: "GET", Path: "/", Spends: false},
	}
	step := `{"type":"teleport","name":"x"}`
	v := MoneyGuardViolations(chainOf(step), al)
	want := `chain step x is a "teleport" step — against a money-handling system only GET http steps may run`
	if len(v) != 1 || v[0] != want {
		t.Errorf("an unknown chain step type must be refused with exactly the base sentence (no hint, no entry kind): %v", v)
	}
}

func TestDBAndLogsEntriesWithHandWrittenPayloads(t *testing.T) {
	db := `{"type":"db","name":"d","target":"ledger"}`
	dbDefault := `{"type":"db","name":"d"}`
	logs := `{"type":"logs","name":"l"}`
	for _, tc := range []struct {
		name  string
		step  string
		al    MoneyWriteAllowlist
		allow bool
	}{
		{"db by name", db, MoneyWriteAllowlist{{Engine: "db", Target: "ledger"}}, true},
		{"db other name", db, MoneyWriteAllowlist{{Engine: "db", Target: "other"}}, false},
		{"db named step, default-target entry", db, MoneyWriteAllowlist{{Engine: "db"}}, false},
		{"db default step, default entry", dbDefault, MoneyWriteAllowlist{{Engine: "db"}}, true},
		{"db default step, named entry", dbDefault, MoneyWriteAllowlist{{Engine: "db", Target: "ledger"}}, false},
		{"db built at run time", `{"type":"db","name":"d","target":"${T}"}`, MoneyWriteAllowlist{{Engine: "db", Target: "${T}"}}, false},
		{"db with no list", db, nil, false},
		{"logs", logs, MoneyWriteAllowlist{{Engine: "logs"}}, true},
		{"logs with only a db entry", logs, MoneyWriteAllowlist{{Engine: "db"}}, false},
		{"logs with no list", logs, nil, false},
		{"db with only a logs entry", db, MoneyWriteAllowlist{{Engine: "logs"}}, false},
		{"logs entry carrying a field", logs, MoneyWriteAllowlist{{Engine: "logs", Target: "x"}}, false},
	} {
		v := MoneyGuardViolations(chainOf(tc.step), tc.al)
		if (len(v) == 0) != tc.allow {
			t.Errorf("%s: allowed = %v, want %v (%v)", tc.name, len(v) == 0, tc.allow, v)
		}
		if !tc.allow && (len(v) != 1 || !strings.HasPrefix(v[0], "chain step ")) {
			t.Errorf("%s: want exactly one chain-step sentence: %v", tc.name, v)
		}
	}
}

func TestEntryKindsDoNotExemptHTTP(t *testing.T) {
	// an entry of another kind, however it is spelled on the wire, exempts no HTTP request
	var al MoneyWriteAllowlist
	raw := `[{"engine":"amqp","op":"consume","queue":"q","method":"","path":""},
	         {"engine":"mcp","tool":"t","method":"POST","path":"/api/v1/order"},
	         {"engine":"future","method":"POST","path":"/api/v1/order","spends":false},
	         {"method":"POST","path":"/api/v1/order","queue":"q"},
	         {"method":"","path":"/api/v1/order"},
	         {"method":"POST","path":""}]`
	if err := json.Unmarshal([]byte(raw), &al); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"POST", "GET", ""} {
		if e := al.Match(m, "/api/v1/order"); e != nil {
			t.Errorf("method %q: an entry of another kind (or a malformed one) exempted an HTTP request: %+v", m, e)
		}
	}
	if v := MoneyGuardViolations(httpScenario("POST", "https://x.example/api/v1/order", naCleanup), al); len(v) == 0 {
		t.Error("a malformed list opened a write")
	}
}

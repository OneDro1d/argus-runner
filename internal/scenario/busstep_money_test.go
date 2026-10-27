package scenario

import (
	"strings"
	"testing"
)

// T7: against a money-handling system a bus step is judged as an amqp `listen` on the same exchange and routing key
// . The three-doors table (control/moneyallow_wire_test.go) pins that the doors agree.
// The refusal of a stream-bound step names the entry to add.
func TestMoneyGuard_StreamBoundBusStepRefusalNamesTheEntry(t *testing.T) {
	get := `{"type":"http","name":"g","method":"GET","url":"http://sut.invalid/x"}`
	v := MoneyGuardViolations(chainOf(get, `{"type":"bus","name":"b","stream":"logs"}`), nil)
	if len(v) != 1 || !strings.Contains(v[0], "engine: amqp, op: listen, stream: \"logs\"") {
		t.Errorf("the refusal must name the stream entry to add, got %v", v)
	}
}

func TestMoneyGuard_BusStepIsAnAMQPListen(t *testing.T) {
	get := `{"type":"http","name":"g","method":"GET","url":"http://sut.invalid/x"}`
	bus := `{"type":"bus","name":"b","exchange":"orders.incoming","routing_key":"orders.created"}`
	listen := MoneyWriteAllow{Engine: "amqp", Op: "listen", Exchange: "orders.incoming", RoutingKey: "orders.created"}
	streamBus := `{"type":"bus","name":"b","stream":"logs"}`
	streamEntry := MoneyWriteAllow{Engine: "amqp", Op: "listen", Stream: "logs"}
	cases := []struct {
		name  string
		step  string
		al    MoneyWriteAllowlist
		allow bool
	}{
		{"refused without a listen entry", bus, nil, false},
		{"allowed with the listen entry", bus, MoneyWriteAllowlist{listen}, true},
		{"another routing key is not covered", bus, MoneyWriteAllowlist{{Engine: "amqp", Op: "listen", Exchange: "orders.incoming", RoutingKey: "orders.deleted"}}, false},
		{"a consume entry is not a listen entry", bus, MoneyWriteAllowlist{{Engine: "amqp", Op: "consume", Queue: "q"}}, false},
		{"a logs entry does not allow it", bus, MoneyWriteAllowlist{{Engine: "logs"}}, false},
		{"a stream-bound step is not covered by an exchange + routing_key entry", streamBus, MoneyWriteAllowlist{listen}, false},
		{"a stream-bound step is covered by the stream entry", streamBus, MoneyWriteAllowlist{streamEntry}, true},
		{"another stream name is not covered", streamBus, MoneyWriteAllowlist{{Engine: "amqp", Op: "listen", Stream: "logs2"}}, false},
		{"a stream name is compared exactly (no prefix)", `{"type":"bus","name":"b","stream":"logs-extra"}`, MoneyWriteAllowlist{streamEntry}, false},
		{"a stream entry never covers an exchange-bound step", bus, MoneyWriteAllowlist{streamEntry}, false},
		{"a stream entry carrying the exchange + routing_key too covers neither",
			bus, MoneyWriteAllowlist{{Engine: "amqp", Op: "listen", Stream: "logs", Exchange: "orders.incoming", RoutingKey: "orders.created"}}, false},
		{"a stream entry carrying the exchange + routing_key too does not cover the stream step",
			streamBus, MoneyWriteAllowlist{{Engine: "amqp", Op: "listen", Stream: "logs", Exchange: "orders.incoming", RoutingKey: "orders.created"}}, false},
		{"a stream entry on another op covers nothing", streamBus, MoneyWriteAllowlist{{Engine: "amqp", Op: "consume", Stream: "logs"}}, false},
		{"a stream built at run time", `{"type":"bus","name":"b","stream":"${S}"}`, MoneyWriteAllowlist{{Engine: "amqp", Op: "listen", Stream: "${S}"}}, false},
		{"a binding built at run time", `{"type":"bus","name":"b","exchange":"${X}","routing_key":"orders.created"}`, MoneyWriteAllowlist{listen}, false},
	}
	for _, tc := range cases {
		v := MoneyGuardViolations(chainOf(get, tc.step), tc.al)
		if (len(v) == 0) != tc.allow {
			t.Errorf("%s: allowed = %v, want %v (%v)", tc.name, len(v) == 0, tc.allow, v)
		}
	}
}

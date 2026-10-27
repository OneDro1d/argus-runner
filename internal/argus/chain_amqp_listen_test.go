package argus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Listen through the WHOLE chain path (scenario.Parse -> runChainScenario -> chain.AMQPStep ->
// chain.Run) with a fake broker behind dialAMQPBroker.

const (
	listenStepJSON = `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"orders.events","routing_key":"order.created","save":{"q":"queue"}}`
	publishJSON    = `{"type":"amqp","name":"go","op":"publish","url_env":"BROKER_URL","exchange":"orders.events","routing_key":"order.created","body":"x"}`
)

func consumeJSON(extra string) string {
	return `{"type":"amqp","name":"take","op":"consume","url_env":"BROKER_URL","queue":"${saved.q}"` + extra + `}`
}

func runListenChain(t *testing.T, corr string, f *argusFakeBroker, claims string, steps ...string) report.ScenarioResult {
	t.Helper()
	withFakeBroker(t, f)
	s := scenario.Parse(chainRunMD("", `{"steps":[`+strings.Join(steps, ",")+`]}`, "### Runnable\n"+claims))
	return runChainScenario(&config.Config{}, s, corr, "testkit/ui")
}

const listenClaims = "- step tap: broker accepts\n- step go: broker accepts\n- step take: broker accepts\n"

func TestListenChain_ListenPublishConsumeAndTheQueueIsDeletedAtTheEnd(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`{"id":"1"}`)}
	res := runListenChain(t, "tr-lc1", f, listenClaims, listenStepJSON, publishJSON, consumeJSON(`,"wait":"90s"`))
	if res.Status != "passed" {
		t.Fatalf("chain = %q %+v %+v", res.Status, res.Failure, res.Steps)
	}
	want := "argus-tap-tr-lc1-0"
	if len(f.listens) != 1 || f.listens[0] != [4]string{want, "orders.events", "order.created", "120000"} {
		t.Errorf("listens = %v", f.listens)
	}
	if len(f.consumed) != 1 || f.consumed[0] != want || f.waits[0] != 90*time.Second {
		t.Errorf("consume reached the broker with %v / %v, want %s / 90s", f.consumed, f.waits, want)
	}
	if len(f.deleted) != 1 || f.deleted[0] != want {
		t.Errorf("deleted at chain end = %v, want [%s]", f.deleted, want)
	}
}

func TestListenChain_TheQueueIsDeletedWhenTheChainFails(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte(`{"id":"1"}`)}
	claims := "- step tap: broker accepts\n- step go: broker accepts\n- step take: broker refuses with 403\n"
	res := runListenChain(t, "tr-lc2", f, claims, listenStepJSON, publishJSON, consumeJSON(""))
	if res.Status != "failed" {
		t.Fatalf("setup: the chain must fail: %q %+v", res.Status, res.Failure)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "argus-tap-tr-lc2-0" {
		t.Errorf("deleted after a FAILED chain = %v, want the listen queue", f.deleted)
	}
}

func TestListenChain_APreflightRefusalOfALaterStepCreatesNothing(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	// the consume asks 90s of a queue that is not the listen's: refused before the listen is ever built
	foreign := `{"type":"amqp","name":"take","op":"consume","url_env":"BROKER_URL","queue":"msgbus.agent.alpha","wait":"90s"}`
	res := runListenChain(t, "tr-lc3", f, listenClaims, listenStepJSON, publishJSON, foreign)
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "`wait` must be a positive Go duration of at most 30s") ||
		!strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("want the 30s preflight refusal, got %q %+v", res.Status, res.Failure)
	}
	if len(f.listens) != 0 || len(f.deleted) != 0 || len(f.dialed) != 0 {
		t.Errorf("a refused chain created/deleted/dialed: listens=%v deleted=%v dialed=%v", f.listens, f.deleted, f.dialed)
	}
}

func TestListenChain_ADialErrorIsAnExecutionErrorAndCleanupDoesNotChangeIt(t *testing.T) {
	f := &argusFakeBroker{dialError: errors.New("connection refused")}
	res := runListenChain(t, "tr-lc4", f, "- step tap: broker accepts\n", listenStepJSON)
	if res.Status != report.StatusError {
		t.Fatalf("a dial error is an execution error: %q %+v", res.Status, res.Failure)
	}
	if len(f.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing (the listen never reached a broker)", f.deleted)
	}
}

func TestListenChain_SecondDoorBounds(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
		want  string
	}{
		{"121s on the listen queue", []string{listenStepJSON, consumeJSON(`,"wait":"121s"`)}, "at most 2m0s"},
		{"decode other than gzip", []string{listenStepJSON, consumeJSON(`,"decode":"zip"`)}, "its only value is \"gzip\""},
		{"save on a publish", []string{strings.Replace(publishJSON, `"body":"x"`, `"body":"x","save":{"q":"queue"}`, 1)}, "allowed on the `listen` op only"},
		{"the author names the listen queue", []string{strings.Replace(listenStepJSON, `"save"`, `"queue":"mine","save"`, 1)}, "named by Argus"},
		{"listen on the default exchange", []string{strings.Replace(listenStepJSON, `"exchange":"orders.events"`, `"exchange":""`, 1)}, "the default exchange cannot be bound"},
		{"queue_bind of a foreign queue", []string{`{"type":"amqp","name":"bind","op":"queue_bind","url_env":"BROKER_URL","queue":"live","exchange":"x","routing_key":"k"}`}, "which no `declare_queue` step in this chain declares"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
			claims := "- step tap: broker accepts\n- step take: broker accepts\n- step go: broker accepts\n- step bind: broker accepts\n"
			res := runListenChain(t, "tr-lc5", f, claims, c.steps...)
			if res.Failure == nil || !strings.Contains(res.Failure.Observed, c.want) {
				t.Fatalf("want %q, got %q %+v", c.want, res.Status, res.Failure)
			}
			if len(f.dialed) != 0 {
				t.Errorf("a refused step must not dial: %v", f.dialed)
			}
		})
	}
}

func TestListenChain_QueueBindOfTheChainsOwnQueueReachesTheBroker(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}}
	declare := `{"type":"amqp","name":"dq","op":"declare_queue","url_env":"BROKER_URL","queue":"mine"}`
	bind := `{"type":"amqp","name":"bind","op":"queue_bind","url_env":"BROKER_URL","queue":"mine","exchange":"x","routing_key":"k"}`
	res := runListenChain(t, "tr-lc6", f, "- step dq: broker accepts\n- step bind: broker accepts\n", declare, bind)
	if res.Status != "passed" || len(f.binds) != 1 || f.binds[0] != [3]string{"mine", "x", "k"} {
		t.Errorf("queue_bind: %q binds=%v", res.Status, f.binds)
	}
}

func TestListenChain_GzipAutomaticThroughTheWholePath(t *testing.T) {
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeEncoding: "gzip", consumeBody: []byte("not gzip")}
	res := runListenChain(t, "tr-lc7", f, "- step tap: broker accepts\n- step take: broker accepts\n", listenStepJSON, consumeJSON(""))
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "delivered message is not valid gzip") {
		t.Errorf("a delivery announcing gzip must be gunzipped: %q %+v", res.Status, res.Failure)
	}
}

package scenario

import (
	"strings"
	"testing"
	"time"
)

// The listen and queue_bind ops, the 120 s wait on a listen queue, decode, and the money
// guard's reading of a ${saved.…} queue. Every rule by its sentence.

const (
	lsListen  = `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"orders.events","routing_key":"order.created","save":{"q":"queue"}}`
	lsPublish = `{"type":"amqp","name":"go","op":"publish","url_env":"BROKER_URL","exchange":"orders.events","routing_key":"order.created","body":"x"}`
)

func lsConsume(extra string) string {
	return `{"type":"amqp","name":"take","op":"consume","url_env":"BROKER_URL","queue":"${saved.q}"` + extra + `}`
}

func lsValidate(t *testing.T, steps []string, claims string) []Error {
	t.Helper()
	_, errs := Validate(amqpMD(`{"steps":[`+strings.Join(steps, ",")+`]}`, "### Runnable\n"+claims))
	return errs
}

const lsClaims = "- step tap: broker accepts\n- step go: broker accepts\n- step take: broker accepts\n"

func TestListen_WellFormedChainWithA90sWaitIsAccepted(t *testing.T) {
	if errs := lsValidate(t, []string{lsListen, lsPublish, lsConsume(`,"wait":"90s","decode":"gzip"`)}, lsClaims); len(errs) != 0 {
		t.Fatalf("listen + publish + consume(wait 90s, decode gzip) must be accepted: %v", errs)
	}
}

func TestListen_WaitBounds(t *testing.T) {
	// 120 s is the ceiling on a listen queue
	if errs := lsValidate(t, []string{lsListen, lsConsume(`,"wait":"2m"`)}, "- step tap: broker accepts\n- step take: broker accepts\n"); len(errs) != 0 {
		t.Errorf("wait 2m on a listen queue must be accepted: %v", errs)
	}
	errs := lsValidate(t, []string{lsListen, lsConsume(`,"wait":"121s"`)}, "- step tap: broker accepts\n- step take: broker accepts\n")
	if !find(errs, "more than the 2m0s bound on a consume from a listen queue") {
		t.Errorf("121s must be refused with the listen bound: %v", errs)
	}
	// a foreign queue keeps 30 s
	foreign := `{"type":"amqp","name":"take","op":"consume","url_env":"BROKER_URL","queue":"msgbus.agent.hop","wait":"31s"}`
	errs = lsValidate(t, []string{foreign}, "- step take: broker accepts\n")
	if !find(errs, "more than the 30s bound on an amqp consume from a queue this chain did not create") {
		t.Errorf("31s on a foreign queue must be refused: %v", errs)
	}
	// a saved variable that is not a listen's is not Argus's queue either
	httpSave := `{"type":"http","name":"h","method":"GET","url":"https://x.example/a","save":{"q":"id"}}`
	errs = lsValidate(t, []string{httpSave, lsConsume(`,"wait":"90s"`)}, "- step h: status=200\n- step take: broker accepts\n")
	if !find(errs, "more than the 30s bound") {
		t.Errorf("a ${saved.q} an http step saved must keep the 30s bound: %v", errs)
	}
	// ...and neither is one saved by BOTH a listen and an http step
	errs = lsValidate(t, []string{lsListen, httpSave, lsConsume(`,"wait":"90s"`)}, "- step tap: broker accepts\n- step h: status=200\n- step take: broker accepts\n")
	if !find(errs, "more than the 30s bound") {
		t.Errorf("a variable another step also saves is not a listen queue: %v", errs)
	}
	// a name only PART of which is a saved ref is not the listen queue
	errs = lsValidate(t, []string{lsListen, `{"type":"amqp","name":"take","op":"consume","url_env":"BROKER_URL","queue":"x-${saved.q}","wait":"90s"}`},
		"- step tap: broker accepts\n- step take: broker accepts\n")
	if !find(errs, "more than the 30s bound") {
		t.Errorf("x-${saved.q} must not earn the long wait: %v", errs)
	}
}

func TestListen_ShapeRules(t *testing.T) {
	cases := []struct {
		name, step, want string
	}{
		{"no exchange", `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","routing_key":"k","save":{"q":"queue"}}`, "declares no `exchange`"},
		{"default exchange", `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"","routing_key":"k","save":{"q":"queue"}}`, "the default exchange (\"\") cannot be bound"},
		{"no routing key", `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"x","save":{"q":"queue"}}`, "declares no `routing_key`"},
		{"no save", `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"x","routing_key":"k"}`, "saves nothing"},
		{"author names the queue", `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"x","routing_key":"k","queue":"mine","save":{"q":"queue"}}`, `carries the field "queue"`},
		{"poll", `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"x","routing_key":"k","save":{"q":"queue"},"poll":{"timeout":"5s","interval":"1s"}}`, "may not carry `poll`"},
		{"always", `{"type":"amqp","name":"tap","op":"listen","url_env":"BROKER_URL","exchange":"x","routing_key":"k","save":{"q":"queue"},"always":true}`, "may not carry `always`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if errs := lsValidate(t, []string{c.step}, "- step tap: broker accepts\n"); !find(errs, c.want) {
				t.Errorf("want %q, got %v", c.want, errs)
			}
		})
	}
}

func TestListen_SaveIsAllowedOnListenOnly(t *testing.T) {
	pub := strings.Replace(lsPublish, `"body":"x"`, `"body":"x","save":{"q":"queue"}`, 1)
	if errs := lsValidate(t, []string{pub}, "- step go: broker accepts\n"); !find(errs, `carries the field "save", which that op does not accept`) {
		t.Errorf("save on a publish must be refused: %v", errs)
	}
}

func TestListen_DecodeValues(t *testing.T) {
	errs := lsValidate(t, []string{lsListen, lsConsume(`,"decode":"zip"`)}, "- step tap: broker accepts\n- step take: broker accepts\n")
	if !find(errs, "`decode` \"zip\" is not supported") {
		t.Errorf("decode zip must be refused by name: %v", errs)
	}
	decodeOnPublish := strings.Replace(lsPublish, `"body":"x"`, `"body":"x","decode":"gzip"`, 1)
	if errs := lsValidate(t, []string{decodeOnPublish}, "- step go: broker accepts\n"); !find(errs, `carries the field "decode"`) {
		t.Errorf("decode on a publish must be refused: %v", errs)
	}
}

func TestListen_QueueBindAndTheDeclaredNameGuard(t *testing.T) {
	bind := func(queue, extra string) string {
		return `{"type":"amqp","name":"bind","op":"queue_bind","url_env":"BROKER_URL","queue":"` + queue + `","exchange":"x","routing_key":"k"` + extra + `}`
	}
	declare := `{"type":"amqp","name":"dq","op":"declare_queue","url_env":"BROKER_URL","queue":"mine"}`
	claims := "- step bind: broker accepts\n"
	if errs := lsValidate(t, []string{bind("live.queue", "")}, claims); !find(errs, "would bind queue \"live.queue\", which no `declare_queue` step in this chain declares") {
		t.Errorf("binding a foreign queue must be refused by name: %v", errs)
	}
	if errs := lsValidate(t, []string{bind("live.queue", `,"force":true`)}, claims); len(errs) != 0 {
		t.Errorf("force must allow binding a foreign queue: %v", errs)
	}
	if errs := lsValidate(t, []string{declare, bind("mine", "")}, "- step dq: broker accepts\n"+claims); len(errs) != 0 {
		t.Errorf("binding a queue the chain declared must be accepted: %v", errs)
	}
	if errs := lsValidate(t, []string{lsListen, bind("${saved.q}", "")}, "- step tap: broker accepts\n"+claims); len(errs) != 0 {
		t.Errorf("binding the listen queue must be accepted: %v", errs)
	}
	missing := `{"type":"amqp","name":"bind","op":"queue_bind","url_env":"BROKER_URL","queue":"mine"}`
	errs := lsValidate(t, []string{declare, missing}, "- step dq: broker accepts\n"+claims)
	if !find(errs, "declares no `exchange`") || !find(errs, "declares no `routing_key`") {
		t.Errorf("queue_bind needs exchange and routing_key: %v", errs)
	}
}

func TestListen_CleanupOpsAcceptTheListenQueue(t *testing.T) {
	del := `{"type":"amqp","name":"del","op":"queue_delete","url_env":"BROKER_URL","queue":"${saved.q}"}`
	unbind := `{"type":"amqp","name":"unb","op":"queue_unbind","url_env":"BROKER_URL","queue":"${saved.q}","exchange":"orders.events","routing_key":"order.created"}`
	claims := "- step tap: broker accepts\n- step del: broker accepts\n- step unb: broker accepts\n"
	if errs := lsValidate(t, []string{lsListen, unbind, del}, claims); len(errs) != 0 {
		t.Errorf("queue_unbind / queue_delete of the listen queue need no force: %v", errs)
	}
	other := strings.Replace(del, "${saved.q}", "${saved.z}", 1)
	if errs := lsValidate(t, []string{lsListen, other}, "- step tap: broker accepts\n- step del: broker accepts\n"); !find(errs, "would delete queue") {
		t.Errorf("a ${saved.z} no listen made is still guarded: %v", errs)
	}
	// deleting the exchange the listen bound is still a foreign-exchange delete
	exdel := `{"type":"amqp","name":"xd","op":"exchange_delete","url_env":"BROKER_URL","exchange":"orders.events"}`
	if errs := lsValidate(t, []string{lsListen, exdel}, "- step tap: broker accepts\n- step xd: broker accepts\n"); !find(errs, "would delete exchange") {
		t.Errorf("a listen must not make exchange_delete of its exchange easier: %v", errs)
	}
}

func TestListen_NamesAndHelpers(t *testing.T) {
	if got := ListenQueueName("tr-ab12", 4); got != "argus-tap-tr-ab12-4" {
		t.Errorf("ListenQueueName = %q", got)
	}
	if ListenQueueTTLMillis != 120000 || ListenQueuePrefix != "argus-tap-" || AMQPListenWaitMax != 120*time.Second {
		t.Error("the tap constants moved")
	}
	if v, ok := SavedWholeRef(" ${saved.q} "); !ok || v != "q" {
		t.Errorf("SavedWholeRef = %q %v", v, ok)
	}
	if _, ok := SavedWholeRef("a${saved.q}"); ok {
		t.Error("a partial ref is not a whole ref")
	}
}

// ── the money guard ─────────────────────────────────────────────────────────────────────────────

var lsEntry = MoneyWriteAllow{Engine: "amqp", Op: "listen", Exchange: "orders.events", RoutingKey: "order.created"}

func lsMoneyStep(name, op, fields string) string { return amqpStep(name, op, fields) }

func lsMoneyListen() string {
	return lsMoneyStep("tap", "listen", `,"exchange":"orders.events","routing_key":"order.created","save":{"q":"queue"}`)
}

func TestMoney_ListenAndItsOwnQueueAreAllowedByTheListenEntryAlone(t *testing.T) {
	al := MoneyWriteAllowlist{lsEntry}
	for _, tail := range []string{
		lsMoneyStep("take", "consume", `,"queue":"${saved.q}"`),
		lsMoneyStep("del", "queue_delete", `,"queue":"${saved.q}"`),
	} {
		if v := MoneyGuardViolations(chainOf(lsMoneyListen(), tail), al); len(v) != 0 {
			t.Errorf("listen + its own queue must be allowed by the listen entry: %v", v)
		}
	}
	// FAIL CLOSED without the entry: the listen is refused, and so is the step that reads its queue
	v := MoneyGuardViolations(chainOf(lsMoneyListen(), lsMoneyStep("take", "consume", `,"queue":"${saved.q}"`)), nil)
	if len(v) != 2 || !strings.HasPrefix(v[0], `chain step tap is a "amqp" step — against a money-handling system only GET http steps may run`) {
		t.Errorf("without an entry both steps must be refused, the base sentence first: %v", v)
	}
	// an entry that does not name this listen's exchange covers neither the listen nor its queue
	wrong := MoneyWriteAllowlist{{Engine: "amqp", Op: "listen", Exchange: "other.events", RoutingKey: "order.created"}}
	if v := MoneyGuardViolations(chainOf(lsMoneyListen(), lsMoneyStep("take", "consume", `,"queue":"${saved.q}"`)), wrong); len(v) != 2 {
		t.Errorf("a listen entry for another exchange covers nothing: %v", v)
	}
}

func TestMoney_ASavedQueueNotMadeByAListenNeverMatches(t *testing.T) {
	al := MoneyWriteAllowlist{lsEntry, {Engine: "amqp", Op: "consume", Queue: "${saved.q}"}}
	take := lsMoneyStep("take", "consume", `,"queue":"${saved.q}"`)
	httpSave := `{"type":"http","name":"h","method":"GET","url":"https://x.example/a","save":{"q":"id"}}`
	// 1. saved by an http step only, a listen entry exists
	if v := MoneyGuardViolations(chainOf(httpSave, take), al[:1]); len(v) == 0 {
		t.Error("a ${saved.q} an http step saved was allowed by a listen entry")
	}
	// 2. saved by a listen AND an http step: the value may be the SUT's answer
	if v := MoneyGuardViolations(chainOf(lsMoneyListen(), httpSave, take), al[:1]); len(v) == 0 {
		t.Error("a variable an http step also saves was treated as the listen's queue")
	}
	// 3. a variable no step saves
	if v := MoneyGuardViolations(chainOf(lsMoneyListen(), lsMoneyStep("take", "consume", `,"queue":"${saved.z}"`)), al[:1]); len(v) == 0 {
		t.Error("an unsaved ${saved.z} was allowed")
	}
	// 4. only part of the name is a saved ref
	if v := MoneyGuardViolations(chainOf(lsMoneyListen(), lsMoneyStep("take", "consume", `,"queue":"prefix-${saved.q}"`)), al[:1]); len(v) == 0 {
		t.Error("prefix-${saved.q} was allowed")
	}
	// 5. the saving step comes from a non-amqp type that merely uses the same op word
	notListen := `{"type":"amqp","name":"dq","op":"declare_queue","url_env":"BROKER_URL","queue":"x","save":{"q":"queue"}}`
	if v := MoneyGuardViolations(chainOf(notListen, take), al[:1]); len(v) == 0 {
		t.Error("a variable saved by a non-listen amqp step was allowed")
	}
	// 6. an entry cannot list a run-time name for a consume either (unchanged rule)
	if v := MoneyGuardViolations(chainOf(httpSave, take), al); len(v) == 0 {
		t.Error("an entry naming ${saved.q} matched a run-time name")
	}
}

func TestMoney_ListenAndQueueBindAreListable(t *testing.T) {
	for _, tc := range []struct {
		op, fields string
		entry      MoneyWriteAllow
		near       string
	}{
		{"listen", `,"exchange":"x","routing_key":"k","save":{"q":"queue"}`,
			MoneyWriteAllow{Engine: "amqp", Op: "listen", Exchange: "x", RoutingKey: "k"}, `,"exchange":"x","routing_key":"j","save":{"q":"queue"}`},
		{"queue_bind", `,"queue":"mine","exchange":"x","routing_key":"k"`,
			MoneyWriteAllow{Engine: "amqp", Op: "queue_bind", Queue: "mine", Exchange: "x", RoutingKey: "k"}, `,"queue":"yours","exchange":"x","routing_key":"k"`},
	} {
		al := MoneyWriteAllowlist{tc.entry}
		if v := MoneyGuardViolations(chainOf(amqpStep("s", tc.op, tc.fields)), al); len(v) != 0 {
			t.Errorf("%s: its entry did not allow it: %v", tc.op, v)
		}
		if v := MoneyGuardViolations(chainOf(amqpStep("s", tc.op, tc.near)), al); len(v) == 0 {
			t.Errorf("%s: a near miss was allowed", tc.op)
		}
		if v := MoneyGuardViolations(chainOf(amqpStep("s", tc.op, tc.fields)), nil); len(v) != 1 {
			t.Errorf("%s: without the entry it must be refused: %v", tc.op, v)
		}
	}
	// the owner can only list the names the op uses: a queue on a listen entry covers nothing
	badListen := MoneyWriteAllowlist{{Engine: "amqp", Op: "listen", Exchange: "x", RoutingKey: "k", Queue: "q"}}
	if v := MoneyGuardViolations(chainOf(amqpStep("s", "listen", `,"exchange":"x","routing_key":"k","save":{"q":"queue"}`)), badListen); len(v) == 0 {
		t.Error("a listen entry carrying a queue was allowed")
	}
}

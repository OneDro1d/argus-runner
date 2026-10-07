package scenario

import (
	"strings"
	"testing"
)

// AC-D18b — an "amqp" chain step is refused BY NAME at authoring (seed) time. A real agent's inbox
// is on the other end of a publish, so the shape rules here are the first line of the one-publish
// safety contract: no `poll`, no `always`, a url_env that is a NAME (never a URL), exactly one of
// envelope/body, and exactly the two `broker …` claim forms.

const amqpPublishEnvelope = `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"MSGBUS_ARGUS_TEST_URL",` +
	`"exchange":"msgbus.inbox","routing_key":"inbox.agent.hop","user_id":"hop",` +
	`"envelope":{"to_inbox":"agent.hop","from_agent_id":"argus-test","intent":"INFORM","prompt":"argus-test: ${correlation_id}"}}]}`

func amqpMD(trigger, expect string) string { return chainMD(trigger, expect) }

func TestValidate_AMQPStepWellFormedAccepted(t *testing.T) {
	cases := []struct{ name, trigger, expect string }{
		{"publish envelope, broker refuses with 406", amqpPublishEnvelope,
			"### Runnable\n- step spoof: broker refuses with 406\n"},
		{"publish envelope, broker accepts",
			`{"steps":[{"type":"amqp","name":"send","op":"publish","url_env":"ARGUS_TEST_AMQP_URL","exchange":"msgbus.inbox",` +
				`"routing_key":"inbox.agent.hop","envelope":{"to_inbox":"agent.hop","from_agent_id":"argus-test","prompt":"p ${cid}"}}]}`,
			"### Runnable\n- step send: broker accepts\n"},
		{"publish raw body, broker refuses with 403",
			`{"steps":[{"type":"amqp","name":"all","op":"publish","url_env":"ARGUS_TEST_AMQP_URL","exchange":"msgbus.inbox",` +
				`"routing_key":"inbox.all","body":"raw probe"}]}`,
			"### Runnable\n- step all: broker refuses with 403\n"},
		{"consume, broker refuses with 403",
			`{"steps":[{"type":"amqp","name":"steal","op":"consume","url_env":"ARGUS_TEST_AMQP_URL","queue":"msgbus.agent.hop","wait":"2s"}]}`,
			"### Runnable\n- step steal: broker refuses with 403\n"},
		{"declare_queue, broker refuses with 403",
			`{"steps":[{"type":"amqp","name":"dq","op":"declare_queue","url_env":"ARGUS_TEST_AMQP_URL","queue":"msgbus.agent.x"}]}`,
			"### Runnable\n- step dq: broker refuses with 403\n"},
		{"declare_exchange, broker accepts",
			`{"steps":[{"type":"amqp","name":"dx","op":"declare_exchange","url_env":"ARGUS_TEST_AMQP_URL","exchange":"x.y","exchange_kind":"fanout"}]}`,
			"### Runnable\n- step dx: broker accepts\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, errs := Validate(amqpMD(c.trigger, c.expect)); len(errs) != 0 {
				t.Fatalf("a well-formed amqp step must be accepted, got %v", errs)
			}
		})
	}
}

func TestValidate_AMQPStepMissingURLEnvRefused(t *testing.T) {
	trig := strings.Replace(amqpPublishEnvelope, `"url_env":"MSGBUS_ARGUS_TEST_URL",`, "", 1)
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "declares no `url_env`") {
		t.Fatalf("a missing url_env must be refused by name; got %v", errs)
	}
}

// A literal URL (with credentials) in url_env is refused — and the refusal must not echo it.
func TestValidate_AMQPStepLiteralURLEnvRefused(t *testing.T) {
	trig := strings.Replace(amqpPublishEnvelope, `"MSGBUS_ARGUS_TEST_URL"`, `"amqp://u:p@h/"`, 1)
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "`url_env` must be an environment variable NAME") {
		t.Fatalf("a literal url in url_env must be refused by name; got %v", errs)
	}
	for _, e := range errs {
		if strings.Contains(e.Message, "u:p@") {
			t.Fatalf("the refusal must never echo the literal credential: %q", e.Message)
		}
	}
}

func TestValidate_AMQPStepPollRefused(t *testing.T) {
	trig := strings.Replace(amqpPublishEnvelope, `"op":"publish",`, `"op":"publish","poll":{"timeout":"30s","interval":"5s"},`, 1)
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "may not carry `poll`") {
		t.Fatalf("poll on an amqp step must be refused by name; got %v", errs)
	}
}

func TestValidate_AMQPStepAlwaysRefused(t *testing.T) {
	trig := strings.Replace(amqpPublishEnvelope, `"op":"publish",`, `"op":"publish","always":true,`, 1)
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "may not carry `always`") {
		t.Fatalf("always on an amqp step must be refused by name; got %v", errs)
	}
}

func TestValidate_AMQPStepUnknownOpRefused(t *testing.T) {
	trig := strings.Replace(amqpPublishEnvelope, `"op":"publish"`, `"op":"purge"`, 1)
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "unknown `op`") {
		t.Fatalf("an unknown op must be refused by name; got %v", errs)
	}
}

func TestValidate_AMQPPublishNeedsExactlyOneOfEnvelopeOrBody(t *testing.T) {
	// ARGUS-DESIGN-GENERIC-SCHEMA-MESSAGES-2026-09-28 (design §2) added a third publish form,
	// `schema`+`record`, so the refusal now names all three — the substring below is the part of
	// that message every "wrong number of forms" case still shares.
	both := strings.Replace(amqpPublishEnvelope, `"user_id":"hop",`, `"user_id":"hop","body":"x",`, 1)
	_, errs := Validate(amqpMD(both, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "exactly one of `envelope`, `body` or `schema`+`record`") {
		t.Fatalf("a publish with both envelope and body must be refused; got %v", errs)
	}
	neither := `{"steps":[{"type":"amqp","name":"spoof","op":"publish","url_env":"ARGUS_TEST_AMQP_URL",` +
		`"exchange":"msgbus.inbox","routing_key":"inbox.agent.hop"}]}`
	_, errs2 := Validate(amqpMD(neither, "### Runnable\n- step spoof: broker accepts\n"))
	if !find(errs2, "exactly one of `envelope`, `body` or `schema`+`record`") {
		t.Fatalf("a publish with neither envelope nor body must be refused; got %v", errs2)
	}
}

func TestValidate_AMQPStepNonBrokerClaimRefused(t *testing.T) {
	for _, claim := range []string{"status=200", "body contains ok", "result.isError == false"} {
		_, errs := Validate(amqpMD(amqpPublishEnvelope, "### Runnable\n- step spoof: "+claim+"\n"))
		if !find(errs, "is not an amqp-step claim") {
			t.Fatalf("claim %q on an amqp step must be refused by name; got %v", claim, errs)
		}
	}
}

func TestValidate_AMQPStepBadRefusalCodeRefused(t *testing.T) {
	for _, claim := range []string{"broker refuses with 4x6", "broker refuses with 40", "broker refuses", "broker accepts it"} {
		_, errs := Validate(amqpMD(amqpPublishEnvelope, "### Runnable\n- step spoof: "+claim+"\n"))
		if !find(errs, "is not an amqp-step claim") {
			t.Fatalf("claim %q must be refused; got %v", claim, errs)
		}
	}
}

func TestValidate_BrokerClaimOnHTTPStepRefused(t *testing.T) {
	trig := `{"steps":[{"type":"http","name":"create","method":"POST","url":"http://x/y"}]}`
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step create: broker accepts\n"))
	if !find(errs, "a `broker …` claim belongs to an `amqp` step") {
		t.Fatalf("a broker claim on an http step must be refused by name; got %v", errs)
	}
}

func TestValidate_AMQPStepWithNoClaimRefused(t *testing.T) {
	// Two steps, only one claimed — so the file as a whole has a bullet and the per-step guard is
	// what must speak.
	trig := `{"steps":[{"type":"amqp","name":"dq","op":"declare_queue","url_env":"ARGUS_TEST_AMQP_URL","queue":"q"},` +
		`{"type":"amqp","name":"dq2","op":"declare_queue","url_env":"ARGUS_TEST_AMQP_URL","queue":"q2"}]}`
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step dq: broker refuses with 403\n"))
	if !find(errs, "step \"dq2\" has no claim — every `amqp` step needs at least one") {
		t.Fatalf("an amqp step with no claim must be refused by name; got %v", errs)
	}
}

func TestValidate_AMQPStepContradictoryClaimsRefused(t *testing.T) {
	_, errs := Validate(amqpMD(amqpPublishEnvelope,
		"### Runnable\n- step spoof: broker accepts\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "exactly one `broker …` claim") {
		t.Fatalf("two different broker claims on one step can never both pass and must be refused; got %v", errs)
	}
}

func TestValidate_AMQPStepEnvelopeShapeRefused(t *testing.T) {
	badIntent := strings.Replace(amqpPublishEnvelope, `"intent":"INFORM"`, `"intent":"APPROVAL"`, 1)
	_, errs := Validate(amqpMD(badIntent, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "`envelope.intent`") {
		t.Fatalf("an intent outside INFORM/ASK/DECISION_REQUEST must be refused; got %v", errs)
	}
	noInbox := strings.Replace(amqpPublishEnvelope, `"to_inbox":"agent.hop",`, "", 1)
	_, errs2 := Validate(amqpMD(noInbox, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs2, "`envelope.to_inbox`") {
		t.Fatalf("an envelope with no to_inbox must be refused; got %v", errs2)
	}
}

func TestValidate_AMQPConsumeWaitBounded(t *testing.T) {
	trig := `{"steps":[{"type":"amqp","name":"steal","op":"consume","url_env":"ARGUS_TEST_AMQP_URL","queue":"q","wait":"31s"}]}`
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step steal: broker refuses with 403\n"))
	if !find(errs, "30s bound") {
		t.Fatalf("a consume wait over 30s must be refused; got %v", errs)
	}
	pubWait := strings.Replace(amqpPublishEnvelope, `"op":"publish",`, `"op":"publish","wait":"2s",`, 1)
	_, errs2 := Validate(amqpMD(pubWait, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs2, `carries the field "wait"`) {
		t.Fatalf("wait is consume-only and must be refused on a publish; got %v", errs2)
	}
}

// P3 #24a/d — the consume-only claim extensions: `queue is empty` and a `body …` assertion.
func TestValidate_AMQPConsumeExtendedClaimsAccepted(t *testing.T) {
	consumeTrig := func(extra string) string {
		return `{"steps":[{"type":"amqp","name":"steal","op":"consume","url_env":"ARGUS_TEST_AMQP_URL","queue":"q"` + extra + `}]}`
	}
	cases := []struct{ name, expect string }{
		{"queue is empty, alone", "### Runnable\n- step steal: queue is empty\n"},
		{"a whole-body assertion, alone (implies broker accepts)", "### Runnable\n- step steal: body contains ok\n"},
		{"a field-scoped equals assertion", "### Runnable\n- step steal: body has status equals ok\n"},
		{"broker accepts PLUS a body assertion", "### Runnable\n- step steal: broker accepts\n- step steal: body contains ok\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, errs := Validate(amqpMD(consumeTrig(""), c.expect)); len(errs) != 0 {
				t.Fatalf("must be accepted: %v", errs)
			}
		})
	}
}

func TestValidate_AMQPConsumeExtendedClaimContradictionsRefused(t *testing.T) {
	consumeTrig := `{"steps":[{"type":"amqp","name":"steal","op":"consume","url_env":"ARGUS_TEST_AMQP_URL","queue":"q"}]}`
	cases := []struct{ name, expect, want string }{
		{"empty + body assertion", "### Runnable\n- step steal: queue is empty\n- step steal: body contains ok\n",
			"cannot be combined with a body assertion"},
		{"empty + broker refuses", "### Runnable\n- step steal: queue is empty\n- step steal: broker refuses with 403\n",
			"cannot be combined with a `broker …` claim"},
		{"body assertion + broker refuses", "### Runnable\n- step steal: broker refuses with 403\n- step steal: body contains ok\n",
			"a body assertion requires the message to arrive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, errs := Validate(amqpMD(consumeTrig, c.expect)); !find(errs, c.want) {
				t.Fatalf("must be refused with %q; got %v", c.want, errs)
			}
		})
	}
}

// `queue is empty` and a body assertion are consume-only — every other op keeps the original,
// unchanged two-form grammar (and its original error message).
func TestValidate_AMQPNonConsumeStepRejectsExtendedClaims(t *testing.T) {
	for _, claim := range []string{"queue is empty", "body contains ok"} {
		_, errs := Validate(amqpMD(amqpPublishEnvelope, "### Runnable\n- step spoof: "+claim+"\n"))
		if !find(errs, "is not an amqp-step claim") {
			t.Fatalf("claim %q on a publish step must still be refused by the original message; got %v", claim, errs)
		}
	}
}

// P3 #24b — publish headers: string values only, and the schema marker is reserved.
func TestValidate_AMQPPublishHeaders(t *testing.T) {
	t.Run("string values accepted", func(t *testing.T) {
		trig := strings.Replace(amqpPublishEnvelope, `"user_id":"hop",`, `"user_id":"hop","headers":{"x-retry-count":"3"},`, 1)
		if _, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n")); len(errs) != 0 {
			t.Fatalf("string header values must be accepted: %v", errs)
		}
	})
	t.Run("non-string value refused BY NAME", func(t *testing.T) {
		trig := strings.Replace(amqpPublishEnvelope, `"user_id":"hop",`, `"user_id":"hop","headers":{"x-retry-count":3},`, 1)
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
		if !find(errs, `header "x-retry-count" must be a string value`) {
			t.Fatalf("a non-string header value must be refused by the KEY's name; got %v", errs)
		}
	})
	t.Run("the schema marker header is reserved", func(t *testing.T) {
		trig := strings.Replace(amqpPublishEnvelope, `"user_id":"hop",`, `"user_id":"hop","headers":{"x-envelope-schema":"9"},`, 1)
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
		if !find(errs, "is reserved") {
			t.Fatalf("x-envelope-schema must be refused as reserved; got %v", errs)
		}
	})
	t.Run("headers on a non-publish op are a stray field", func(t *testing.T) {
		trig := `{"steps":[{"type":"amqp","name":"dq","op":"declare_queue","url_env":"ARGUS_TEST_AMQP_URL","queue":"q","headers":{"a":"b"}}]}`
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step dq: broker accepts\n"))
		if !find(errs, `carries the field "headers"`) {
			t.Fatalf("headers on declare_queue must be refused as a stray field; got %v", errs)
		}
	})
}

// P3 #24c — queue_delete/queue_unbind/exchange_delete: refused BY DEFAULT when the chain never
// declared the name, accepted when it did, and accepted with `force: true` regardless.
func TestValidate_AMQPCleanupOpsDeclaredNameGuard(t *testing.T) {
	t.Run("queue_delete of an undeclared queue is refused", func(t *testing.T) {
		trig := `{"steps":[{"type":"amqp","name":"cleanup","op":"queue_delete","url_env":"ARGUS_TEST_AMQP_URL","queue":"q1"}]}`
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step cleanup: broker accepts\n"))
		if !find(errs, "no `declare_queue` step in this chain declares") {
			t.Fatalf("an undeclared queue_delete must be refused; got %v", errs)
		}
	})
	t.Run("queue_delete of a declared queue is accepted", func(t *testing.T) {
		trig := `{"steps":[` +
			`{"type":"amqp","name":"dq","op":"declare_queue","url_env":"ARGUS_TEST_AMQP_URL","queue":"q1"},` +
			`{"type":"amqp","name":"cleanup","op":"queue_delete","url_env":"ARGUS_TEST_AMQP_URL","queue":"q1"}]}`
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step dq: broker accepts\n- step cleanup: broker accepts\n"))
		if len(errs) != 0 {
			t.Fatalf("a queue_delete of a declared queue must be accepted: %v", errs)
		}
	})
	t.Run("force:true bypasses the guard", func(t *testing.T) {
		trig := `{"steps":[{"type":"amqp","name":"cleanup","op":"queue_delete","url_env":"ARGUS_TEST_AMQP_URL","queue":"q1","force":true}]}`
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step cleanup: broker accepts\n"))
		if len(errs) != 0 {
			t.Fatalf("force:true must bypass the declared-name guard: %v", errs)
		}
	})
	t.Run("queue_unbind checks BOTH the queue and the exchange", func(t *testing.T) {
		trig := `{"steps":[` +
			`{"type":"amqp","name":"dq","op":"declare_queue","url_env":"ARGUS_TEST_AMQP_URL","queue":"q1"},` +
			`{"type":"amqp","name":"cleanup","op":"queue_unbind","url_env":"ARGUS_TEST_AMQP_URL","queue":"q1","exchange":"x1","routing_key":"rk"}]}`
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step dq: broker accepts\n- step cleanup: broker accepts\n"))
		if !find(errs, "no `declare_exchange` step in this chain declares") {
			t.Fatalf("an undeclared exchange must still be refused even though the queue is declared; got %v", errs)
		}
	})
	t.Run("exchange_delete of an undeclared exchange is refused", func(t *testing.T) {
		trig := `{"steps":[{"type":"amqp","name":"cleanup","op":"exchange_delete","url_env":"ARGUS_TEST_AMQP_URL","exchange":"x1"}]}`
		_, errs := Validate(amqpMD(trig, "### Runnable\n- step cleanup: broker accepts\n"))
		if !find(errs, "no `declare_exchange` step in this chain declares") {
			t.Fatalf("an undeclared exchange_delete must be refused; got %v", errs)
		}
	})
}

// The step must never be able to smuggle its own broker URL into a message it publishes.
func TestValidate_AMQPStepReferencingItsOwnURLEnvRefused(t *testing.T) {
	trig := strings.Replace(amqpPublishEnvelope, `"prompt":"argus-test: ${correlation_id}"`, `"prompt":"${MSGBUS_ARGUS_TEST_URL}"`, 1)
	_, errs := Validate(amqpMD(trig, "### Runnable\n- step spoof: broker refuses with 406\n"))
	if !find(errs, "references its own `url_env`") {
		t.Fatalf("a field that expands the broker URL must be refused; got %v", errs)
	}
}

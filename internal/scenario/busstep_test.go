package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// The chain `bus` step at the AUTHORING door. Each refusal is asserted by its SENTENCE, because the
// sentence is the interface: the executor's preflight must say the same thing (argus/chain_bus_test.go).

const busTrigger0 = `{"type":"http","name":"go","method":"POST","url":"http://sut.invalid/o","body":{"a":1}}`

// busChainTrig builds a chain TRIGGER: an http step that causes the traffic, then one bus step named "b". fields is the
// rest of the bus step (leading comma added here), "" for none.
func busChainTrig(fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return `{"steps":[` + busTrigger0 + `,{"type":"bus","name":"b"` + fields + `}]}`
}

const busBinding = `"exchange":"orders.incoming","routing_key":"orders.created"`
const busOKExpect = "### Runnable\n- step go: status=202\n- step b: body has count >= 1\n"

func busErrs(trigger, expect string) []Error {
	_, errs := Validate(chainMD(trigger, expect))
	return errs
}

// T2: "bus" is a chain step type; before this change it was refused as an unknown type.
func TestBusIsAChainStepType(t *testing.T) {
	if !contains(ChainStepTypes, "bus") {
		t.Fatalf("ChainStepTypes = %v, want bus in it", ChainStepTypes)
	}
	if why := UnknownStepTypeProblem("bus"); why != "" {
		t.Fatalf("bus must be a known step type, got %q", why)
	}
}

func TestValidate_BusStepWellFormedAccepted(t *testing.T) {
	cases := []struct{ name, trigger, expect string }{
		// Example A of the design: presence, with a where filter and a save
		{"example A", busChainTrig(`"target":"sut-bus",` + busBinding + `,"where":{"status":"pending"},"wait":"10s","save":{"oid":"messages.0.body.order_id"}`),
			"### Runnable\n- step go: status=202\n- step b: body has count >= 1\n- step b: body has messages.0.body.currency equals EUR\n"},
		// Example B: an absence with its anchor
		{"example B", busChainTrig(`"target":"sut-bus","stream":"logs","where":{"level":["error","ERROR","fatal","FATAL"]},"wait":"10s"`),
			"### Runnable\n- step go: status=202\n- step b: body has count == 0\n- step b: body has correlated >= 1\n"},
		{"minimal", busChainTrig(busBinding), busOKExpect},
		{"wait bounds", busChainTrig(busBinding + `,"wait":"1s"`), busOKExpect},
		{"wait max", busChainTrig(busBinding + `,"wait":"60s"`), busOKExpect},
		{"a cid and a saved value in where", `{"steps":[{"type":"http","name":"go","method":"POST","url":"http://sut.invalid/o","save":{"oid":"id"}},` +
			`{"type":"bus","name":"b",` + busBinding + `,"where":{"order_id":"${saved.oid}","trace":"${cid}"}}]}`,
			"### Runnable\n- step go: status=200\n- step b: body has count >= 1\n"},
		{"a narrow wildcard", busChainTrig(`"exchange":"orders","routing_key":"orders.*"`), busOKExpect},
		{"presence needs no anchor", busChainTrig(busBinding), "### Runnable\n- step go: status=202\n- step b: body has count >= 3\n"},
		{"absence with a greater-than anchor", busChainTrig(busBinding), "### Runnable\n- step go: status=202\n- step b: body has count < 1\n- step b: body has correlated > 0\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if errs := busErrs(c.trigger, c.expect); len(errs) != 0 {
				t.Fatalf("a well-formed bus step must be accepted, got %v", errs)
			}
		})
	}
}

// T4: the shape table, each by its sentence.
func TestValidate_BusStepShapeRefusals(t *testing.T) {
	cases := []struct{ name, fields, want string }{
		{"no binding", ``, "this bus step declares no binding"},
		{"default exchange", `"exchange":"","routing_key":"orders.created"`, "the default exchange cannot be subscribed to"},
		{"exchange without a key", `"exchange":"orders"`, "`routing_key` is missing or empty"},
		{"a key without an exchange", `"routing_key":"orders.created"`, "`exchange` is empty"},
		{"placeholder in the exchange", `"exchange":"${EX}","routing_key":"a.b"`, "`exchange` carries `${…}`"},
		{"placeholder in the key", `"exchange":"x","routing_key":"orders.${cid}"`, "`routing_key` carries `${…}`"},
		{"bare #", `"exchange":"x","routing_key":"#"`, "binds the whole exchange"},
		{"hash words only", `"exchange":"x","routing_key":"#.#"`, "binds the whole exchange"},
		{"stream with exchange", `"stream":"logs","exchange":"x","routing_key":"a.b"`, "both `stream` and `exchange`/`routing_key`"},
		{"stream with a key", `"stream":"logs","routing_key":"a.b"`, "both `stream` and `exchange`/`routing_key`"},
		{"empty stream", `"stream":""`, "`stream` is empty"},
		{"stream placeholder", `"stream":"${S}"`, "`stream` carries `${…}`"},
		{"wait zero", busBinding + `,"wait":"0s"`, "outside the 1s..1m0s bounds of a bus step"},
		{"wait 61s", busBinding + `,"wait":"61s"`, "outside the 1s..1m0s bounds of a bus step"},
		{"wait not a duration", busBinding + `,"wait":"soon"`, "`wait` \"soon\" is not a Go duration"},
		{"poll", busBinding + `,"poll":{"timeout":"10s","interval":"1s"}`, "may not carry `poll` or `always`"},
		{"always", busBinding + `,"always":true`, "may not carry `poll` or `always`"},
		{"a stray key", busBinding + `,"url":"http://evil.invalid"`, `this bus step carries the field "url"`},
		{"where not an object", busBinding + `,"where":["a"]`, "`where` must be a JSON object"},
		{"where value not a string", busBinding + `,"where":{"a":5}`, "`where` value of \"a\" is neither a string nor an array of strings"},
		{"where empty value", busBinding + `,"where":{"a":""}`, "`where` value of \"a\" is empty"},
		{"where empty list", busBinding + `,"where":{"a":[]}`, "`where` value of \"a\" is an empty list"},
		{"where bad path", busBinding + `,"where":{"a b":"x"}`, "`where` path \"a b\" is not a body path"},
		{"where foreign placeholder", busBinding + `,"where":{"a":"${TOKEN}"}`, "carries `${TOKEN}`, which nothing fills in"},
		{"where empty object", busBinding + `,"where":{}`, "`where` is an empty object"},
		{"target placeholder", busBinding + `,"target":"${T}"`, "`target` carries `${…}`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := busErrs(busChainTrig(c.fields), busOKExpect)
			if !find(errs, c.want) {
				t.Fatalf("want a refusal containing %q, got %v", c.want, errs)
			}
		})
	}
	t.Run("a bus step at index 0", func(t *testing.T) {
		trig := `{"steps":[{"type":"bus","name":"b",` + busBinding + `},` + busTrigger0 + `]}`
		errs := busErrs(trig, "### Runnable\n- step go: status=202\n- step b: body has count >= 1\n")
		if !find(errs, "this bus step is the chain's first step") {
			t.Fatalf("a bus step may not be first; got %v", errs)
		}
	})
}

// A JSON-saved variable is a structure, and a `where` value is text the message must hold.
func TestValidate_BusStepJSONSavedValueInWhereRefused(t *testing.T) {
	two := `{"steps":[{"type":"http","name":"mk","method":"POST","url":"http://sut.invalid/o","save":{"oid":{"json":"id"}}},` +
		`{"type":"bus","name":"b",` + busBinding + `,"where":{"order":"${saved.oid}"}}]}`
	errs := busErrs(two, "### Runnable\n- step mk: status=200\n- step b: body has count >= 1\n")
	if !find(errs, "the bus step's `where` value of \"order\" uses ${saved.oid}, which was saved with the {\"json\": …} form") {
		t.Fatalf("a JSON-saved variable in where must be refused by name; got %v", errs)
	}
}

func TestValidate_BusStepNoClaimRefused(t *testing.T) {
	errs := busErrs(busChainTrig(busBinding), "### Runnable\n- step go: status=202\n")
	if !find(errs, `step "b" has no claim`) || !find(errs, "`bus` step needs at least one") || !find(errs, "- step b: body has count >= 1") {
		t.Fatalf("a bus step with no claim must be refused with the minimum claim named; got %v", errs)
	}
}

// T3 (the scenario half): the shape rules are ONE function for both doors. What Validate refuses, BusStepProblems says, and
// the executor's preflight asks BusStepProblems (argus.TestBusPreflight_RefusesWhatValidateRefuses).
func TestBusStepProblems_IsTheSentenceValidateUses(t *testing.T) {
	st := ChainStep{Type: "bus", Name: "b", Exchange: "orders", RoutingKey: "#"}
	probs := BusStepProblems(st, nil)
	if len(probs) != 1 || !strings.Contains(probs[0], "binds the whole exchange") {
		t.Fatalf("got %v", probs)
	}
	errs := busErrs(busChainTrig(`"exchange":"orders","routing_key":"#"`), busOKExpect)
	if !find(errs, probs[0]) {
		t.Fatalf("Validate must carry the same sentence %q; got %v", probs[0], errs)
	}
	claimsErr := func() error { _, err := BusStepClaims([]string{"body has count == 0"}); return err }()
	if claimsErr == nil {
		t.Fatal("BusStepClaims must refuse an absence without its anchor")
	}
	errs = busErrs(busChainTrig(busBinding), "### Runnable\n- step go: status=202\n- step b: body has count == 0\n")
	if !find(errs, claimsErr.Error()) {
		t.Fatalf("Validate must carry the claim sentence %q; got %v", claimsErr, errs)
	}
}

// T5: the claims.
func TestBusStepClaims(t *testing.T) {
	cases := []struct {
		name    string
		claims  []string
		wantErr string // "" = accepted
	}{
		{"a status claim is refused by name", []string{"status=200"}, "is a status claim - a bus step has no HTTP status"},
		{"a broker claim is refused by name", []string{"broker accepts"}, "is a broker claim"},
		{"an elapsed claim is refused by name", []string{"elapsed < 5s"}, "is an `elapsed` claim"},
		{"unreachable is refused by name", []string{"unreachable"}, "is an `unreachable` claim"},
		{"prose is refused", []string{"it should work"}, "is not a recognised bus-step assertion"},
		// C2, the anchor rule
		{"count == 0 alone is refused", []string{"body has count == 0"}, "no anchor"},
		{"count < 1 alone is refused", []string{"body has count < 1"}, "no anchor"},
		{"count <= 0 alone is refused", []string{"body has count <= 0"}, "no anchor"},
		{"count == 0 with an anchor is accepted", []string{"body has count == 0", "body has correlated >= 1"}, ""},
		{"an anchor of zero does not anchor", []string{"body has count == 0", "body has correlated >= 0"}, "no anchor"},
		{"an anchor of the wrong field does not anchor", []string{"body has count == 0", "body has seen >= 1"}, "no anchor"},
		{"an anchor that is a saved value does not anchor", []string{"body has count == 0", "body has correlated >= ${saved.n}"}, "no anchor"},
		{"undecodable == 0 holds on silence too", []string{"body has undecodable == 0"}, "no anchor"},
		{"presence alone needs no anchor", []string{"body has count >= 1"}, ""},
		{"a message field needs no anchor", []string{"body has messages.0.body.status equals paid"}, ""},
		// C1, the capped list
		{"every message is refused", []string{"body every messages has body.level equals info"}, "is a claim over every message"},
		{"a negative on a message field is refused", []string{"body has messages.0.body.level not equals error"}, "is a negative claim over the message list"},
		{"a whole-body negative is refused", []string{"body not contains error"}, "is a negative claim over the message list"},
		{"a whole-body negative regex is refused", []string{"body not matching err.*"}, "is a negative claim over the message list"},
		{"an absence of a message field is refused", []string{"body has no messages.0.body.error"}, "is a negative claim over the message list"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := BusStepClaims(c.claims)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("accepted claims were refused: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("want a refusal containing %q, got %v", c.wantErr, err)
			}
		})
	}
	// the refusal of the universal form points the author to the way out
	_, err := BusStepClaims([]string{"body not contains error"})
	if err == nil || !strings.Contains(err.Error(), "`where`") || !strings.Contains(err.Error(), "count == 0") {
		t.Fatalf("the C1 sentence must name `where` and `count == 0`, got %v", err)
	}
}

// T6: BusStepEarlyExit.
func TestBusStepEarlyExit_TruthTable(t *testing.T) {
	cases := []struct {
		name   string
		claims []string
		want   bool
	}{
		{"count >= n", []string{"body has count >= 1"}, true},
		{"count > n", []string{"body has count > 2"}, true},
		{"correlated >= n", []string{"body has correlated >= 1"}, true},
		{"count == n is not monotone", []string{"body has count == 1"}, false},
		{"count <= n is not monotone", []string{"body has count <= 3"}, false},
		{"count < n is not monotone", []string{"body has count < 3"}, false},
		{"absence with its anchor waits", []string{"body has count == 0", "body has correlated >= 1"}, false},
		{"a message field equals", []string{"body has messages.0.body.status equals paid"}, true},
		{"a message field containing", []string{"body has messages.1.body.status containing pa"}, true},
		{"a message field exists", []string{"body has messages.0.body.id"}, true},
		{"a message field matching", []string{"body has messages.0.body.id matching ^o-"}, true},
		{"a whole-body contains is not monotone", []string{"body contains paid"}, false},
		{"a non-index message path", []string{"body has messages.x.body.id"}, false},
		{"one non-monotone claim spoils it", []string{"body has count >= 1", "body has count == 2"}, false},
		{"no claims", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asserts []BodyAssert
			if len(c.claims) > 0 {
				var errs []error
				asserts, errs = ParseBodyAsserts(c.claims)
				if len(errs) > 0 {
					t.Fatal(errs)
				}
			}
			if got := BusStepEarlyExit(asserts); got != c.want {
				t.Fatalf("BusStepEarlyExit(%v) = %v, want %v", c.claims, got, c.want)
			}
		})
	}
}

func TestBusStepWhereReading(t *testing.T) {
	w, err := BusStepWhere(json.RawMessage(`{"level":["error","fatal"],"svc":"orders"}`))
	if err != nil || len(w["level"]) != 2 || len(w["svc"]) != 1 || w["svc"][0] != "orders" {
		t.Fatalf("got %v %v", w, err)
	}
	if w, err := BusStepWhere(nil); w != nil || err != nil {
		t.Fatalf("absent where = %v %v", w, err)
	}
}

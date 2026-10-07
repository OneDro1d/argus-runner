package argus

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/amqpengine"
	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// Dispatcher's holdout for the cid-in-chain-claims fix (withheld from the builder). The builder's own
// env-var test reads Failure.Observed only; the promise is that the value appears NOWHERE a report
// carries, on every step type, and that an UNSET variable is refused the same way (not compared as
// the literal text, which could pass or fail by accident).

func TestHoldout_ClaimEnvValueNeverInTheWholeReport(t *testing.T) {
	const env, sentinel = "ARGUS_HOLDOUT_CLAIM_SECRET", "h0ldout-sentinel-5d21"
	t.Setenv(env, sentinel)
	cases := map[string]struct{ trig, expect string }{
		"amqp consume": {
			`{"steps":[{"type":"amqp","name":"consume","op":"consume","url_env":"MSGBUS_ARGUS_TEST_URL","queue":"q","wait":"1s"}]}`,
			"### Runnable\n- step consume: body contains marker ${" + env + "}\n"},
		"http": {
			`{"steps":[{"type":"http","name":"get","method":"GET","url":"http://127.0.0.1:1/x"}]}`,
			"### Runnable\n- step get: status=200\n- step get: body contains ${" + env + "}\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("marker " + sentinel)}
			withFakeBroker(t, f)
			s := scenario.Parse(chainRunMD("", c.trig, c.expect))
			res := runChainScenario(&config.Config{}, s, "tr-holdout-env", "testkit/ui")
			if res.Status == "passed" {
				t.Fatalf("an env placeholder in a claim must be refused, not resolved (it would have matched): %+v", res.Steps)
			}
			raw, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), sentinel) {
				t.Fatalf("the env var's VALUE reached the report JSON: %s", raw)
			}
			if !strings.Contains(string(raw), "${"+env+"}") {
				t.Fatalf("the refusal must name the placeholder: %s", raw)
			}
		})
	}
}

func TestHoldout_ClaimUnsetEnvIsRefusedNotComparedLiterally(t *testing.T) {
	const env = "ARGUS_HOLDOUT_NEVER_SET_7c"
	trig := `{"steps":[{"type":"amqp","name":"consume","op":"consume","url_env":"MSGBUS_ARGUS_TEST_URL","queue":"q","wait":"1s"}]}`
	expect := "### Runnable\n- step consume: body contains ${" + env + "}\n"
	// The delivered body carries the LITERAL placeholder text: a literal compare would PASS.
	f := &argusFakeBroker{outcome: amqpengine.Outcome{OK: true}, consumeBody: []byte("${" + env + "}")}
	withFakeBroker(t, f)
	s := scenario.Parse(chainRunMD("", trig, expect))
	res := runChainScenario(&config.Config{}, s, "tr-holdout-unset", "testkit/ui")
	if res.Status == "passed" {
		t.Fatalf("an unset env placeholder was compared as literal text and passed: %+v", res.Steps)
	}
	if res.Failure == nil || !strings.Contains(res.Failure.Observed, "${"+env+"}") {
		t.Fatalf("the refusal must name the placeholder: %+v", res.Failure)
	}
}

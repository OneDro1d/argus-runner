package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// The chain `logs` step at the AUTHORING door. Each refusal is asserted by its SENTENCE, because
// the sentence is the interface: the executor's preflight must say the same thing (argus/chain_logs_test.go).

// logsTrig builds a chain TRIGGER with one logs step named "l". fields is the rest of the step (leading comma
// added here), "" for none.
func logsTrig(fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return `{"steps":[{"type":"logs","name":"l"` + fields + `}]}`
}

const logsOKExpect = "### Runnable\n- step l: body has count >= 1\n"

func logsErrs(trigger, expect string) []Error {
	_, errs := Validate(chainMD(trigger, expect))
	return errs
}

func TestValidate_LogsStepWellFormedAccepted(t *testing.T) {
	cases := []struct{ name, trigger, expect string }{
		{"correlation", logsTrig(`"scope":"correlation"`), logsOKExpect},
		{"correlation + contains + matching", logsTrig(`"scope":"correlation","contains":"order ${cid} placed","matching":"level=(warn|error)"`), logsOKExpect},
		{"source with a label and contains", logsTrig(`"scope":"source","labels":{"app":"orders"},"contains":"started"`), logsOKExpect},
		{"source with two labels and matching", logsTrig(`"scope":"source","labels":{"app":"orders","namespace":"prod"},"matching":"ERROR .*"`), logsOKExpect},
		{"a saved value in contains", `{"steps":[{"type":"http","name":"mk","method":"POST","url":"http://sut.invalid/o","save":{"oid":"id"}},` +
			`{"type":"logs","name":"l","scope":"source","labels":{"app":"orders"},"contains":"order ${saved.oid} shipped"}]}`,
			"### Runnable\n- step mk: status=200\n- step l: body has count >= 1\n"},
		{"every option", logsTrig(`"scope":"correlation","window":"1h","settle":"60s","always":true,"save":{"first":"lines.0"},"poll":{"timeout":"10m","interval":"1s"}`), logsOKExpect},
		{"settle zero", logsTrig(`"scope":"correlation","settle":"0s"`), logsOKExpect},
		{"absence", logsTrig(`"scope":"correlation","matching":"level=error"`), "### Runnable\n- step l: body has count == 0\n"},
		{"a line", logsTrig(`"scope":"correlation"`), "### Runnable\n- step l: body has count >= 1\n- step l: body has lines.0 containing placed\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if errs := logsErrs(c.trigger, c.expect); len(errs) != 0 {
				t.Fatalf("a well-formed logs step must be accepted, got %v", errs)
			}
		})
	}
}

func TestValidate_LogsStepNoClaimRefused(t *testing.T) {
	errs := logsErrs(logsTrig(`"scope":"correlation"`), "### Runnable\n")
	if !find(errs, `step "l" has no claim`) || !find(errs, "`logs` step needs at least one") || !find(errs, "- step l: body has count >= 1") {
		t.Fatalf("a logs step with no claim must be refused with the minimum claim named; got %v", errs)
	}
}

func TestValidate_LogsStepShapeRefusals(t *testing.T) {
	cases := []struct{ name, fields, want string }{
		{"no scope", ``, "this logs step declares no `scope`"},
		{"unknown scope", `"scope":"namespace"`, `this logs step has scope "namespace" - it must be "correlation" or "source"`},
		{"labels on correlation", `"scope":"correlation","labels":{"app":"x"}`, `has scope "correlation" and carries ` + "`labels`"},
		{"source with no labels", `"scope":"source","contains":"x"`, `has scope "source" and no ` + "`labels`"},
		{"source with empty labels", `"scope":"source","labels":{},"contains":"x"`, `has scope "source" and no ` + "`labels`"},
		{"source with neither filter", `"scope":"source","labels":{"app":"x"}`, "neither `contains` nor `matching` - a step may not count a whole stream"},
		{"labels not an object", `"scope":"source","labels":["app"],"contains":"x"`, "`labels` must be a JSON object of label name to value"},
		{"label value not a string", `"scope":"source","labels":{"app":3},"contains":"x"`, "`labels` value of \"app\" is not a string"},
		{"label name with a dash", `"scope":"source","labels":{"my-app":"x"},"contains":"x"`, "`labels` name \"my-app\" is not a label name"},
		{"label name starting with a digit", `"scope":"source","labels":{"1app":"x"},"contains":"x"`, "`labels` name \"1app\" is not a label name"},
		{"label name with a quote", `"scope":"source","labels":{"a\"b":"x"},"contains":"x"`, "is not a label name"},
		{"empty label value", `"scope":"source","labels":{"app":""},"contains":"x"`, "`labels` value of \"app\" is empty"},
		{"placeholder in a label", `"scope":"source","labels":{"app":"${APP}"},"contains":"x"`, "`labels` value of \"app\" carries `${…}`"},
		{"regex label is just a literal, not a matcher: the name cannot carry =~", `"scope":"source","labels":{"app=~\"x.*\"":"y"},"contains":"x"`, "is not a label name"},
		{"empty contains", `"scope":"correlation","contains":""`, "`contains` is empty"},
		{"empty matching", `"scope":"correlation","matching":""`, "`matching` is empty"},
		{"matching does not compile", `"scope":"correlation","matching":"(unclosed"`, "`matching` \"(unclosed\" does not compile as an RE2 pattern"},
		{"placeholder in matching", `"scope":"correlation","matching":"${cid}"`, "`matching` carries `${…}`"},
		{"environment variable in contains", `"scope":"correlation","contains":"token ${TOKEN}"`, "`contains` carries `${TOKEN}`, which nothing fills in"},
		{"cid8 in contains", `"scope":"correlation","contains":"${cid8}"`, "`contains` carries `${cid8}`, which nothing fills in"},
		{"unclosed placeholder in contains", `"scope":"correlation","contains":"${cid"`, "`contains` carries `${cid`, which nothing fills in"},
		{"window not a duration", `"scope":"correlation","window":"recently"`, "`window` \"recently\" is not a positive Go duration"},
		{"window zero", `"scope":"correlation","window":"0s"`, "`window` \"0s\" is not a positive Go duration"},
		{"window over 1h", `"scope":"correlation","window":"61m"`, "`window` of 1h1m0s is more than the 1h0m0s bound"},
		{"settle not a duration", `"scope":"correlation","settle":"later"`, "`settle` \"later\" is not a Go duration"},
		{"settle negative", `"scope":"correlation","settle":"-1s"`, "`settle` \"-1s\" is not a Go duration of zero or more"},
		{"settle over 60s", `"scope":"correlation","settle":"61s"`, "`settle` of 1m1s is more than the 1m0s bound"},
		{"poll interval under 1s", `"scope":"correlation","poll":{"timeout":"30s","interval":"500ms"}`, "`poll.interval` \"500ms\", which is under 1s - a check must not hammer the log store"},
		{"poll timeout over 10m", `"scope":"correlation","poll":{"timeout":"11m","interval":"5s"}`, "`poll.timeout` of 11m0s, which is more than the 10m bound"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := logsErrs(logsTrig(c.fields), logsOKExpect)
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
		})
	}
}

func TestValidate_LogsStepStrayFieldsRefusedByName(t *testing.T) {
	for _, k := range []string{`"method":"GET"`, `"url":"http://loki.invalid"`, `"query":"{app=\"x\"}"`, `"target":"loki"`, `"timeout":"5s"`,
		`"tool":"t"`, `"max_rows":5`, `"params":["1"]`, `"server_url":"http://loki.invalid"`, `"loki":"http://loki.invalid"`} {
		errs := logsErrs(logsTrig(`"scope":"correlation",`+k), logsOKExpect)
		name := strings.Trim(strings.SplitN(k, ":", 2)[0], `"`)
		if !find(errs, `this logs step carries the field "`+name+`", which a logs step does not accept`) {
			t.Errorf("the field %s on a logs step must be refused by name; got %v", k, errs)
		}
	}
}

func TestValidate_LogsStepOtherStepKindsClaimsRefusedByName(t *testing.T) {
	cases := []struct{ claim, want string }{
		{"status=200", "is a status claim - a logs step has no HTTP status"},
		{"elapsed < 500ms", "`elapsed` is a claim for an http step only"},
		{"unreachable", "`unreachable` is a claim for an http step only"},
		{"broker accepts", "its claim \"broker accepts\" is a broker claim"},
		{"result.isError == false", "is not a recognised logs-step assertion"},
	}
	for _, c := range cases {
		t.Run(c.claim, func(t *testing.T) {
			errs := logsErrs(logsTrig(`"scope":"correlation"`), "### Runnable\n- step l: body has count >= 1\n- step l: "+c.claim+"\n")
			if !find(errs, c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, errs)
			}
		})
	}
}

// A JSON-saved variable is a structure, and `contains` is text the line must hold.
func TestValidate_LogsStepJSONSavedValueInContainsRefused(t *testing.T) {
	two := `{"steps":[{"type":"http","name":"mk","method":"POST","url":"http://sut.invalid/o","save":{"oid":{"json":"id"}}},` +
		`{"type":"logs","name":"l","scope":"correlation","contains":"order ${saved.oid}"}]}`
	errs := logsErrs(two, "### Runnable\n- step mk: status=200\n- step l: body has count >= 1\n")
	if !find(errs, "the logs step's `contains` uses ${saved.oid}, which was saved with the {\"json\": …} form") {
		t.Fatalf("a JSON-saved variable in contains must be refused by name; got %v", errs)
	}
}

// The shape rules are ONE function for both doors: what Validate refuses, LogsStepProblems says, and the executor's
// preflight asks LogsStepProblems.
func TestLogsStepProblems_IsTheSentenceValidateUses(t *testing.T) {
	st := ChainStep{Type: "logs", Name: "l", Scope: "source", Contains: "x", Labels: json.RawMessage(`{"app":""}`)}
	probs := LogsStepProblems(st, nil)
	if len(probs) != 1 || !strings.Contains(probs[0], "`labels` value of \"app\" is empty") {
		t.Fatalf("got %v", probs)
	}
	errs := logsErrs(logsTrig(`"scope":"source","contains":"x","labels":{"app":""}`), logsOKExpect)
	if !find(errs, probs[0]) {
		t.Fatalf("Validate must carry the same sentence %q; got %v", probs[0], errs)
	}
}

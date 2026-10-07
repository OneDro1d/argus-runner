package scenario

import (
	"strings"
	"testing"
)

// (F1) — an `http` chain step's claims were never parsed at WRITE time. A malformed
// numeric claim passed the validator and was then refused by the executor before any step ran.
// These tests pin the write-time half; the executor half is pinned in internal/argus (parity) and
// internal/chain (run time).

const httpSaveTrigger = `{"steps":[
  {"type":"http","name":"first","method":"GET","url":"http://x/a","save":{"n":"count"}},
  {"type":"http","name":"second","method":"GET","url":"http://x/b"}
]}`

func httpSavedMD(second string) string {
	return chainMD(httpSaveTrigger, "### Runnable\n- step first: status=200\n- step second: "+second+"\n")
}

// The F1 bullet, but the variable IS saved by an earlier step: accepted.
func TestValidate_HTTPStepNumericAgainstSavedValueIsAccepted(t *testing.T) {
	for _, op := range []string{">", ">=", "<", "<="} {
		_, errs := Validate(httpSavedMD("body has count " + op + " ${saved.n}"))
		if len(errs) != 0 {
			t.Errorf("op %s: a numeric claim against a saved value must validate; got %v", op, errs)
		}
	}
}

func TestValidate_HTTPStepClaimsAreParsedAtWriteTime(t *testing.T) {
	cases := []struct {
		name, claim, want string
	}{
		{"bad operator", "body has count => 5", "operator"},
		{"non-numeric threshold", "body has count > lots", "not a number"},
		{"threshold with trailing text", "body has count > ${saved.n}0", "not a number"},
		{"non-whole saved threshold", "body has count > 1${saved.n}", "not a number"},
		{"unrecognised claim", "result.isError == false", "not a recognised http-step assertion"},
		{"uncompilable regex", "body has count matching (", "does not compile"},
		{"truncated body claim", "body has count containing", "looks like a body assertion"},
	}
	for _, c := range cases {
		_, errs := Validate(httpSavedMD(c.claim))
		if !find(errs, c.want) {
			t.Errorf("%s: want a refusal containing %q at write time; got %v", c.name, c.want, errs)
		}
		if !find(errs, `step "second"`) {
			t.Errorf("%s: the refusal must name the step; got %v", c.name, errs)
		}
	}
}

// P2: a variable no EARLIER step saves is refused at write time, by name — including one that is
// saved only by a LATER step, and on every step type that can carry the claim.
func TestValidate_NumericAgainstAVariableNobodySavedIsRefused(t *testing.T) {
	trig := `{"steps":[
  {"type":"http","name":"first","method":"GET","url":"http://x/a"},
  {"type":"http","name":"second","method":"GET","url":"http://x/b","save":{"late":"count"}}
]}`
	md := chainMD(trig, "### Runnable\n- step first: body has count > ${saved.late}\n- step second: status=200\n")
	_, errs := Validate(md)
	if !find(errs, "no earlier step saves") || !find(errs, "late") {
		t.Fatalf("a variable saved only by a later step must be refused by name; got %v", errs)
	}

	mcpTrig := `{"steps":[
  {"type":"mcp","name":"a","tool":"t","args":{}},
  {"type":"mcp","name":"b","tool":"t","args":{}}
]}`
	md = chainMD(mcpTrig, "### Runnable\n- step a: result.isError == false\n- step b: body has count >= ${saved.never}\n")
	_, errs = Validate(md)
	if !find(errs, "no earlier step saves") || !find(errs, "never") {
		t.Fatalf("an mcp step's numeric claim against an unsaved variable must be refused by name; got %v", errs)
	}
}

func TestValidate_NumericAgainstSavedValueOnAnAMQPConsumeStepIsAccepted(t *testing.T) {
	trig := `{"steps":[
  {"type":"http","name":"first","method":"GET","url":"http://x/a","save":{"n":"count"}},
  {"type":"amqp","name":"take","op":"consume","url_env":"AMQP_URL","queue":"q"}
]}`
	md := chainMD(trig, "### Runnable\n- step first: status=200\n- step take: broker accepts\n- step take: body has count > ${saved.n}\n")
	_, errs := Validate(md)
	if len(errs) != 0 {
		t.Fatalf("an amqp consume step may compare against a saved value; got %v", errs)
	}
}

func TestParseBodyAsserts_SavedThresholdIsKeptAsWritten(t *testing.T) {
	got, errs := ParseBodyAsserts([]string{"body has count > ${saved.n}"})
	if len(errs) != 0 || len(got) != 1 {
		t.Fatalf("got %v %v", got, errs)
	}
	if got[0].Op != BodyGT || got[0].Value != "${saved.n}" || got[0].Field != "count" {
		t.Errorf("the assertion must carry the placeholder verbatim to be bound at run time: %+v", got[0])
	}
	_, errs = ParseBodyAsserts([]string{"body has count == ${saved.n}"})
	if len(errs) == 0 {
		t.Error("`==` stays unsupported")
	}
}

// ── P4 — `save` from a text body: {"regex": "..."} ─────────────────────────────────────────────────

func TestSaveSpec_WireShapes(t *testing.T) {
	steps, err := ParseChainSteps(`{"steps":[{"type":"http","name":"s","save":{"a":"x.y","b":{"regex":"total (\\d+)"}}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	sv := steps[0].Save
	if sv["a"].Path != "x.y" || sv["a"].Regex != "" {
		t.Errorf("the string form must be unchanged: %+v", sv["a"])
	}
	if sv["b"].Regex != `total (\d+)` || sv["b"].Path != "" {
		t.Errorf("the regex form: %+v", sv["b"])
	}
}

func saveMD(save string) string {
	trig := `{"steps":[{"type":"http","name":"metrics","method":"GET","url":"http://x/m","save":` + save + `}]}`
	return chainMD(trig, "### Runnable\n- step metrics: status=200\n")
}

func TestValidate_RegexSave(t *testing.T) {
	if _, errs := Validate(saveMD(`{"n":{"regex":"deduped_total (\\d+)"}}`)); len(errs) != 0 {
		t.Fatalf("a one-group regex save must validate; got %v", errs)
	}
	if _, errs := Validate(saveMD(`{"n":"count"}`)); len(errs) != 0 {
		t.Fatalf("the string form must still validate; got %v", errs)
	}
	cases := []struct{ name, save, want string }{
		{"does not compile", `{"n":{"regex":"deduped_total ("}}`, "does not compile"},
		{"zero groups", `{"n":{"regex":"deduped_total \\d+"}}`, "0 capture group"},
		{"two groups", `{"n":{"regex":"(deduped)_total (\\d+)"}}`, "2 capture group"},
		{"unknown key", `{"n":{"regexp":"x (\\d+)"}}`, "save for variable \"n\""},
		{"not a string or object", `{"n":7}`, "save for variable \"n\""},
		{"regex not a string", `{"n":{"regex":7}}`, "save for variable \"n\""},
	}
	for _, c := range cases {
		_, errs := Validate(saveMD(c.save))
		if !find(errs, c.want) {
			t.Errorf("%s: want a refusal containing %q; got %v", c.name, c.want, errs)
		}
		if !find(errs, `step "metrics"`) {
			t.Errorf("%s: the refusal must name the step; got %v", c.name, errs)
		}
	}
}

// the one place a save is judged: the same text for the validator and the executor's second door.
func TestSaveProblem_Shared(t *testing.T) {
	if why := SaveProblem("n", SaveSpec{Regex: `(a)(b)`}); !strings.Contains(why, "2 capture group") {
		t.Errorf("got %q", why)
	}
	if why := SaveProblem("n", SaveSpec{Path: "a.b"}); why != "" {
		t.Errorf("a path save is fine: %q", why)
	}
}

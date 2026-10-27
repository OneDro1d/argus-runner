package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// `save` can keep a JSON value AS JSON: {"json": "<path>"} ("$" = the whole response).
// The grammar and the authoring refusals live here; the evaluator and the binding are pinned in
// internal/chain, and the executor's second door in internal/argus.

func TestSaveSpec_JSONForm_ThreeShapes(t *testing.T) {
	steps, err := ParseChainSteps(`{"steps":[{"type":"http","name":"s","save":{
	  "a":"x.y","b":{"regex":"total (\\d+)"},"c":{"json":"items.0"},"d":{"json":"$"}}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	sv := steps[0].Save
	if sv["a"].Path != "x.y" || sv["a"].Regex != "" || sv["a"].JSON || sv["a"].Invalid != "" {
		t.Errorf("the string form must be unchanged: %+v", sv["a"])
	}
	if sv["b"].Regex != `total (\d+)` || sv["b"].JSON || sv["b"].Invalid != "" {
		t.Errorf("the regex form must be unchanged: %+v", sv["b"])
	}
	if !sv["c"].JSON || sv["c"].Path != "items.0" || sv["c"].Invalid != "" {
		t.Errorf("the json form: %+v", sv["c"])
	}
	if !sv["d"].JSON || sv["d"].Path != "$" {
		t.Errorf("the json form with $: %+v", sv["d"])
	}
}

func TestSaveSpec_JSONForm_EveryIllegalObjectIsInvalidAndNamesTheThreeShapes(t *testing.T) {
	for name, wire := range map[string]string{
		"two keys":         `{"json":"a","regex":"(x)"}`,
		"unknown key":      `{"jsn":"a"}`,
		"json not string":  `{"json":7}`,
		"json an object":   `{"json":{"a":1}}`,
		"regex not string": `{"regex":7}`,
		"empty object":     `{}`,
		"a number":         `7`,
		"an array":         `["a"]`,
	} {
		var sp SaveSpec
		if err := json.Unmarshal([]byte(wire), &sp); err != nil {
			t.Fatalf("%s: decoding must not fail the whole step: %v", name, err)
		}
		if sp.Invalid == "" {
			t.Errorf("%s: want Invalid, got %+v", name, sp)
			continue
		}
		for _, shape := range []string{"JSON path string", `"json"`, `"regex"`} {
			if !strings.Contains(sp.Invalid, shape) {
				t.Errorf("%s: the sentence must name the legal shape %q: %s", name, shape, sp.Invalid)
			}
		}
		if why := SaveProblem("v", sp); !strings.Contains(why, `save for variable "v"`) {
			t.Errorf("%s: SaveProblem must name the variable: %q", name, why)
		}
	}
}

func TestSaveSpec_JSONForm_EmptyPathIsRefusedByName(t *testing.T) {
	var sp SaveSpec
	if err := json.Unmarshal([]byte(`{"json":""}`), &sp); err != nil || !sp.JSON || sp.Invalid != "" {
		t.Fatalf("an empty json path still decodes as the json form: %+v %v", sp, err)
	}
	// `{"json": null}` reads as an empty path too (a JSON null decodes into a string as ""): refused by name.
	var nul SaveSpec
	if err := json.Unmarshal([]byte(`{"json":null}`), &nul); err != nil || SaveProblem("ids", nul) == "" {
		t.Errorf("{\"json\": null} must be refused by name: %+v %v", nul, err)
	}
	if why := SaveProblem("ids", sp); !strings.Contains(why, `"ids"`) || !strings.Contains(why, "empty json path") {
		t.Errorf("got %q", why)
	}
	if why := SaveProblem("ids", SaveSpec{Path: "items", JSON: true}); why != "" {
		t.Errorf("a json path is fine: %q", why)
	}
	if why := SaveProblem("ids", SaveSpec{Path: "$", JSON: true}); why != "" {
		t.Errorf("$ is fine: %q", why)
	}
}

func TestSaveSpec_JSONForm_RoundTripsTheWire(t *testing.T) {
	for _, wire := range []string{`{"json":"a.b"}`, `{"json":"$"}`, `"a.b"`, `{"regex":"(x)"}`} {
		var sp SaveSpec
		if err := json.Unmarshal([]byte(wire), &sp); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(sp)
		if err != nil || string(out) != wire {
			t.Errorf("%s came back as %s (%v)", wire, out, err)
		}
	}
}

// ── authoring refusals: the validator (door one) ────────────────────────────────────────────────────

const jsonSaveTrigger = `{"steps":[
  {"type":"http","name":"read","method":"GET","url":"http://x/a","save":{"doc":{"json":"doc"},"id":"doc.id"}},
  {"type":"http","name":"use","method":"POST","url":"http://x/b%s","headers":{"X-Doc":"%s"},"body":{"doc":"${saved.doc}"}}
]}`

func jsonSaveMD(urlSuffix, header, claim string) string {
	trig := strings.Replace(strings.Replace(jsonSaveTrigger, "%s", urlSuffix, 1), "%s", header, 1)
	return chainMD(trig, "### Runnable\n- step read: status=200\n- step use: "+claim+"\n")
}

func TestValidate_JSONSavedVariable_BodyIsLegal(t *testing.T) {
	if _, errs := Validate(jsonSaveMD("", "${saved.id}", "status=200")); len(errs) != 0 {
		t.Fatalf("a JSON-saved variable in a body (and a text one in a header) must validate; got %v", errs)
	}
}

func TestValidate_JSONSavedVariable_RefusedInUrlHeaderAndClaim(t *testing.T) {
	for _, c := range []struct{ name, url, header, claim, where string }{
		{"url", "/${saved.doc}", "${saved.id}", "status=200", "the url"},
		{"header", "", "${saved.doc}", "status=200", `the header "X-Doc"`},
		{"claim", "", "${saved.id}", "body has name containing ${saved.doc}", "a claim (body has name containing ${saved.doc})"},
		{"numeric claim", "", "${saved.id}", "body has n > ${saved.doc}", "a claim (body has n > ${saved.doc})"},
	} {
		_, errs := Validate(jsonSaveMD(c.url, c.header, c.claim))
		want := SavedJSONPlacementSentence(c.where, "doc")
		if !find(errs, want) {
			t.Errorf("%s: want the shared sentence %q; got %v", c.name, want, errs)
		}
		if !find(errs, `step "use"`) || !find(errs, "only be placed in a body or args") ||
			!find(errs, "save a scalar with the string form") {
			t.Errorf("%s: the refusal must name the step and say what to do; got %v", c.name, errs)
		}
	}
}

func TestValidate_SavedVariableKindIsOneThing(t *testing.T) {
	trig := `{"steps":[
  {"type":"http","name":"a","method":"GET","url":"http://x/a","save":{"v":"id"}},
  {"type":"http","name":"b","method":"GET","url":"http://x/b","save":{"v":{"json":"doc"}}}
]}`
	_, errs := Validate(chainMD(trig, "### Runnable\n- step a: status=200\n- step b: status=200\n"))
	if !find(errs, `variable "v" is saved as text here and with the {"json": …} form in step "b"`) || !find(errs, `step "a"`) {
		t.Errorf("want the mixed-kind refusal naming the variable and both steps; got %v", errs)
	}
}

// SavedJSONProblems is the ONE function both doors call: its output is what the executor's preflight prints.
func TestSavedJSONProblems_OneFunctionOneSentence(t *testing.T) {
	steps := []ChainStep{
		{Type: "http", Name: "read", Save: map[string]SaveSpec{"doc": {Path: "doc", JSON: true}}},
		{Type: "http", Name: "use", URL: "http://x/${saved.doc}/${saved.doc}"},
	}
	got := SavedJSONProblems(steps, nil)
	if len(got) != 1 || got[0].Step != "use" || got[0].Why != SavedJSONPlacementSentence("the url", "doc") {
		t.Fatalf("one refusal per variable per place, with the shared sentence: %+v", got)
	}
	if got := SavedJSONProblems(steps[:1], map[string][]string{"read": {"status=200"}}); len(got) != 0 {
		t.Errorf("no reference, no refusal: %+v", got)
	}
}

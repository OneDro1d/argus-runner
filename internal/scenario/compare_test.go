package scenario

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
)

// ARGUS-CMP-2 / the ninth, optional section `## COMPARE`.

func withCompare(md, body string) string {
	return strings.Replace(md, "## TIMEOUT", "## COMPARE\n"+body+"\n\n## TIMEOUT", 1)
}

const goodCompare = "- **Reference**: measured\n" +
	"- **Output**: status, body, header:Content-Type\n" +
	"- **Mask**: $.id; $.items[*].createdAt\n" +
	"- **Unordered**: $.items\n" +
	"- **Repeats**: 5\n" +
	"- **Agreement**: 100%"

func errText(errs []Error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.String() + "\n")
	}
	return b.String()
}

func TestCompare_SectionIsDefinedAndOptional(t *testing.T) {
	if !KnownSection("COMPARE") {
		t.Fatal("COMPARE must be a defined section")
	}
	names := SectionNames()
	if len(names) != 9 || names[7] != "LOAD" || names[8] != "COMPARE" {
		t.Fatalf("sections = %v: COMPARE comes right after LOAD", names)
	}
	for _, r := range RequiredSections() {
		if r == "COMPARE" {
			t.Fatal("COMPARE is optional")
		}
	}
}

func TestCompare_AbsentSectionParsesExactlyAsBefore(t *testing.T) {
	s := Parse(validMD())
	if s.Compare != nil || s.CompareDeclared || len(s.CompareProblems()) != 0 {
		t.Fatalf("no ## COMPARE: %+v", s)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "compare") {
		t.Fatalf("Scenario JSON must not mention compare when absent: %s", b)
	}
	if _, ok := s.AsParsedMap()["compare"]; ok {
		t.Fatal("AsParsedMap must not emit compare when absent")
	}
	if _, errs := Validate(validMD()); len(errs) != 0 {
		t.Fatalf("existing fixture must stay valid: %v", errs)
	}
}

func TestCompare_ParsesIntoRules(t *testing.T) {
	md := withCompare(validMD(), goodCompare)
	s := Parse(md)
	if !s.CompareDeclared || s.Compare == nil {
		t.Fatalf("declared=%v compare=%v problems=%v", s.CompareDeclared, s.Compare, s.CompareProblems())
	}
	r := s.Compare
	if r.Reference != compare.RefMeasured || r.Repeats != 5 || r.Agreement != 100 || !r.Output.Body || len(r.Output.Headers) != 1 {
		t.Fatalf("rules = %+v", r)
	}
	if !reflect.DeepEqual(r.Mask, []string{"$.id", "$.items[*].createdAt"}) {
		t.Fatalf("mask = %v", r.Mask)
	}
	if _, errs := Validate(md); len(errs) != 0 {
		t.Fatalf("a valid section must validate clean: %s", errText(errs))
	}
	if _, ok := s.AsParsedMap()["compare"]; !ok {
		t.Fatal("AsParsedMap emits compare when declared (the schema declares the property)")
	}
}

func TestCompare_HTMLCommentsAndBlankLinesAreTolerated(t *testing.T) {
	body := "- **Reference**: measured            <!-- measured | fixed | property   (required) -->\n" +
		"<!-- a whole-line comment -->\n\n" +
		"- **Repeats**: 3  <!-- slice 2 -->\n" +
		"<!--\n multi-line\n comment -->\n" +
		"- **Mask**: $.id <!-- ignore -->"
	md := withCompare(validMD(), body)
	if _, errs := Validate(md); len(errs) != 0 {
		t.Fatalf("%s", errText(errs))
	}
	s := Parse(md)
	if s.Compare == nil || s.Compare.Repeats != 3 || len(s.Compare.Mask) != 1 {
		t.Fatalf("%+v", s.Compare)
	}
}

func TestCompare_ProblemsAreLineLevelAndNameTheKey(t *testing.T) {
	cases := []struct {
		name, body, want string
		line             int // 0 = do not check
	}{
		{"unknown key with suggestion", "- **Reference**: measured\n- **Repeat**: 3", "Did you mean `**Repeats**`", 0},
		{"unknown key", "- **Reference**: measured\n- **Colour**: red", "unknown key **Colour**", 0},
		{"duplicate key", "- **Reference**: measured\n- **Repeats**: 2\n- **Repeats**: 3", "twice", 0},
		{"missing reference", "- **Repeats**: 3", "Reference", 0},
		{"bad path", "- **Reference**: measured\n- **Mask**: $.a[?(@.x)]", "$.a[?(@.x)]", 0},
		{"refused header", "- **Reference**: measured\n- **Output**: header:Set-Cookie", "Set-Cookie", 0},
		{"repeats bound", "- **Reference**: measured\n- **Repeats**: 21", "Repeats", 0},
		{"property without agreement", "- **Reference**: property", "Agreement", 0},
		{"stray prose", "- **Reference**: measured\nthis is not a key line", "not a `**Key**: value` line", 0},
	}
	for _, c := range cases {
		md := withCompare(validMD(), c.body)
		_, errs := Validate(md)
		txt := errText(errs)
		if !strings.Contains(txt, c.want) {
			t.Errorf("%s: want %q in:\n%s", c.name, c.want, txt)
		}
		if !strings.Contains(txt, "## COMPARE") {
			t.Errorf("%s: every message names the section:\n%s", c.name, txt)
		}
		s := Parse(md)
		if s.Compare != nil {
			t.Errorf("%s: Parse keeps Compare only when there is no problem", c.name)
		}
		if !s.CompareDeclared || len(s.CompareProblems()) == 0 {
			t.Errorf("%s: the executor must be able to see the problem (declared=%v problems=%v)", c.name, s.CompareDeclared, s.CompareProblems())
		}
	}
	// line numbers point at the offending line, not the heading
	md := withCompare(validMD(), "- **Reference**: measured\n- **Repeats**: 99")
	_, errs := Validate(md)
	want := lineContaining(md, "**Repeats**: 99", 1)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "Repeats") && e.Line == want {
			found = true
		}
	}
	if !found {
		t.Errorf("want an error for Repeats on line %d: %s", want, errText(errs))
	}
}

func TestCompare_ReferenceMeasuredNeedsALayerThatRecords(t *testing.T) {
	// HTTP Ingestion on the http template records (fixture is http-tagged)
	if _, errs := Validate(withCompare(validMD(), "- **Reference**: measured")); len(errs) != 0 {
		t.Fatalf("http layer: %s", errText(errs))
	}
	for _, layer := range []string{"Database State", "Message Flow", "External Delivery", "Web UI"} {
		md := strings.Replace(validMD(), "- **Layer**: HTTP Ingestion", "- **Layer**: "+layer, 1)
		_, errs := Validate(withCompare(md, "- **Reference**: measured"))
		if !strings.Contains(errText(errs), "records no output yet") || !strings.Contains(errText(errs), layer) {
			t.Errorf("%s: want a refusal naming the layer, got:\n%s", layer, errText(errs))
		}
	}
	// fixed and property are judged from the check's own outcome and need no capture
	md := strings.Replace(validMD(), "- **Layer**: HTTP Ingestion", "- **Layer**: Database State", 1)
	for _, body := range []string{"- **Reference**: fixed", "- **Reference**: property\n- **Agreement**: 100%"} {
		for _, e := range commentErrs(Validate(withCompare(md, body))) {
			if strings.Contains(e.Message, "records no output yet") {
				t.Errorf("%q must not need a capture: %s", body, e)
			}
		}
	}
	// a native mcp scenario has no capture either
	mcp := strings.Replace(validMD(), "- **Tags**: http, critical, idempotency", "- **Tags**: mcp, critical", 1)
	_, errs := Validate(withCompare(mcp, "- **Reference**: measured"))
	if !strings.Contains(errText(errs), "records no output yet") {
		t.Errorf("mcp: %s", errText(errs))
	}
}

func commentErrs(_ *Scenario, errs []Error) []Error { return errs }

func TestCompare_ChainNeedsHTTPStepsAndRealStepNames(t *testing.T) {
	httpChain := `{"steps":[{"type":"http","name":"create","method":"POST","url":"http://x/y"},` +
		`{"type":"http","name":"read","method":"GET","url":"http://x/y"},` +
		`{"type":"mcp","name":"tool","tool":"t","args":{}}]}`
	expect := "### Runnable\n- step create: status=201\n- step read: status=200\n- step tool: result.isError == false\n"
	md := func(c string) string { return withCompare(chainMD(httpChain, expect), c) }
	if _, errs := Validate(md("- **Reference**: measured\n- **Steps**: create, read")); len(errs) != 0 {
		t.Fatalf("valid steps: %s", errText(errs))
	}
	_, errs := Validate(md("- **Reference**: measured\n- **Steps**: create, nope"))
	if !strings.Contains(errText(errs), "nope") {
		t.Errorf("a step that does not exist is named: %s", errText(errs))
	}
	_, errs = Validate(md("- **Reference**: measured\n- **Steps**: tool"))
	if !strings.Contains(errText(errs), "tool") || !strings.Contains(errText(errs), "http") {
		t.Errorf("only http steps record: %s", errText(errs))
	}
	mcpOnly := withCompare(chainMD(twoSteps, "### Runnable\n- step create: result.isError == false\n"), "- **Reference**: measured")
	_, errs = Validate(mcpOnly)
	if !strings.Contains(errText(errs), "no `http` step") {
		t.Errorf("a chain with no http step records nothing: %s", errText(errs))
	}
	_, errs = Validate(withCompare(validMD(), "- **Reference**: measured\n- **Steps**: a"))
	if !strings.Contains(errText(errs), "chain") {
		t.Errorf("Steps on a non-chain scenario: %s", errText(errs))
	}
}

func TestCompare_NotWorseThanNeedsLoad(t *testing.T) {
	body := "- **Reference**: measured\n- **Not Worse Than**: p95 20%"
	_, errs := Validate(withCompare(validMD(), body))
	if !strings.Contains(errText(errs), "## LOAD") {
		t.Errorf("Not Worse Than without ## LOAD: %s", errText(errs))
	}
	md := withLoad(withCompare(validMD(), body), validLoadBody)
	if _, errs := Validate(md); len(errs) != 0 {
		t.Errorf("with ## LOAD: %s", errText(errs))
	}
}

func TestCompare_WarningsCarryTheStatisticsRule(t *testing.T) {
	md := withCompare(validMD(), "- **Reference**: measured\n- **Repeats**: 5\n- **Agreement**: 99%")
	w := Warnings(md)
	found := false
	for _, x := range w {
		if strings.Contains(x, "299") && strings.Contains(x, "99%") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", w)
	}
	if _, errs := Validate(md); len(errs) != 0 {
		t.Fatalf("a warning is never an error: %s", errText(errs))
	}
}

// Review F3: the validator and compare.BuildRules refuse a Tolerance under an Unordered path with the
// same message, on the Tolerance line.
func TestCompare_ReviewF3_ToleranceUnderUnorderedIsRefusedWithLine(t *testing.T) {
	body := "- **Reference**: measured\n- **Unordered**: $.items\n- **Tolerance**: $.items[*].p abs 0.1"
	md := withCompare(validMD(), body)
	_, errs := Validate(md)
	want := lineContaining(md, "**Tolerance**: $.items[*].p", 1)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "**Tolerance** $.items[*].p lies at or under the **Unordered** path $.items") && e.Line == want {
			found = true
		}
	}
	if !found {
		t.Errorf("want the refusal on line %d: %s", want, errText(errs))
	}
	_, probs := compare.BuildRules([]compare.RawKV{{Key: "Reference", Value: "measured"}, {Key: "Unordered", Value: "$.items"}, {Key: "Tolerance", Value: "$.items[*].p abs 0.1"}})
	if len(probs) != 1 || !strings.Contains(errText(errs), probs[0].Msg) {
		t.Errorf("BuildRules message %+v is not the validator's message: %s", probs, errText(errs))
	}
}

func TestCompare_SectionNamesNearestSuggestion(t *testing.T) {
	md := strings.Replace(validMD(), "## TIMEOUT", "## COMPARRE\n- **Reference**: measured\n\n## TIMEOUT", 1)
	_, errs := Validate(md)
	if !strings.Contains(errText(errs), "Did you mean `## COMPARE`") {
		t.Errorf("a typo in the new section name is refused with the nearest: %s", errText(errs))
	}
}

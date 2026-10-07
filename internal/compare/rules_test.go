package compare

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func kv(pairs ...string) []RawKV {
	var out []RawKV
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, RawKV{Key: pairs[i], Value: pairs[i+1], Line: 10 + i/2})
	}
	return out
}

func problemsText(ps []Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.Key + ": " + p.Msg + "\n")
	}
	return b.String()
}

func TestBuildRules_Defaults(t *testing.T) {
	r, probs := BuildRules(kv("Reference", "measured"))
	if len(probs) != 0 {
		t.Fatal(problemsText(probs))
	}
	if r.Reference != RefMeasured || !r.Output.Status || !r.Output.Body || len(r.Output.Headers) != 0 {
		t.Errorf("default output must be status+body: %+v", r.Output)
	}
	if r.Repeats != 1 || r.Agreement != 100 {
		t.Errorf("defaults: repeats %d agreement %v, want 1 and 100", r.Repeats, r.Agreement)
	}
	if len(r.Mask) != 0 || len(r.Unordered) != 0 || len(r.Tolerance) != 0 || len(r.NotWorseThan) != 0 || len(r.Steps) != 0 {
		t.Errorf("unexpected non-defaults: %+v", r)
	}
}

func TestBuildRules_FullExample(t *testing.T) {
	r, probs := BuildRules(kv(
		"Reference", "measured",
		"Output", "status, body, header:Content-Type, header:Date",
		"Mask", "$.id; $.items[*].createdAt; header:Date",
		"Unordered", "$.items",
		"Tolerance", "$.total abs 0.01; $.rate rel 0.001",
		"Repeats", "5",
		"Agreement", "100%",
		"Not Worse Than", "p95 20%; error_rate 0.5pp",
		"Steps", "create, read",
	))
	if len(probs) != 0 {
		t.Fatal(problemsText(probs))
	}
	if !reflect.DeepEqual(r.Output.Headers, []string{"content-type", "date"}) {
		t.Errorf("headers = %v", r.Output.Headers)
	}
	if !reflect.DeepEqual(r.Mask, []string{"$.id", "$.items[*].createdAt", "header:date"}) {
		t.Errorf("mask = %v", r.Mask)
	}
	if !reflect.DeepEqual(r.Tolerance, []Tolerance{{Path: "$.rate", Kind: "rel", Value: 0.001}, {Path: "$.total", Kind: "abs", Value: 0.01}}) {
		t.Errorf("tolerance (sorted by path) = %+v", r.Tolerance)
	}
	if r.Repeats != 5 || r.Agreement != 100 {
		t.Errorf("repeats/agreement = %d/%v", r.Repeats, r.Agreement)
	}
	if !reflect.DeepEqual(r.NotWorseThan, []Band{{Metric: "error_rate", Value: 0.5, Unit: "pp"}, {Metric: "p95", Value: 20, Unit: "%"}}) {
		t.Errorf("bands (sorted by metric) = %+v", r.NotWorseThan)
	}
	if !reflect.DeepEqual(r.Steps, []string{"create", "read"}) {
		t.Errorf("steps = %v", r.Steps)
	}
}

func TestBuildRules_ClosedKeyList(t *testing.T) {
	_, probs := BuildRules(kv("Reference", "measured", "Referance", "x"))
	if len(probs) != 1 || !probs[0].UnknownKey || probs[0].Key != "Referance" || probs[0].Line != 11 {
		t.Fatalf("an unknown key must be a problem naming the key and its line: %+v", probs)
	}
	for _, k := range []string{"Reference", "Output", "Mask", "Unordered", "Tolerance", "Repeats", "Agreement", "Not Worse Than", "Steps"} {
		if !contains(Keys(), k) {
			t.Errorf("Keys() lacks %q", k)
		}
	}
	if len(Keys()) != 9 {
		t.Errorf("Keys() = %v: the list is closed at nine", Keys())
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestBuildRules_DuplicateKeyIsAProblem(t *testing.T) {
	_, probs := BuildRules(kv("Reference", "measured", "Repeats", "2", "Repeats", "3"))
	if len(probs) != 1 || !strings.Contains(probs[0].Msg, "twice") || probs[0].Line != 12 {
		t.Fatalf("%+v", probs)
	}
}

func TestBuildRules_ReferenceRequiredAndClosed(t *testing.T) {
	if _, p := BuildRules(nil); len(p) == 0 {
		t.Error("an empty section is a problem: Reference is required")
	}
	if _, p := BuildRules(kv("Repeats", "2")); len(p) == 0 {
		t.Error("Reference is required")
	}
	for _, bad := range []string{"Measured", "other", "", "measured, fixed"} {
		if _, p := BuildRules(kv("Reference", bad)); len(p) == 0 {
			t.Errorf("Reference %q must be refused", bad)
		}
	}
	for _, ok := range []string{"measured", "fixed", "property"} {
		if _, p := BuildRules(kv("Reference", ok, "Agreement", "100%")); len(p) != 0 {
			t.Errorf("Reference %q: %s", ok, problemsText(p))
		}
	}
}

func TestBuildRules_PropertyNeedsAgreement(t *testing.T) {
	_, p := BuildRules(kv("Reference", "property"))
	if len(p) != 1 || p[0].Key != "Agreement" {
		t.Fatalf("property without Agreement: %+v", p)
	}
	r, p := BuildRules(kv("Reference", "fixed"))
	if len(p) != 0 || r.Agreement != 100 {
		t.Fatalf("fixed defaults to 100%%: %+v %+v", r, p)
	}
}

func TestBuildRules_FixedAndPropertyDoNotReadOutputRules(t *testing.T) {
	// DECISION: keys nothing reads are refused, never ignored
	for _, k := range [][2]string{{"Output", "body"}, {"Mask", "$.id"}, {"Unordered", "$.a"}, {"Tolerance", "$.a abs 1"}, {"Steps", "a"}} {
		_, p := BuildRules(kv("Reference", "fixed", k[0], k[1]))
		if len(p) == 0 {
			t.Errorf("Reference fixed with %s must be a problem", k[0])
		}
	}
	if _, p := BuildRules(kv("Reference", "fixed", "Repeats", "3", "Agreement", "90%", "Not Worse Than", "p95 10%")); len(p) != 0 {
		t.Errorf("repeats, agreement and bands are read by fixed: %s", problemsText(p))
	}
}

func TestBuildRules_Bounds(t *testing.T) {
	bad := [][]string{
		{"Repeats", "0"}, {"Repeats", "21"}, {"Repeats", "-1"}, {"Repeats", "two"}, {"Repeats", "1.5"}, {"Repeats", ""},
		{"Agreement", "0%"}, {"Agreement", "0.5%"}, {"Agreement", "100.5%"}, {"Agreement", "99"}, {"Agreement", "abc%"}, {"Agreement", "NaN%"},
		{"Output", "headers"}, {"Output", "status, status"}, {"Output", ""}, {"Output", "header:"}, {"Output", "header:Bad Name"},
		{"Mask", "id"}, {"Mask", "$.a; $.a"}, {"Mask", "$.a[?(@.x)]"}, {"Mask", "header:Date"}, // Date is not in Output
		{"Unordered", "header:x"}, {"Unordered", "$.a; $.a"},
		{"Tolerance", "$.a"}, {"Tolerance", "$.a abs"}, {"Tolerance", "$.a abs -1"}, {"Tolerance", "$.a abs 0"}, {"Tolerance", "$.a mul 1"},
		{"Tolerance", "$.a abs NaN"}, {"Tolerance", "$.a abs Inf"}, {"Tolerance", "header:x abs 1"}, {"Tolerance", "$.a abs 1; $.a rel 0.1"},
		{"Not Worse Than", "p95"}, {"Not Worse Than", "p95 20"}, {"Not Worse Than", "p95 20pp"}, {"Not Worse Than", "error_rate 1%"},
		{"Not Worse Than", "p75 10%"}, {"Not Worse Than", "p95 -1%"}, {"Not Worse Than", "p95 10%; p95 20%"},
		{"Steps", ""}, {"Steps", "a, a"}, {"Steps", "a,,b"},
	}
	for _, b := range bad {
		if _, p := BuildRules(kv("Reference", "measured", b[0], b[1])); len(p) == 0 {
			t.Errorf("%s = %q must be refused", b[0], b[1])
		}
	}
	// too many of each
	var masks, unordered, tols, hdrs []string
	for i := 0; i < MaxMasks+1; i++ {
		masks = append(masks, "$.m"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	for i := 0; i < MaxUnordered+1; i++ {
		unordered = append(unordered, "$.u"+string(rune('a'+i)))
	}
	for i := 0; i < MaxTolerances+1; i++ {
		tols = append(tols, "$.t"+string(rune('a'+i))+" abs 1")
	}
	for i := 0; i < MaxHeaders+1; i++ {
		hdrs = append(hdrs, "header:H"+string(rune('a'+i)))
	}
	over := [][]string{
		{"Mask", strings.Join(masks, "; ")}, {"Unordered", strings.Join(unordered, "; ")},
		{"Tolerance", strings.Join(tols, "; ")}, {"Output", "body, " + strings.Join(hdrs, ", ")},
	}
	for _, o := range over {
		if _, p := BuildRules(kv("Reference", "measured", o[0], o[1])); len(p) == 0 {
			t.Errorf("%s over its cap must be refused", o[0])
		}
	}
	// at the caps
	if _, p := BuildRules(kv("Reference", "measured", "Repeats", "20", "Agreement", "1%")); len(p) != 0 {
		t.Errorf("caps are inclusive: %s", problemsText(p))
	}
	if _, p := BuildRules(kv("Reference", "measured", "Mask", strings.Join(masks[:MaxMasks], "; "))); len(p) != 0 {
		t.Errorf("32 masks are allowed: %s", problemsText(p))
	}
}

func TestBuildRules_PathRulesNeedTheBodyPart(t *testing.T) {
	for _, k := range [][2]string{{"Mask", "$.id"}, {"Unordered", "$.a"}, {"Tolerance", "$.a abs 1"}} {
		_, p := BuildRules(kv("Reference", "measured", "Output", "status", k[0], k[1]))
		if len(p) == 0 {
			t.Errorf("%s with Output lacking body must be refused", k[0])
		}
	}
	if _, p := BuildRules(kv("Reference", "measured", "Output", "status, header:Date", "Mask", "header:Date")); len(p) != 0 {
		t.Errorf("a header mask on a declared header is fine: %s", problemsText(p))
	}
}

func TestBuildRules_HeaderOutputs(t *testing.T) {
	var hdrs []string
	for i := 0; i < MaxHeaders; i++ {
		hdrs = append(hdrs, "header:H"+string(rune('a'+i)))
	}
	if _, p := BuildRules(kv("Reference", "measured", "Output", strings.Join(hdrs, ", "))); len(p) != 0 {
		t.Errorf("8 headers are allowed: %s", problemsText(p))
	}
	_, p := BuildRules(kv("Reference", "measured", "Output", "header:Content-Type, header:content-type"))
	if len(p) == 0 {
		t.Error("the same header twice (case-folded) must be refused")
	}
}

func TestBuildRules_CommentsAndWhitespace(t *testing.T) {
	r, p := BuildRules(kv("Reference", "  measured  "))
	if len(p) != 0 || r.Reference != RefMeasured {
		t.Fatalf("%+v %+v", r, p)
	}
}

func TestRulesCanonicalJSON_IsStableAndSorted(t *testing.T) {
	a, _ := BuildRules(kv("Reference", "measured", "Mask", "$.b; $.a", "Steps", "z, y", "Output", "body, status, header:B, header:A"))
	b, _ := BuildRules(kv("Reference", "measured", "Mask", "$.a; $.b", "Steps", "y, z", "Output", "header:a, status, header:b, body", "Repeats", "1", "Agreement", "100%"))
	if string(a.CanonicalJSON()) != string(b.CanonicalJSON()) {
		t.Fatalf("author order and written defaults must not change the encoding:\n%s\n%s", a.CanonicalJSON(), b.CanonicalJSON())
	}
	var back map[string]any
	if err := json.Unmarshal(a.CanonicalJSON(), &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"reference", "output", "mask", "unordered", "tolerance", "repeats", "agreement", "not_worse_than", "steps"} {
		if _, ok := back[k]; !ok {
			t.Errorf("canonical JSON lacks %q (empty lists must encode as [], never be omitted or null): %s", k, a.CanonicalJSON())
		}
	}
	if strings.Contains(string(a.CanonicalJSON()), "null") {
		t.Errorf("no nulls: %s", a.CanonicalJSON())
	}
	c, _ := BuildRules(kv("Reference", "measured", "Mask", "$.a; $.b", "Repeats", "2"))
	if string(c.CanonicalJSON()) == string(b.CanonicalJSON()) {
		t.Error("different rules must encode differently")
	}
}

func TestRulesHash(t *testing.T) {
	r1, _ := BuildRules(kv("Reference", "measured"))
	r2, _ := BuildRules(kv("Reference", "measured", "Mask", "$.id"))
	a := RulesHash([]PathRules{{Path: "b.md", Rules: r2}, {Path: "a.md", Rules: r1}})
	b := RulesHash([]PathRules{{Path: "a.md", Rules: r1}, {Path: "b.md", Rules: r2}})
	if a != b || len(a) != 64 {
		t.Fatalf("hash is over path order, not argument order: %s %s", a, b)
	}
	if RulesHash([]PathRules{{Path: "a.md", Rules: r2}, {Path: "b.md", Rules: r1}}) == a {
		t.Fatal("swapping the rules between paths must change the hash")
	}
	if RulesHash([]PathRules{{Path: "a.md", Rules: r1}}) == RulesHash([]PathRules{{Path: "c.md", Rules: r1}}) {
		t.Fatal("the path is part of what is hashed")
	}
	if RulesHash(nil) == "" {
		t.Fatal("an empty set still has a hash")
	}
}

func TestWarnings_AgreementThatRepeatsCannotSupport(t *testing.T) {
	r, _ := BuildRules(kv("Reference", "measured", "Repeats", "5", "Agreement", "99%"))
	w := Warnings(r)
	if len(w) != 1 || !strings.Contains(w[0], "299") || !strings.Contains(w[0], "99%") {
		t.Fatalf("warnings = %v", w)
	}
	ok, _ := BuildRules(kv("Reference", "measured", "Repeats", "20", "Agreement", "100%"))
	if len(Warnings(ok)) != 0 {
		t.Errorf("100%% is the plain every-run rule and carries no warning: %v", Warnings(ok))
	}
	enough, _ := BuildRules(kv("Reference", "measured", "Repeats", "20", "Agreement", "90%"))
	if len(Warnings(enough)) != 1 {
		t.Errorf("20 runs cannot support 90%% (29 needed): %v", Warnings(enough))
	}
}

func TestFloors(t *testing.T) {
	e1, _ := BuildRules(kv("Reference", "measured", "Mask", "$.id"))
	if e1.Floor() != FloorE1 {
		t.Errorf("floor = %q, want E1", e1.Floor())
	}
	tol, _ := BuildRules(kv("Reference", "measured", "Tolerance", "$.a abs 1"))
	if tol.Floor() != FloorE2 {
		t.Errorf("tolerance needs E2, got %q", tol.Floor())
	}
	nwt, _ := BuildRules(kv("Reference", "measured", "Not Worse Than", "p95 10%"))
	if nwt.Floor() != FloorE2 {
		t.Errorf("Not Worse Than needs E2, got %q", nwt.Floor())
	}
	if KeyFloors["Tolerance"] != FloorE2 || KeyFloors["Not Worse Than"] != FloorE2 || KeyFloors["Reference"] != FloorE1 || KeyFloors["CompareTarget"] != FloorE2 {
		t.Errorf("KeyFloors = %v", KeyFloors)
	}
}

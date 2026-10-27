package mcp

import (
	"strings"
	"testing"
)

// The judge's table for every new op. Each case names the answer, the claim and whether
// the claim must be a MISS.

type evalCase struct {
	name string
	text string
	a    BodyAssert
	miss bool
}

func runEval(t *testing.T, cases []evalCase) {
	t.Helper()
	for _, c := range cases {
		if got := bodyAssertMiss(c.text, c.a); got != c.miss {
			t.Errorf("%s: miss=%v, want %v (claim %+v on %q)", c.name, got, c.miss, c.a, c.text)
		}
		// the three entry points must agree on whether the claim held
		m, _ := BodyAssertsMissReason(c.text, []BodyAssert{c.a})
		o, _ := BodyAssertObservation(c.text, c.a)
		if m != c.miss || o != c.miss {
			t.Errorf("%s: BodyAssertsMissReason=%v BodyAssertObservation=%v, want %v", c.name, m, o, c.miss)
		}
	}
}

func eq(f, v string) BodyAssert { return BodyAssert{Field: f, Op: BodyEQOp, Value: v} }
func within(f, tol, ref string) BodyAssert {
	return BodyAssert{Field: f, Op: BodyWithinOp, Tol: tol, Value: ref}
}

func TestJudge_NumericEquality_AcrossSpellings(t *testing.T) {
	runEval(t, []evalCase{
		{"1 == 1", `{"n":1}`, eq("n", "1"), false},
		{"1 == 1.0", `{"n":1}`, eq("n", "1.0"), false},
		{"1 == 1e0", `{"n":1}`, eq("n", "1e0"), false},
		{"1.0 in the answer == 1", `{"n":1.0}`, eq("n", "1"), false},
		{"a numeric string answer", `{"n":"1.50"}`, eq("n", "1.5"), false},
		{"1e2 == 100", `{"n":100}`, eq("n", "1e2"), false},
		{"negative", `{"n":-3.5}`, eq("n", "-3.5"), false},
		{"zero and negative zero", `{"n":0}`, eq("n", "-0"), false},
		{"differs", `{"n":2}`, eq("n", "1"), true},
		{"differs by the last digit", `{"n":0.30000000000000004}`, eq("n", "0.3"), true},
		{"non-numeric observed", `{"n":"abc"}`, eq("n", "1"), true},
		{"hex is not a number", `{"n":"0x1"}`, eq("n", "1"), true},
		{"inf is not a number", `{"n":"inf"}`, eq("n", "1"), true},
		{"absent", `{"m":1}`, eq("n", "1"), true},
		{"not JSON", `n=1`, eq("n", "1"), true},
	})
}

func TestJudge_Within_AbsoluteAndPercent(t *testing.T) {
	runEval(t, []evalCase{
		// absolute: at, just inside, just outside the bound
		{"abs at the bound", `{"n":10.5}`, within("n", "0.5", "10"), false},
		{"abs at the bound, below", `{"n":9.5}`, within("n", "0.5", "10"), false},
		{"abs just inside", `{"n":10.49}`, within("n", "0.5", "10"), false},
		{"abs just outside", `{"n":10.51}`, within("n", "0.5", "10"), true},
		{"abs just outside, below", `{"n":9.49}`, within("n", "0.5", "10"), true},
		// the float trap: 1.3 - 1.0 is 0.30000000000000004 in float64
		{"abs at the bound where float64 rounds up", `{"n":1.3}`, within("n", "0.3", "1.0"), false},
		{"zero tolerance is equality", `{"n":5}`, within("n", "0", "5"), false},
		{"zero tolerance, off by a hair", `{"n":5.001}`, within("n", "0", "5"), true},
		// percent: relative to the reference
		{"pct at the bound", `{"n":102}`, within("n", "2%", "100"), false},
		{"pct just inside", `{"n":101.99}`, within("n", "2%", "100"), false},
		{"pct just outside", `{"n":102.01}`, within("n", "2%", "100"), true},
		{"pct below the bound", `{"n":98}`, within("n", "2%", "100"), false},
		{"pct below, outside", `{"n":97.99}`, within("n", "2%", "100"), true},
		{"fractional pct", `{"n":100.5}`, within("n", "0.5%", "100"), false},
		// negative references: |ref| scales the percentage
		{"negative ref abs inside", `{"n":-10.2}`, within("n", "0.5", "-10"), false},
		{"negative ref abs outside", `{"n":-10.6}`, within("n", "0.5", "-10"), true},
		{"negative ref pct at the bound", `{"n":-102}`, within("n", "2%", "-100"), false},
		{"negative ref pct outside", `{"n":-102.1}`, within("n", "2%", "-100"), true},
		{"the sign matters", `{"n":10}`, within("n", "2%", "-10"), true},
		// reference 0
		{"ref 0 abs inside", `{"n":0.1}`, within("n", "0.5", "0"), false},
		{"ref 0 abs outside", `{"n":0.6}`, within("n", "0.5", "0"), true},
		{"ref 0 pct holds only at exactly 0", `{"n":0}`, within("n", "50%", "0"), false},
		{"ref 0 pct, 0.0001 is not 0", `{"n":0.0001}`, within("n", "50%", "0"), true},
		{"ref 0 pct, negative tiny", `{"n":-0.0001}`, within("n", "99%", "0"), true},
		// non-numeric / absent / not JSON
		{"non-numeric observed", `{"n":"abc"}`, within("n", "1", "1"), true},
		{"absent", `{"m":1}`, within("n", "1", "1"), true},
		{"not JSON", `1`, within("n", "1", "1"), true},
		{"a bad tolerance at run time is a miss, never a pass", `{"n":1}`, within("n", "-1", "1"), true},
	})
}

func TestJudge_Within_NonNumericObservedSaysSo(t *testing.T) {
	miss, nonNumeric := BodyAssertsMissReason(`{"n":"abc"}`, []BodyAssert{within("n", "1", "1")})
	if !miss || !nonNumeric {
		t.Errorf("a non-numeric observed value is a miss with the reason that says so: miss=%v nonNumeric=%v", miss, nonNumeric)
	}
	miss, nonNumeric = BodyAssertsMissReason(`{"n":5}`, []BodyAssert{within("n", "1", "1")})
	if !miss || nonNumeric {
		t.Errorf("an ordinary out-of-tolerance miss is not 'non-numeric': miss=%v nonNumeric=%v", miss, nonNumeric)
	}
}

func notC(f, v string) BodyAssert { return BodyAssert{Field: f, Op: BodyNotContainsOp, Value: v} }

func TestJudge_Negatives_WholeBody(t *testing.T) {
	runEval(t, []evalCase{
		{"not contains: absent word passes", `{"status":"ok"}`, BodyAssert{Op: BodyNotContainsOp, Value: "error"}, false},
		{"not contains: present word misses", `{"status":"error"}`, BodyAssert{Op: BodyNotContainsOp, Value: "error"}, true},
		{"not contains on prose (not JSON) is fine whole-body", `all good here`, BodyAssert{Op: BodyNotContainsOp, Value: "error"}, false},
		{"not matching passes", `{"status":"ok"}`, BodyAssert{Op: BodyNotMatchesOp, Value: `"status":\s*"(error|failed)"`}, false},
		{"not matching misses", `{"status": "failed"}`, BodyAssert{Op: BodyNotMatchesOp, Value: `"status":\s*"(error|failed)"`}, true},
		{"an uncompilable regex cannot be shown NOT to match", `x`, BodyAssert{Op: BodyNotMatchesOp, Value: `(`}, true},
	})
}

func TestJudge_Negatives_FieldScoped_R4Honesty(t *testing.T) {
	runEval(t, []evalCase{
		// present field
		{"not containing: present, absent word", `{"s":"fine"}`, notC("s", "bad"), false},
		{"not containing: present, has the word", `{"s":"bad day"}`, notC("s", "bad"), true},
		{"not equals: present, different", `{"s":"a"}`, BodyAssert{Field: "s", Op: BodyNotEqualsOp, Value: "b"}, false},
		{"not equals: present, equal", `{"s":"b"}`, BodyAssert{Field: "s", Op: BodyNotEqualsOp, Value: "b"}, true},
		{"not matching: present, no match", `{"s":"abc"}`, BodyAssert{Field: "s", Op: BodyNotMatchesOp, Value: `^x`}, false},
		{"not matching: present, match", `{"s":"xyz"}`, BodyAssert{Field: "s", Op: BodyNotMatchesOp, Value: `^x`}, true},
		// ⛔ R4: an ABSENT field is a MISS, not a pass
		{"not containing: ABSENT field is a miss", `{"other":"x"}`, notC("s", "bad"), true},
		{"not equals: ABSENT field is a miss", `{"other":"x"}`, BodyAssert{Field: "s", Op: BodyNotEqualsOp, Value: "b"}, true},
		{"not matching: ABSENT field is a miss", `{"other":"x"}`, BodyAssert{Field: "s", Op: BodyNotMatchesOp, Value: `^x`}, true},
		{"a null field is absent: a miss", `{"s":null}`, notC("s", "bad"), true},
		// ⛔ R4: a non-JSON answer is a MISS
		{"not containing: non-JSON answer is a miss", `status fine`, notC("s", "bad"), true},
		{"not equals: non-JSON answer is a miss", `status fine`, BodyAssert{Field: "s", Op: BodyNotEqualsOp, Value: "b"}, true},
		{"not matching: non-JSON answer is a miss", `status fine`, BodyAssert{Field: "s", Op: BodyNotMatchesOp, Value: `^x`}, true},
		{"nested path", `{"a":{"b":"ok"}}`, notC("a.b", "bad"), false},
	})
}

func TestJudge_HasNo(t *testing.T) {
	no := func(f string) BodyAssert { return BodyAssert{Field: f, Op: BodyAbsentOp} }
	runEval(t, []evalCase{
		{"absent holds", `{"a":1}`, no("b"), false},
		{"null holds", `{"b":null}`, no("b"), false},
		{"nested absent holds", `{"a":{}}`, no("a.b"), false},
		{"present misses", `{"b":1}`, no("b"), true},
		{"present empty string misses", `{"b":""}`, no("b"), true},
		{"present false misses", `{"b":false}`, no("b"), true},
		{"present object misses", `{"b":{}}`, no("b"), true},
		{"non-JSON is a MISS (cannot be evaluated)", `plain text`, no("b"), true},
		{"empty answer is a MISS", ``, no("b"), true},
		{"index out of range is absent", `{"l":[1]}`, no("l.3"), false},
	})
}

func every(arr string, a BodyAssert) BodyAssert { a.Every = arr; return a }

func TestJudge_Every(t *testing.T) {
	rows := `{"rows":[{"status":"ok","n":1},{"status":"ok","n":2},{"status":"ok","n":3}]}`
	runEval(t, []evalCase{
		{"every element has the field", rows, every("rows", BodyAssert{Field: "status", Op: BodyExistsOp}), false},
		{"every element contains", rows, every("rows", BodyAssert{Field: "status", Op: BodyContainsOp, Value: "ok"}), false},
		{"every element not equals", rows, every("rows", BodyAssert{Field: "status", Op: BodyNotEqualsOp, Value: "failed"}), false},
		{"every element ordering", rows, every("rows", BodyAssert{Field: "n", Op: BodyGTEOp, Value: "1"}), false},
		{"every element ==", `{"r":[{"n":1},{"n":1.0}]}`, every("r", eq("n", "1")), false},
		{"every element within", `{"r":[{"n":10},{"n":10.4}]}`, every("r", within("n", "0.5", "10")), false},
		{"one breaking element", `{"rows":[{"status":"ok"},{"status":"failed"},{"status":"ok"}]}`,
			every("rows", BodyAssert{Field: "status", Op: BodyNotEqualsOp, Value: "failed"}), true},
		{"a missing field in one element breaks it", `{"rows":[{"status":"ok"},{"x":1}]}`,
			every("rows", BodyAssert{Field: "status", Op: BodyExistsOp}), true},
		{"every has no: all absent", `{"rows":[{"a":1},{"a":2}]}`, every("rows", BodyAssert{Field: "secret", Op: BodyAbsentOp}), false},
		{"every has no: one has it", `{"rows":[{"a":1},{"secret":"x"}]}`, every("rows", BodyAssert{Field: "secret", Op: BodyAbsentOp}), true},
		// an EMPTY array holds: no element breaks the claim
		{"empty array holds", `{"rows":[]}`, every("rows", BodyAssert{Field: "status", Op: BodyExistsOp}), false},
		{"empty array holds, even for a claim nothing can satisfy", `{"rows":[]}`, every("rows", BodyAssert{Field: "status", Op: BodyContainsOp, Value: "zzz"}), false},
		// a missing path / a non-array is a MISS
		{"missing path", `{"other":[1]}`, every("rows", BodyAssert{Field: ".", Op: BodyExistsOp}), true},
		{"non-array (object)", `{"rows":{"a":1}}`, every("rows", BodyAssert{Field: "a", Op: BodyExistsOp}), true},
		{"non-array (string)", `{"rows":"abc"}`, every("rows", BodyAssert{Field: ".", Op: BodyExistsOp}), true},
		{"null path", `{"rows":null}`, every("rows", BodyAssert{Field: ".", Op: BodyExistsOp}), true},
		{"non-JSON answer", `rows`, every("rows", BodyAssert{Field: ".", Op: BodyExistsOp}), true},
		// scalars with `.`
		{"array of scalars: every element matches", `{"ids":["a1","a2"]}`, every("ids", BodyAssert{Field: ".", Op: BodyMatchesOp, Value: `^a\d$`}), false},
		{"array of scalars: one breaks", `{"ids":["a1","b2"]}`, every("ids", BodyAssert{Field: ".", Op: BodyMatchesOp, Value: `^a\d$`}), true},
		{"array of numbers within", `{"p":[10,10.2,9.9]}`, every("p", within(".", "0.5", "10")), false},
		{"array of numbers, one outside", `{"p":[10,12]}`, every("p", within(".", "0.5", "10")), true},
		{"a null element is absent: a miss", `{"ids":["a",null]}`, every("ids", BodyAssert{Field: ".", Op: BodyNotContainsOp, Value: "z"}), true},
		// `$`: the whole answer is the array
		{"$ whole answer", `[{"n":1},{"n":2}]`, every("$", BodyAssert{Field: "n", Op: BodyGTOp, Value: "0"}), false},
		{"$ whole answer, one breaks", `[{"n":1},{"n":0}]`, every("$", BodyAssert{Field: "n", Op: BodyGTOp, Value: "0"}), true},
		{"$ but the answer is an object", `{"n":1}`, every("$", BodyAssert{Field: "n", Op: BodyExistsOp}), true},
		{"nested array path", `{"a":{"b":[{"k":1}]}}`, every("a.b", BodyAssert{Field: "k", Op: BodyExistsOp}), false},
	})
}

func TestJudge_Every_ObservedNamesTheFirstBreakingElement(t *testing.T) {
	text := `{"rows":[{"status":"ok"},{"status":"failed"},{"status":"failed"}]}`
	a := every("rows", BodyAssert{Field: "status", Op: BodyNotEqualsOp, Value: "failed"})
	miss, observed := BodyAssertObservation(text, a)
	if !miss || observed != "element 1: failed" {
		t.Errorf("want the index of the FIRST breaking element and its value, got miss=%v %q", miss, observed)
	}
	// a missing field in an element reads as absent, with the index
	_, observed = BodyAssertObservation(`{"r":[{"s":"a"},{"x":1}]}`, every("r", BodyAssert{Field: "s", Op: BodyExistsOp}))
	if observed != "element 1: "+FieldAbsentObserved {
		t.Errorf("got %q", observed)
	}
	_, observed = BodyAssertObservation(`{"r":3}`, every("r", BodyAssert{Field: "s", Op: BodyExistsOp}))
	if observed != EveryNotArrayObserved {
		t.Errorf("got %q", observed)
	}
}

func TestJudge_Observed_PerForm(t *testing.T) {
	cases := []struct {
		text, want string
		a          BodyAssert
	}{
		{`{"n":5}`, "5", eq("n", "1")},
		{`{"n":"abc"}`, "abc", within("n", "1", "1")},
		{`{"s":"bad day"}`, "bad day", notC("s", "bad")},
		{`{"b":{"x":1}}`, `{"x":1}`, BodyAssert{Field: "b", Op: BodyAbsentOp}},
		{`prose`, NotJSONObserved, BodyAssert{Field: "b", Op: BodyAbsentOp}},
		{`{"o":1}`, FieldAbsentObserved, notC("s", "bad")},
	}
	for _, c := range cases {
		miss, observed := BodyAssertObservation(c.text, c.a)
		if !miss || observed != c.want {
			t.Errorf("%+v on %q: miss=%v observed=%q, want %q", c.a, c.text, miss, observed, c.want)
		}
	}
}

// A reference or tolerance is never in the observed text: the observed text is the SUT's value only.
func TestJudge_Observed_NeverCarriesTheReferenceOrTolerance(t *testing.T) {
	_, observed := BodyAssertObservation(`{"n":5}`, within("n", "0.123", "98765"))
	if strings.Contains(observed, "98765") || strings.Contains(observed, "0.123") {
		t.Errorf("observed carries the expected value: %q", observed)
	}
}

// The mcp scenario path: result.content text goes through the same judge.
func TestJudge_MCPScenarioUsesTheNewForms(t *testing.T) {
	r := CallResult{Content: []ContentBlock{{Type: "text", Text: `{"items":[{"s":"ok"},{"s":"ok"}],"price":10.2}`}}}
	ok := Judge(r, Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{
		every("items", BodyAssert{Field: "s", Op: BodyNotEqualsOp, Value: "failed"}),
		within("price", "0.5", "10"),
		{Field: "error", Op: BodyAbsentOp},
	}})
	if !ok.Pass {
		t.Fatalf("all three new forms hold: %+v", ok)
	}
	bad := Judge(r, Expect{ErrorPlane: PlaneNone, Body: []BodyAssert{within("price", "0.1", "10")}})
	if bad.Pass || bad.Plane != PlaneBody {
		t.Fatalf("a tolerance miss is a body miss: %+v", bad)
	}
	if strings.Contains(bad.Observed, "0.1") {
		t.Errorf("the verdict's observed text carries the tolerance: %q", bad.Observed)
	}
}

package mcp

import "testing"

// judge_numeric_test.go — item 25(b): the numeric comparison operators (`gt`/`gte`/`lt`/`lte`) at
// the judge level, shared by the chain `http` step and any `mcp` scenario (BodyAssert is one
// grammar, one judge, for both engines — VR10-S2-owner).

func TestBodyAssertsMiss_NumericComparisons(t *testing.T) {
	cases := []struct {
		name string
		op   string
		val  string
		text string
		miss bool
	}{
		{"gt pass", BodyGTOp, "10", `{"n":11}`, false},
		{"gt fail equal", BodyGTOp, "10", `{"n":10}`, true},
		{"gte pass equal", BodyGTEOp, "10", `{"n":10}`, false},
		{"gte fail", BodyGTEOp, "10", `{"n":9}`, true},
		{"lt pass", BodyLTOp, "10", `{"n":9}`, false},
		{"lt fail equal", BodyLTOp, "10", `{"n":10}`, true},
		{"lte pass equal", BodyLTEOp, "10", `{"n":10}`, false},
		{"lte fail", BodyLTEOp, "10", `{"n":10.5}`, true},
		{"decimal threshold", BodyGTOp, "3.14", `{"n":3.15}`, false},
		{"negative observed", BodyLTOp, "0", `{"n":-1}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BodyAssertsMiss(c.text, []BodyAssert{{Field: "n", Op: c.op, Value: c.val}})
			if got != c.miss {
				t.Errorf("miss = %v, want %v (op=%s val=%s text=%s)", got, c.miss, c.op, c.val, c.text)
			}
		})
	}
}

// R4 twin for numerics: a field-scoped numeric assertion that cannot be evaluated (absent field,
// non-JSON answer) is a MISS, never a pass — the same degradation bodyAssertMiss already applies
// to contains/matches.
func TestBodyAssertsMiss_NumericAbsentFieldOrNonJSONIsAMiss(t *testing.T) {
	if !BodyAssertsMiss(`{"other":1}`, []BodyAssert{{Field: "n", Op: BodyGTOp, Value: "10"}}) {
		t.Error("an absent field must MISS a numeric comparison")
	}
	if !BodyAssertsMiss(`not json`, []BodyAssert{{Field: "n", Op: BodyGTOp, Value: "10"}}) {
		t.Error("a non-JSON answer must MISS a numeric comparison")
	}
}

// TestBodyAssertsMissReason_DistinguishesNonNumericFromOrdinaryMiss is the http engine's own hook
// (chain/http.go's judge()): it must be able to tell "the field was not a number" apart from "the
// number just did not satisfy the comparison", without ever learning the threshold.
func TestBodyAssertsMissReason_DistinguishesNonNumericFromOrdinaryMiss(t *testing.T) {
	miss, nonNumeric := BodyAssertsMissReason(`{"n":"fast"}`, []BodyAssert{{Field: "n", Op: BodyGTOp, Value: "10"}})
	if !miss || !nonNumeric {
		t.Errorf("a non-numeric field must miss AND flag nonNumeric: miss=%v nonNumeric=%v", miss, nonNumeric)
	}

	miss, nonNumeric = BodyAssertsMissReason(`{"n":5}`, []BodyAssert{{Field: "n", Op: BodyGTOp, Value: "10"}})
	if !miss || nonNumeric {
		t.Errorf("an ordinary numeric miss must NOT flag nonNumeric: miss=%v nonNumeric=%v", miss, nonNumeric)
	}

	miss, nonNumeric = BodyAssertsMissReason(`{"n":11}`, []BodyAssert{{Field: "n", Op: BodyGTOp, Value: "10"}})
	if miss || nonNumeric {
		t.Errorf("a pass must report miss=false: miss=%v nonNumeric=%v", miss, nonNumeric)
	}
}

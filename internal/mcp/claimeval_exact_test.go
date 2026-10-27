package mcp

import (
	"strings"
	"testing"
)

// `==` and `within` are EXACT, and that has to hold above 2^53, where two different integers are one
// float64. Before decodeJSONExact, `== 1000000000000000000` held on an answer of 1000000000000000001 (a
// false pass) and `== 12345678901234567890` missed on that very number (it had been rounded on the way in).
func TestJudge_ExactOps_KeepEveryDigitAbove2To53(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    BodyAssert
		body string
		miss bool
		obs  string // a miss shows the answer's own digits, never a rounded copy
	}{
		{"one apart is not equal", BodyAssert{Field: "a", Op: BodyEQOp, Value: "1000000000000000000"}, `{"a":1000000000000000001}`, true, "1000000000000000001"},
		{"a 20-digit integer equals itself", BodyAssert{Field: "a", Op: BodyEQOp, Value: "12345678901234567890"}, `{"a":12345678901234567890}`, false, ""},
		{"within 0 is equality", BodyAssert{Field: "a", Op: BodyWithinOp, Value: "9007199254740993", Tol: "0"}, `{"a":9007199254740992}`, true, "9007199254740992"},
		{"within 1 holds one apart", BodyAssert{Field: "a", Op: BodyWithinOp, Value: "9007199254740993", Tol: "1"}, `{"a":9007199254740992}`, false, ""},
		{"every: the breaking element is the second", BodyAssert{Every: "rows", Field: "amt", Op: BodyEQOp, Value: "9007199254740993"},
			`{"rows":[{"amt":9007199254740993},{"amt":9007199254740992}]}`, true, "element 1: 9007199254740992"},
		{"a decimal keeps its trailing digits", BodyAssert{Field: "a", Op: BodyEQOp, Value: "0.1000000000000000000001"}, `{"a":0.1}`, true, "0.1"},
		{"spellings of one value are still equal", BodyAssert{Field: "a", Op: BodyEQOp, Value: "1e0"}, `{"a":1.0}`, false, ""},
		{"an answer with trailing content is not JSON", BodyAssert{Field: "a", Op: BodyEQOp, Value: "1"}, `{"a":1} {"a":1}`, true, FieldAbsentObserved},
	} {
		miss, obs := BodyAssertObservation(tc.body, tc.a)
		if miss != tc.miss {
			t.Errorf("%s: miss=%v, want %v (observed %q)", tc.name, miss, tc.miss, obs)
		}
		if tc.obs != "" && !strings.Contains(obs, tc.obs) {
			t.Errorf("%s: observed %q, want it to carry %q", tc.name, obs, tc.obs)
		}
	}
}

// The older ops keep the float64 decoding they always had: what `equals` sees for a number does not move.
func TestJudge_OlderOps_StillSeeANumberTheOldWay(t *testing.T) {
	if miss, obs := BodyAssertObservation(`{"a":1.0}`, BodyAssert{Field: "a", Op: BodyEqualsOp, Value: "1"}); miss {
		t.Errorf("`equals 1` on 1.0 missed (observed %q): the exact decoder leaked into an older op", obs)
	}
}

package compare

// cmp11_f6_test.go -- ARGUS-CMP-11 fix F6: ValueDiffs / ValueDiffsFor, the numbers behind a tolerant cell that differs, for the author's read.
// The numbers are NOT in Output (so not in result_hash, an anchor payload or any other reader of the pure function).

import (
	"encoding/json"
	"strings"
	"testing"
)

func f6Row(step string, vals ...ToleranceValue) ScenarioOutput {
	r := out("C-1", hx("b"))
	r.Step = step
	r.Values = vals
	return r
}

func TestCMP11_F6_ValueDiffsNamesEachPathOutsideItsToleranceWithBothNumbersAndTheDeclaredTolerance(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Tolerance", "$.total abs 0.01; $.fee rel 0.1")
	ref := []ScenarioOutput{f6Row("", ToleranceValue{Path: "$.total", Rule: 1, Value: 100}, ToleranceValue{Path: "$.fee", Rule: 0, Value: 10}, ToleranceValue{Path: "$.only_ref", Rule: 0, Value: 1})}
	got := []ScenarioOutput{f6Row("", ToleranceValue{Path: "$.total", Rule: 1, Value: 100.5}, ToleranceValue{Path: "$.fee", Rule: 0, Value: 10.5}, ToleranceValue{Path: "$.only_got", Rule: 0, Value: 2})}
	d := ValueDiffs(rules, ref, got)
	// $.fee 10 vs 10.5 is inside rel 10%: not listed. $.total is outside abs 0.01. $.only_ref and $.only_got are present on one side only.
	if len(d) != 3 {
		t.Fatalf("value diffs = %+v, want 3 (total, only_got, only_ref)", d)
	}
	byPath := map[string]ValueDiff{}
	for _, x := range d {
		byPath[x.Path] = x
	}
	tot := byPath["$.total"]
	if tot.Reference == nil || *tot.Reference != 100 || tot.Member == nil || *tot.Member != 100.5 || tot.Tolerance != "abs 0.01" || tot.Declared != "$.total" {
		t.Errorf("$.total = %+v", tot)
	}
	if o := byPath["$.only_ref"]; o.Reference == nil || o.Member != nil {
		t.Errorf("a leaf only the reference recorded must read member null: %+v", o)
	}
	if o := byPath["$.only_got"]; o.Reference != nil || o.Member == nil {
		t.Errorf("a leaf only the member recorded must read reference null: %+v", o)
	}
	if _, listed := byPath["$.fee"]; listed {
		t.Errorf("a value inside its tolerance was listed")
	}
	raw, _ := json.Marshal(tot)
	if !strings.Contains(string(raw), `"reference":100`) || !strings.Contains(string(raw), `"member":100.5`) {
		t.Errorf("wire form %s", raw)
	}
}

func TestCMP11_F6_ValueDiffsAreCappedAt64AndEmptyWhenTheRunsDoNotRecordTheSameRows(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Tolerance", "$.v abs 0.01")
	mk := func(step string, n int, base float64) ScenarioOutput {
		var vs []ToleranceValue
		for i := 0; i < n; i++ {
			vs = append(vs, ToleranceValue{Path: "$.v[" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "]", Rule: 0, Value: base})
		}
		return f6Row(step, vs...)
	}
	ref := []ScenarioOutput{mk("a", 50, 1), mk("b", 50, 1)}
	got := []ScenarioOutput{mk("a", 50, 9), mk("b", 50, 9)}
	if d := ValueDiffs(rules, ref, got); len(d) != MaxValueDiffs {
		t.Errorf("100 differing leaves gave %d entries, want the cap %d", len(d), MaxValueDiffs)
	}
	if d := ValueDiffs(rules, ref, got[:1]); d != nil {
		t.Errorf("runs with different rows gave value diffs %+v: that difference is samples, not values", d)
	}
}

func TestCMP11_F6_ValueDiffsForTakesTheFirstDisagreeingPairAndTheNumbersAreNotInTheOutputOrItsHash(t *testing.T) {
	// the reference is stable (0 and 0.008 agree), the candidate's first run (0.0095) agrees with both and its second (0.5) does not
	in := f4Input(t, "$.total abs 0.01", []float64{0, 0.008}, []float64{0.0095, 0.5})
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if c.State != StateDiffers {
		t.Fatalf("cell = %+v", c)
	}
	d := ValueDiffsFor(in, "C-1", "new", c.VersionKey)
	if len(d) != 1 || *d[0].Member != 0.5 || *d[0].Reference != 0 {
		t.Fatalf("value diffs = %+v, want the pair (reference run r1 = 0, member run s2 = 0.5)", d)
	}
	// nothing of it is in the pure function's output, so none of it is in result_hash or any payload built from it
	for _, needle := range []string{"value_diffs", `"declared"`, `"member":0.5`, `"reference":0,`} {
		if strings.Contains(string(o.JSON()), needle) {
			t.Errorf("Output carries value diffs (%s)", needle)
		}
	}
	if got := ValueDiffsFor(in, "C-1", "old", c.VersionKey); got != nil {
		t.Errorf("the reference's own cell has value diffs: %+v", got)
	}
	if got := ValueDiffsFor(in, "NOPE", "new", c.VersionKey); got != nil {
		t.Errorf("an unknown check has value diffs: %+v", got)
	}
}

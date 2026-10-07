package compare

// cmp11_f4_test.go -- ARGUS-CMP-11 fix F4: under a tolerance the reference is stable only if EVERY PAIR of its usable runs agree, and a
// candidate run agrees only if it agrees with EVERY usable reference run. A tolerance is not transitive, so neither answer may depend on
// which run happens to have the lowest id; `rel` is relative to the smaller magnitude of the two values, so it is symmetric.

import "testing"

func f4Row(id string, v float64) ScenarioOutput {
	r := out(id, hx("b"))
	r.Values = []ToleranceValue{{Path: "$.total", Rule: 0, Value: v}}
	return r
}

func f4Input(t *testing.T, tol string, ref []float64, cand []float64) Input {
	t.Helper()
	rules := mustRules(t, "Reference", "measured", "Tolerance", tol)
	in := Input{SetHash: setH, Checks: []Check{{ID: "C-1", Path: "a/C-1.md", Rules: rules}}, Members: twoMembers()}
	for i, v := range ref {
		in.Runs = append(in.Runs, run("r"+string(rune('1'+i)), "old", "v", "passed", f4Row("C-1", v)))
	}
	for i, v := range cand {
		in.Runs = append(in.Runs, run("s"+string(rune('1'+i)), "new", "v", "passed", f4Row("C-1", v)))
	}
	return in
}

func TestCMP11_F4_TheReferenceIsStableOnlyIfEveryPairOfItsRunsAgreeWhateverTheRunOrder(t *testing.T) {
	// 0 ~ 0.008 and 0.008 ~ 0.016 under abs 0.01, but 0 and 0.016 are 0.016 apart: a tolerance is not transitive
	for _, order := range [][]float64{{0, 0.008, 0.016}, {0.008, 0, 0.016}, {0.016, 0.008, 0}, {0.008, 0.016, 0}} {
		o := Result(f4Input(t, "$.total abs 0.01", order, []float64{0.008}))
		ck := o.Checks[0]
		if ck.Reference.Stable {
			t.Errorf("runs %v: the reference is stable although its first and last runs are 0.016 apart (the verdict depends on run order)", order)
		}
		if got := cellOf(t, o, "C-1", "new").State; got != StateNoise {
			t.Errorf("runs %v: the candidate cell reads %s, want noise (the reference disagrees with itself)", order, got)
		}
	}
	// the same three values all within 0.01 of each other are stable in every order
	for _, order := range [][]float64{{0, 0.004, 0.009}, {0.009, 0, 0.004}} {
		o := Result(f4Input(t, "$.total abs 0.01", order, []float64{0.004}))
		if !o.Checks[0].Reference.Stable {
			t.Errorf("runs %v: every pair agrees, the reference must be stable", order)
		}
	}
}

func TestCMP11_F4_ACandidateRunAgreesOnlyIfItAgreesWithEveryReferenceRunWhateverTheRunOrder(t *testing.T) {
	// the reference runs 0 and 0.009 agree with each other; 0.0105 is within 0.01 of 0.009 but not of 0
	for _, ref := range [][]float64{{0, 0.009}, {0.009, 0}} {
		o := Result(f4Input(t, "$.total abs 0.01", ref, []float64{0.0105}))
		if !o.Checks[0].Reference.Stable {
			t.Fatalf("reference %v must be stable", ref)
		}
		c := cellOf(t, o, "C-1", "new")
		if c.State != StateDiffers || c.Agreed != 0 {
			t.Errorf("reference %v: the candidate reads %s (agreed %d), want differs: it does not agree with every reference run", ref, c.State, c.Agreed)
		}
		if len(c.ValuePaths) != 1 || c.ValuePaths[0] != "$.total" {
			t.Errorf("reference %v: value_paths = %v", ref, c.ValuePaths)
		}
	}
	// within tolerance of both: agrees, in either order
	for _, ref := range [][]float64{{0, 0.009}, {0.009, 0}} {
		o := Result(f4Input(t, "$.total abs 0.01", ref, []float64{0.0045}))
		if c := cellOf(t, o, "C-1", "new"); c.State != StateIdentical || c.Agreed != 1 {
			t.Errorf("reference %v: a candidate within tolerance of every reference run reads %s (agreed %d), want identical", ref, c.State, c.Agreed)
		}
	}
}

func TestCMP11_F4_RelToleranceIsRelativeToTheSmallerOfTheTwoValuesSoItIsSymmetric(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Tolerance", "$.total rel 0.1")
	pairs := [][2]float64{{100, 90}, {100, 109}, {100, 111}, {0, 0}, {0, 0.001}, {50, 55}, {-100, -91}}
	for _, p := range pairs {
		ab, _ := RecordsAgree(rules, []ScenarioOutput{f4Row("C-1", p[0])}, []ScenarioOutput{f4Row("C-1", p[1])})
		ba, _ := RecordsAgree(rules, []ScenarioOutput{f4Row("C-1", p[1])}, []ScenarioOutput{f4Row("C-1", p[0])})
		if ab != ba {
			t.Errorf("rel 10%% between %v and %v: agree(a,b) = %v but agree(b,a) = %v", p[0], p[1], ab, ba)
		}
	}
	// 100 and 90: 10 apart is 10%% of 100 but 11%% of 90: the smaller one governs, so they do NOT agree, in either direction
	if ok, _ := RecordsAgree(rules, []ScenarioOutput{f4Row("C-1", 100)}, []ScenarioOutput{f4Row("C-1", 90)}); ok {
		t.Errorf("100 and 90 agreed under rel 10%%: 10 is more than 10%% of the smaller value, 90")
	}
	// 100 and 109: 9 is under 10% of the smaller value, 100: they agree
	if ok, _ := RecordsAgree(rules, []ScenarioOutput{f4Row("C-1", 100)}, []ScenarioOutput{f4Row("C-1", 109)}); !ok {
		t.Errorf("100 and 109 did not agree under rel 10%%")
	}
}

func TestCMP11_F4_AnApprovalPinnedToACellGoesStaleWhenAnyReferenceRunsValuesChange(t *testing.T) {
	hashOf := func(ref []float64) string {
		o := Result(f4Input(t, "$.total abs 0.01", ref, []float64{0.004}))
		return cellOf(t, o, "C-1", "new").ReferenceHash
	}
	base := hashOf([]float64{0.001, 0.002})
	if base == "" {
		t.Fatal("the cell carries no reference hash")
	}
	// the SECOND reference run changes, within tolerance: the reference is still stable, the hash must still move
	if got := hashOf([]float64{0.001, 0.003}); got == base {
		t.Errorf("the reference hash did not change when the second reference run's value changed")
	}
	// and the first
	if got := hashOf([]float64{0.0015, 0.002}); got == base {
		t.Errorf("the reference hash did not change when the first reference run's value changed")
	}
	// run order does not move it
	if a, b := hashOf([]float64{0.001, 0.002}), hashOf([]float64{0.002, 0.001}); a != b {
		t.Errorf("the reference hash depends on which run came first: %s vs %s", a, b)
	}
}

package compare

// cmp11_f5_test.go -- ARGUS-CMP-11 fix F5: outputs_root commits to the numbers that decide a cell (a row's tolerant values and its load
// numbers), and a row that carries neither contributes byte-for-byte what it always did, so every root a released 0.3.57 executor
// computed, and every stored root, stays valid.

import (
	"encoding/json"
	"testing"
)

// f5Plain are rows with no tolerant values and no load numbers: the only rows a 0.3.57 executor can send.
func f5Plain() []ScenarioOutput {
	a := out("C-1", hx("1"))
	a.Step, a.Sample = "create", 1
	b := out("C-1", hx("2"))
	b.Step, b.Sample = "read", 2
	c := out("C-2", hx("3"))
	return []ScenarioOutput{c, b, a}
}

// goldRootNoValues is OutputsRoot(f5Plain()) at origin/dev (050608b), at the v0.3.57 tag (f9d28d0) and at 83deb25e: the same.
const goldRootNoValues = "5a5beb01fb000bde328e55cac7c7b85de1cc4f977c3c4001131915a7945dd9c6"

// goldRootWithValues is OutputsRoot(f5WithValues()): new in this fix, there is nothing older to equal.
const goldRootWithValues = "4f95c017cae85b5396ee900f54ac6fbc9620b1ab1844e1b22007343f37fa9be8"

func f5WithValues() []ScenarioOutput {
	rows := f5Plain()
	rows[0].Values = []ToleranceValue{{Path: "$.total", Rule: 0, Value: 1234.5}, {Path: "$.fee", Rule: 1, Value: 0.25}}
	l := LoadRecord(LoadNumbers{Samples: 40, P50Ms: 12, P95Ms: 80, P99Ms: 99, ErrorRate: 0.02})
	rows = append(rows, ScenarioOutput{ScenarioID: "C-3", OutputRecord: l})
	return rows
}

func TestCMP11_F5_ARowWithNoValuesAndNoLoadContributesWhatItAlwaysDid(t *testing.T) {
	got := OutputsRoot(f5Plain())
	t.Logf("GOLDEN root without values = %s", got)
	if got != goldRootNoValues {
		t.Errorf("OutputsRoot over rows without values = %s, want %s (every stored root and every root a released executor computed must stay valid)", got, goldRootNoValues)
	}
}

func TestCMP11_F5_TheRootCoversTheTolerantValuesAndTheLoadNumbers(t *testing.T) {
	base := OutputsRoot(f5WithValues())
	t.Logf("GOLDEN root with values = %s", base)
	if base != goldRootWithValues {
		t.Errorf("OutputsRoot over rows with values = %s, want %s", base, goldRootWithValues)
	}
	if base == OutputsRoot(f5Plain()) {
		t.Fatalf("the root is the same with and without the values: it commits to nothing about them")
	}
	for name, mod := range map[string]func(rows []ScenarioOutput){
		"a tolerant value":      func(r []ScenarioOutput) { r[0].Values[0].Value = 1234.6 },
		"a tolerant path":       func(r []ScenarioOutput) { r[0].Values[0].Path = "$.totals" },
		"a tolerant rule index": func(r []ScenarioOutput) { r[0].Values[0].Rule = 1; r[0].Values[1].Rule = 0 },
		"a value dropped":       func(r []ScenarioOutput) { r[0].Values = r[0].Values[:1] },
		"a value added": func(r []ScenarioOutput) {
			r[0].Values = append(r[0].Values, ToleranceValue{Path: "$.x", Rule: 2, Value: 1})
		},
		"a load sample count":       func(r []ScenarioOutput) { r[3].Load.Samples = 41 },
		"a load p50":                func(r []ScenarioOutput) { r[3].Load.P50Ms = 13 },
		"a load p95":                func(r []ScenarioOutput) { r[3].Load.P95Ms = 81 },
		"a load p99":                func(r []ScenarioOutput) { r[3].Load.P99Ms = 100 },
		"a load error rate":         func(r []ScenarioOutput) { r[3].Load.ErrorRate = 0.021 },
		"a value moved to the next": func(r []ScenarioOutput) { r[0].Values, r[1].Values = nil, r[0].Values },
	} {
		rows := f5WithValues()
		// deep copy so a mod cannot leak into the next case
		raw, _ := json.Marshal(rows)
		var cp []ScenarioOutput
		_ = json.Unmarshal(raw, &cp)
		mod(cp)
		if OutputsRoot(cp) == base {
			t.Errorf("%s changed in transit, same row hashes: the root did not move", name)
		}
	}
	// the order the rows or a row's values arrive in is not part of what the root commits to
	rows := f5WithValues()
	rows[0].Values[0], rows[0].Values[1] = rows[0].Values[1], rows[0].Values[0]
	rows[0], rows[3] = rows[3], rows[0]
	if got := OutputsRoot(rows); got != base {
		t.Errorf("a reordering of rows and of values moved the root: %s vs %s", got, base)
	}
}

func TestCMP11_F5_APushWhoseValueChangedInTransitDoesNotMatchTheRootOverTheRowsAsSent(t *testing.T) {
	sent := f5WithValues()
	root := OutputsRoot(sent)      // what the executor computed
	sent[0].Values[0].Value = 9999 // changed in transit, same row hash
	raw, _ := json.Marshal(sent)
	_, _, sentRoot, _, err := SanitizeOutputs(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sentRoot == root {
		t.Errorf("the control plane's root over the rows as sent equals the executor's although a tolerant value changed")
	}
}

func TestCMP11_F5_ADroppedRowStillCountsInTheRootOverTheRowsAsSentSoTheRestIsKept(t *testing.T) {
	rows := f5WithValues()
	rows = append(rows, ScenarioOutput{ScenarioID: "C-9", OutputRecord: LoadRecord(LoadNumbers{Samples: 5, P95Ms: 3, ErrorRate: 7})}) // a bad number
	root := OutputsRoot(rows)                                                                                                         // the executor committed to every row it sent
	raw, _ := json.Marshal(rows)
	kept, _, sentRoot, dropped, err := SanitizeOutputs(raw)
	if err != nil || dropped != 1 || len(kept) != len(rows)-1 {
		t.Fatalf("kept %d dropped %d err %v", len(kept), dropped, err)
	}
	if sentRoot != root {
		t.Errorf("sentRoot %s is not the root over the rows as sent %s: a push with one malformed number would lose every row", sentRoot, root)
	}
}

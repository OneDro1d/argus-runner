package compare

// cmp11_f3_test.go -- ARGUS-CMP-11 fix F3: impossible load numbers (p50 > p95 or p95 > p99) are not judged. The row's numbers are
// dropped the way a negative number is (that row only, the push is never refused) and the cell reads not measured.

import (
	"encoding/json"
	"testing"
)

func TestCMP11_F3_ARowWithPercentilesOutOfOrderIsDroppedAloneAndEqualValuesAreKept(t *testing.T) {
	mk := func(id string, p50, p95, p99 float64, samples int) map[string]any {
		return rowJSON(id, func(m map[string]any) {
			l := m["load"].(map[string]any)
			l["p50_ms"], l["p95_ms"], l["p99_ms"], l["samples"] = p50, p95, p99, samples
		})
	}
	rows := []any{
		mk("C-1", 10, 20, 30, 50),  // in order
		mk("C-2", 30, 20, 40, 50),  // p50 > p95
		mk("C-3", 10, 50, 40, 50),  // p95 > p99
		mk("C-4", 7, 7, 7, 1),      // one sample: all three equal
		mk("C-5", 0, 0, 0, 0),      // no sample: all zero
		mk("C-6", 10, 10, 30, 50),  // p50 == p95
		mk("C-7", 10, 30, 30, 50),  // p95 == p99
		mk("C-8", 100, 90, 80, 50), // reversed
	}
	raw, _ := json.Marshal(rows)
	kept, _, _, dropped, err := SanitizeOutputs(raw)
	if err != nil {
		t.Fatalf("SanitizeOutputs: %v (an impossible row must never refuse the push)", err)
	}
	var ids []string
	for _, k := range kept {
		ids = append(ids, k.ScenarioID)
	}
	want := []string{"C-1", "C-4", "C-5", "C-6", "C-7"}
	if len(ids) != len(want) || dropped != 3 {
		t.Fatalf("kept %v dropped %d, want kept %v and 3 dropped (C-2, C-3, C-8)", ids, dropped, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("kept %v, want %v", ids, want)
		}
	}
	// the stored form is read back by the strict reader too: nothing out of order survives in the database
	norm, _ := json.Marshal(kept)
	if _, _, derr := DecodeOutputs(norm); derr != nil {
		t.Errorf("the kept rows do not decode: %v", derr)
	}
	if _, _, derr := DecodeOutputs(mustMarshal(t, []any{mk("C-2", 30, 20, 40, 50)})); derr == nil {
		t.Errorf("DecodeOutputs accepted a row with p50 > p95")
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCMP11_F3_AnImpossibleRowAlreadyInARunReadsNotMeasuredNeverJudged(t *testing.T) {
	bad := LoadNumbers{Samples: 50, P50Ms: 500, P95Ms: 100, P99Ms: 200, ErrorRate: 0.01} // p50 above p95
	in := loadInput(propertyLoad(t, "p95 20%"),
		run("r1", "old", "v", "passed"), run("r2", "new", "v", "passed"))
	in.Runs[0].Outputs = []ScenarioOutput{loadRow("C-1", ln(100))}
	in.Runs[1].Outputs = []ScenarioOutput{loadRow("C-1", bad)}
	o := Result(in)
	if len(o.Performance) != 1 || len(o.Performance[0].Cells) != 1 {
		t.Fatalf("performance = %+v", o.Performance)
	}
	if pc := o.Performance[0].Cells[0]; pc.State != BandNotMeasured || pc.Runs != 0 {
		t.Errorf("a member row with impossible numbers reads %+v, want not_measured with 0 runs", pc)
	}
}

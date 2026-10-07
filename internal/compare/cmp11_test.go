package compare

// cmp11_test.go -- ARGUS-CMP-11 ( tolerance values cross as numbers load parity).
// The pure half: the load record, the numbers' bounds, the judge's reading of values and of load numbers. No store,
// no clock, no executor.

import (
	"encoding/json"
	"strings"
	"testing"
)

func loadRow(id string, n LoadNumbers) ScenarioOutput {
	return ScenarioOutput{ScenarioID: id, OutputRecord: LoadRecord(n)}
}

func ln(p95 float64) LoadNumbers {
	return LoadNumbers{Samples: 50, P50Ms: p95 / 2, P95Ms: p95, P99Ms: p95 * 2, ErrorRate: 0.01}
}

// propertyLoad is the rules of a load check: its own claim is a property, its performance is the band.
func propertyLoad(t *testing.T, band string) *Rules {
	t.Helper()
	return mustRules(t, "Reference", "property", "Agreement", "100%", "Not Worse Than", band)
}

func loadInput(rules *Rules, runs ...Run) Input {
	return Input{SetHash: setH, Checks: []Check{{ID: "C-1", Path: "a/C-1.md", Rules: rules}}, Members: twoMembers(), Runs: runs}
}

// ── the record ───────────────────────────────────────────────────────────────────────────────────

func TestCMP11_LoadRecordCarriesNumbersAndNoOutput(t *testing.T) {
	rec := LoadRecord(LoadNumbers{Samples: 120, P50Ms: 40, P95Ms: 90, P99Ms: 130, ErrorRate: 0.02})
	if rec.V != RecordVersion || rec.Sample != 1 || rec.Step != "" {
		t.Errorf("record header = %+v", rec)
	}
	if rec.State != StateNotRecorded || rec.Reason != ReasonLoadNumbersOnly {
		t.Errorf("a load record is not_recorded / load_numbers_only, got %q / %q", rec.State, rec.Reason)
	}
	if rec.Hash != "" || rec.Parts != (Parts{}) || rec.Status != 0 || rec.BodyKind != "" || rec.BodyBytes != 0 {
		t.Errorf("no body, no status and no hash is invented for a load record: %+v", rec)
	}
	if rec.Load == nil || rec.Load.Samples != 120 || rec.Load.P95Ms != 90 || rec.Load.ErrorRate != 0.02 {
		t.Fatalf("load = %+v", rec.Load)
	}
	// the control plane's own reader accepts it, and the key is `load`
	raw, _ := json.Marshal([]ScenarioOutput{{ScenarioID: "C-1", OutputRecord: rec}})
	if !strings.Contains(string(raw), `"load":{"samples":120,"p50_ms":40,"p95_ms":90,"p99_ms":130,"error_rate":0.02}`) {
		t.Errorf("wire form = %s", raw)
	}
	rows, _, err := DecodeOutputs(raw)
	if err != nil || len(rows) != 1 || rows[0].Load == nil || rows[0].Load.P99Ms != 130 {
		t.Fatalf("DecodeOutputs: %v %+v", err, rows)
	}
	if !ValidReason(ReasonLoadNumbersOnly) || ReasonText(ReasonLoadNumbersOnly) == "" {
		t.Errorf("the reason is not in the closed vocabulary")
	}
}

// A measured check whose only record is a load record is NOT an output cell: it cannot be identical or differs by
// accident, whatever the numbers are (the hash is empty on both sides, which would otherwise read as "equal").
func TestCMP11_ALoadRecordIsNeverReadAsAnOutputCellThatIsIdenticalOrDiffers(t *testing.T) {
	for _, cand := range []float64{100, 5000} {
		in := baseInput(
			run("r1", "old", "v", "passed"), run("r2", "old", "v", "passed"), run("r3", "new", "v", "passed"),
		)
		in.Runs[0].Outputs = []ScenarioOutput{loadRow("C-1", ln(100))}
		in.Runs[1].Outputs = []ScenarioOutput{loadRow("C-1", ln(100))}
		in.Runs[2].Outputs = []ScenarioOutput{loadRow("C-1", ln(cand))}
		for _, r := range in.Runs {
			r.Outcomes["C-1"] = "passed"
		}
		in.Checks[0].Rules = mustRules(t, "Reference", "measured", "Not Worse Than", "p95 20%")
		o := Result(in)
		c := cellOf(t, o, "C-1", "new")
		if c.State == StateIdentical || c.State == StateDiffers {
			t.Errorf("candidate p95 %v: the load record was read as an output cell: %+v", cand, c)
		}
		if c.State != StateNotMeasured {
			t.Errorf("candidate p95 %v: cell = %s, want not_measured", cand, c.State)
		}
		if o.Verdict == VerdictSame {
			t.Errorf("candidate p95 %v: verdict same", cand)
		}
	}
}

// ── bounds: refuse nothing, drop what is malformed ───────────────────────────────────────────────

func rowJSON(id string, mod func(map[string]any)) map[string]any {
	rec := LoadRecord(ln(100))
	b, _ := json.Marshal(ScenarioOutput{ScenarioID: id, OutputRecord: rec})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if mod != nil {
		mod(m)
	}
	return m
}

func TestCMP11_SanitizeKeepsGoodRowsAndDropsOnlyTheRowWithABadNumber(t *testing.T) {
	good := rowJSON("C-1", nil)
	bad1 := rowJSON("C-2", func(m map[string]any) { m["load"].(map[string]any)["error_rate"] = 2 })                               // outside 0..1
	bad2 := rowJSON("C-3", func(m map[string]any) { m["load"].(map[string]any)["p95_ms"] = -4 })                                  // negative
	bad3 := rowJSON("C-4", func(m map[string]any) { m["load"].(map[string]any)["samples"] = -1 })                                 // negative count
	bad4 := rowJSON("C-5", func(m map[string]any) { m["values"] = []any{map[string]any{"path": "$.a", "rule": 99, "value": 1}} }) // rule out of range
	extra := rowJSON("C-6", func(m map[string]any) { m["body"] = "CANARY-BODY"; m["load"].(map[string]any)["secret"] = "CANARY-KEY" })
	raw, _ := json.Marshal([]any{good, bad1, bad2, bad3, bad4, extra})

	kept, norm, root, dropped, err := SanitizeOutputs(raw)
	if err != nil {
		t.Fatalf("SanitizeOutputs: %v", err)
	}
	var ids []string
	for _, k := range kept {
		ids = append(ids, k.ScenarioID)
	}
	if strings.Join(ids, ",") != "C-1,C-6" || dropped != 4 {
		t.Fatalf("kept %v dropped %d, want C-1,C-6 and 4 dropped", ids, dropped)
	}
	if strings.Contains(string(norm), "CANARY") || strings.Contains(string(norm), `"body"`) {
		t.Errorf("a key the type does not declare was stored: %s", norm)
	}
	// the root the executor computed covers EVERY row it sent, so it is checked over the rows as sent
	var all []ScenarioOutput
	for _, m := range []map[string]any{good, bad1, bad2, bad3, bad4, extra} {
		var o ScenarioOutput
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &o)
		all = append(all, o)
	}
	if root != OutputsRoot(all) {
		t.Errorf("sentRoot %s is not the root over the rows as sent %s", root, OutputsRoot(all))
	}
}

func TestCMP11_ARecordThatIsNotValidAtAllStillDropsTheArrayNeverTheRun(t *testing.T) {
	if _, _, _, _, err := SanitizeOutputs([]byte(`{"not":"an array"}`)); err == nil {
		t.Errorf("an object is not an outputs array")
	}
	if _, _, _, _, err := SanitizeOutputs([]byte(`[{"scenario_id":"C-1","v":1,"state":"recorded","hash":"zz"}]`)); err == nil {
		t.Errorf("a recorded row with a bad hash must refuse the array: the type is the wire's contract")
	}
}

func TestCMP11_MoreThan64TolerantValuesInOneCheckDropsThatCheckNotTheOthers(t *testing.T) {
	mk := func(id, step string, n int) map[string]any {
		var vals []any
		for i := 0; i < n; i++ {
			vals = append(vals, map[string]any{"path": "$.a[" + string(rune('0'+i%10)) + "]", "rule": 0, "value": float64(i)})
		}
		h := strings.Repeat("a", 64)
		return map[string]any{"scenario_id": id, "v": 1, "step": step, "sample": 1, "state": "recorded", "status": 200, "hash": h,
			"parts": map[string]any{"status": h}, "values": vals}
	}
	// 40 + 40 in two steps of one check is 80 > 64; one check with 30 is fine
	raw, _ := json.Marshal([]any{mk("C-1", "a", 40), mk("C-1", "b", 40), mk("C-2", "a", 30)})
	kept, _, _, dropped, err := SanitizeOutputs(raw)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, k := range kept {
		ids = append(ids, k.ScenarioID+"/"+k.Step)
	}
	if len(kept) != 1 || kept[0].ScenarioID != "C-2" || dropped != 2 {
		t.Errorf("kept %v dropped %d, want only C-2 kept and the 2 rows of C-1 dropped", ids, dropped)
	}
}

// ── what the judge does with load numbers ────────────────────────────────────────────────────────

func TestCMP11_ARecordWithZeroSamplesHasNoNumbersAndIsNotMeasuredNeverFaster(t *testing.T) {
	zero := LoadNumbers{Samples: 0}
	in := loadInput(propertyLoad(t, "p95 20%"),
		run("r1", "old", "v", "passed"), run("r2", "new", "v", "passed"))
	in.Runs[0].Outputs = []ScenarioOutput{loadRow("C-1", ln(100))}
	in.Runs[1].Outputs = []ScenarioOutput{loadRow("C-1", zero)}
	o := Result(in)
	if len(o.Performance) != 1 || len(o.Performance[0].Cells) != 1 {
		t.Fatalf("performance = %+v", o.Performance)
	}
	if pc := o.Performance[0].Cells[0]; pc.State != BandNotMeasured || pc.Runs != 0 {
		t.Errorf("a zero-sample record read as %+v: 0 ms is not 'faster'", pc)
	}
	if o.Verdict != VerdictIncomplete {
		t.Errorf("verdict = %s, want incomplete", o.Verdict)
	}
}

func TestCMP11_APercentBandOverAZeroReferenceMedianAdmitsOnlyZero(t *testing.T) {
	for _, tc := range []struct {
		member float64
		want   BandState
	}{{0, BandNotWorse}, {1, BandWorse}} {
		ref := LoadNumbers{Samples: 10, P50Ms: 0, P95Ms: 0, P99Ms: 0, ErrorRate: 0}
		mem := LoadNumbers{Samples: 10, P95Ms: tc.member, P99Ms: tc.member} // fix F3: p95 above p99 is a row the executor cannot send
		in := loadInput(propertyLoad(t, "p95 20%"), run("r1", "old", "v", "passed"), run("r2", "new", "v", "passed"))
		in.Runs[0].Outputs = []ScenarioOutput{loadRow("C-1", ref)}
		in.Runs[1].Outputs = []ScenarioOutput{loadRow("C-1", mem)}
		pc := Result(in).Performance[0].Cells[0]
		if pc.State != tc.want {
			t.Errorf("reference 0, member %v: %s, want %s (a percentage of zero is zero)", tc.member, pc.State, tc.want)
		}
	}
}

func TestCMP11_APpBandIsTheReferencePlusTheBandInPercentagePoints(t *testing.T) {
	for _, tc := range []struct {
		rate float64
		want BandState
	}{{0.015, BandNotWorse}, {0.0151, BandWorse}, {0.0, BandNotWorse}} {
		in := loadInput(propertyLoad(t, "error_rate 0.5pp"), run("r1", "old", "v", "passed"), run("r2", "new", "v", "passed"))
		in.Runs[0].Outputs = []ScenarioOutput{loadRow("C-1", ln(100))} // 1% errors
		m := ln(100)
		m.ErrorRate = tc.rate
		in.Runs[1].Outputs = []ScenarioOutput{loadRow("C-1", m)}
		pc := Result(in).Performance[0].Cells[0]
		if pc.State != tc.want {
			t.Errorf("reference 1%%, member %v: %s, want %s", tc.rate, pc.State, tc.want)
		}
	}
}

func TestCMP11_APerformanceBandMedianOverRunsAndBothRunCountsAreShown(t *testing.T) {
	in := loadInput(propertyLoad(t, "p95 20%"),
		run("r1", "old", "v", "passed"), run("r2", "old", "v", "passed"), run("r3", "old", "v", "passed"),
		run("r4", "new", "v", "passed"), run("r5", "new", "v", "passed"))
	for i, p := range []float64{90, 100, 1000} { // the median is 100: one wild run does not move it
		in.Runs[i].Outputs = []ScenarioOutput{loadRow("C-1", ln(p))}
	}
	in.Runs[3].Outputs = []ScenarioOutput{loadRow("C-1", ln(110))}
	in.Runs[4].Outputs = []ScenarioOutput{loadRow("C-1", ln(130))} // median 120 = reference x 1.2
	pr := Result(in).Performance[0]
	if pr.Check != "C-1" || pr.Metric != "p95" || pr.Band != "20%" || pr.Reference.Value != 100 || pr.Reference.Runs != 3 {
		t.Fatalf("performance = %+v", pr)
	}
	if c := pr.Cells[0]; c.Member != "new" || c.Value != 120 || c.Runs != 2 || c.State != BandNotWorse {
		t.Errorf("cell = %+v, want new / 120 / 2 runs / not_worse (exactly at the band)", c)
	}
}

// The roll-up: a performance cell that is not measured is a gap and keeps the verdict from `same`; with the numbers there, and
// nothing else wrong, the same input reads `same`. Every other cell here is clean, so only the performance cell decides.
func TestCMP11_ANotMeasuredPerformanceCellAloneMakesTheRollUpIncompleteNeverSame(t *testing.T) {
	build := func(withMemberNumbers bool) Output {
		in := loadInput(propertyLoad(t, "p95 20%"), run("r1", "old", "v", "passed"), run("r2", "new", "v", "passed"))
		for _, r := range in.Runs {
			r.Outcomes["C-1"] = "passed"
		}
		in.Runs[0].Outputs = []ScenarioOutput{loadRow("C-1", ln(100))}
		if withMemberNumbers {
			in.Runs[1].Outputs = []ScenarioOutput{loadRow("C-1", ln(110))}
		}
		return Result(in)
	}
	with, without := build(true), build(false)
	if with.Verdict != VerdictSame {
		t.Fatalf("fixture: with the numbers the verdict is %s (counts %+v), want same", with.Verdict, with.Counts)
	}
	if without.Verdict != VerdictIncomplete {
		t.Errorf("without the member's numbers the verdict is %s, want incomplete", without.Verdict)
	}
	if pc := without.Performance[0].Cells[0]; pc.State != BandNotMeasured {
		t.Errorf("the cell reads %+v, want not_measured", pc)
	}
	if c := without.Counts; c.NotMeasured != 0 || c.Noise != 0 || c.CouldNotRun != 0 || c.Differs != 0 {
		t.Errorf("counts %+v: nothing but the performance cell may be the gap here", c)
	}
}

// A worse performance cell has no output hash to pin: approving the OUTPUT difference of the same check does not
// clear it, and the roll-up stays `differences` (CMP-10 approvals pin two output hashes; a load cell has none).
func TestCMP11_AWorseCellCannotBeApprovedAwayByApprovingTheOutputOfTheSameCheck(t *testing.T) {
	rules := mustRules(t, "Reference", "measured", "Not Worse Than", "p95 20%")
	mk := func(id, member, body string, p95 float64) Run {
		r := run(id, member, "v", "passed", out("C-1", body))
		r.Outputs = append(r.Outputs, loadRow("C-1", ln(p95)))
		return r
	}
	in := baseInput(mk("r1", "old", hx("1"), 100), mk("r2", "old", hx("1"), 100), mk("r3", "new", hx("2"), 300))
	in.Checks[0].Rules = rules
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateDiffers {
		t.Fatalf("fixture: %+v", c)
	}
	ap := approvalFor(c, "apr_1")
	ap.VersionKey = "v"
	in.Approvals = []Approval{ap}
	o := Result(in)
	if got := cellOf(t, o, "C-1", "new"); !got.Approved {
		t.Fatalf("the output difference is approved: %+v", got)
	}
	if o.Counts.Worse != 1 || o.Verdict != VerdictDifferences {
		t.Errorf("counts %+v verdict %s: the worse performance cell must keep the verdict at differences", o.Counts, o.Verdict)
	}
}

// ── tolerance values ─────────────────────────────────────────────────────────────────────────────

func tolRow(vals ...ToleranceValue) ScenarioOutput {
	o := out("C-1", hx("1"))
	o.Values = vals
	return o
}

func TestCMP11_AToleranceDifferenceNamesTheDeclaredPathNotTheValue(t *testing.T) {
	rules := measured(t, "Tolerance", "$.total abs 0.5; $.rate rel 0.1")
	// normalized order: sorted by path => $.rate is rule 0, $.total is rule 1
	ref := tolRow(ToleranceValue{Path: "$.rate", Rule: 0, Value: 10}, ToleranceValue{Path: "$.total", Rule: 1, Value: 100})
	same := tolRow(ToleranceValue{Path: "$.rate", Rule: 0, Value: 10.9}, ToleranceValue{Path: "$.total", Rule: 1, Value: 100.4})
	off := tolRow(ToleranceValue{Path: "$.rate", Rule: 0, Value: 10.9}, ToleranceValue{Path: "$.total", Rule: 1, Value: 987654321.125})
	in := baseInput(
		run("r1", "old", "v", "passed", ref), run("r2", "old", "v", "passed", ref),
		run("r3", "new", "v", "passed", same), run("r4", "other", "v", "passed", off))
	in.Members = append(twoMembers(), Member{Name: "other", Role: RoleCandidate})
	in.Checks[0].Rules = rules
	o := Result(in)
	if c := cellOf(t, o, "C-1", "new"); c.State != StateIdentical {
		t.Errorf("within tolerance: %+v, want identical", c)
	}
	c := cellOf(t, o, "C-1", "other")
	if c.State != StateDiffers || strings.Join(c.PartsDiffer, ",") != "values" || strings.Join(c.ValuePaths, ",") != "$.total" {
		t.Errorf("outside tolerance: %+v, want differs / values / value_paths [$.total]", c)
	}
	if b, _ := json.Marshal(o); strings.Contains(string(b), "987654321") {
		t.Errorf("a measured value reached the result: %s", b)
	}
}

func TestCMP11_ATolerantLeafOnOneSideOnlyDiffers(t *testing.T) {
	rules := measured(t, "Tolerance", "$.a abs 1; $.b abs 1")
	ref := tolRow(ToleranceValue{Path: "$.a", Rule: 0, Value: 1}, ToleranceValue{Path: "$.b", Rule: 1, Value: 1})
	mem := tolRow(ToleranceValue{Path: "$.a", Rule: 0, Value: 1})
	in := baseInput(run("r1", "old", "v", "passed", ref), run("r2", "old", "v", "passed", ref), run("r3", "new", "v", "passed", mem))
	in.Checks[0].Rules = rules
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateDiffers || strings.Join(c.ValuePaths, ",") != "$.b" {
		t.Errorf("a leaf present on one side only: %+v, want differs naming $.b", c)
	}
}

// With several reference runs the reference is stable when EVERY PAIR of its runs agree within tolerance, and a candidate run
// agrees when it agrees with EVERY reference run (ARGUS-CMP-11 fix F4: this test pinned "compared with the FIRST usable run", the
// rule F4 replaces, because a tolerance is not transitive and the first run is only the one with the lowest id).
func TestCMP11_AReferenceWhoseRunsAreWithinToleranceIsStableAndTheCandidateMustAgreeWithEveryReferenceRun(t *testing.T) {
	rules := measured(t, "Tolerance", "$.t abs 0.5")
	v := func(x float64) ScenarioOutput { return tolRow(ToleranceValue{Path: "$.t", Rule: 0, Value: x}) }
	for _, tc := range []struct {
		cand float64
		want CellState
	}{{9.6, StateDiffers}, {10.45, StateIdentical}, {10.8, StateDiffers}} { // 9.6 is within 0.5 of 10.0 but 0.8 from 10.4
		// r1 = 10.0 (the first), r2 = 10.4: within 0.5 of r1, so the reference is stable, not noise
		in := baseInput(run("r1", "old", "v", "passed", v(10)), run("r2", "old", "v", "passed", v(10.4)), run("r3", "new", "v", "passed", v(tc.cand)))
		in.Checks[0].Rules = rules
		o := Result(in)
		ref := o.Checks[0].Reference
		if !ref.Stable || ref.Runs != 2 {
			t.Fatalf("reference = %+v, want stable over 2 runs", ref)
		}
		c := cellOf(t, o, "C-1", "new")
		if c.State != tc.want || c.Control != ControlStable {
			t.Errorf("candidate %v: %s / %s, want %s / stable (it must agree with EVERY reference run, 10.0 and 10.4)", tc.cand, c.State, c.Control, tc.want)
		}
	}
	// a reference whose runs are NOT within tolerance of each other is noise, as before
	in := baseInput(run("r1", "old", "v", "passed", v(10)), run("r2", "old", "v", "passed", v(11)), run("r3", "new", "v", "passed", v(10)))
	in.Checks[0].Rules = rules
	if c := cellOf(t, Result(in), "C-1", "new"); c.State != StateNoise {
		t.Errorf("an unstable tolerant reference: %+v, want noise", c)
	}
}

// the executor's half of the per-check bound: more than 64 tolerant values over the steps of one check make every row of it
// not_recorded / too_many_values, and a check within the bound is untouched
func TestCMP11_CapValuesPerCheckTurnsAnOverBoundCheckIntoNotRecorded(t *testing.T) {
	mk := func(id, step string, n int) ScenarioOutput {
		o := out(id, hx("1"))
		o.Step = step
		for i := 0; i < n; i++ {
			o.Values = append(o.Values, ToleranceValue{Path: "$.a", Rule: 0, Value: float64(i)})
		}
		return o
	}
	got := CapValuesPerCheck([]ScenarioOutput{mk("C-1", "a", 40), mk("C-1", "b", 40), mk("C-2", "a", 30)})
	for _, r := range got[:2] {
		if r.State != StateNotRecorded || r.Reason != ReasonTooManyValues || r.Hash != "" || r.Parts != (Parts{}) || len(r.Values) != 0 {
			t.Errorf("an over-bound check's row = %+v, want not_recorded / too_many_values with no hash and no values", r)
		}
	}
	if r := got[2]; r.State != StateRecorded || len(r.Values) != 30 || r.Hash == "" {
		t.Errorf("a check within the bound was touched: %+v", r)
	}
	// the rows it makes are ones the control plane's reader keeps
	raw, _ := json.Marshal(got)
	if kept, _, _, dropped, err := SanitizeOutputs(raw); err != nil || dropped != 0 || len(kept) != 3 {
		t.Errorf("the control plane's reader: kept %d dropped %d err %v", len(kept), dropped, err)
	}
}

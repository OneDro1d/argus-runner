package compare

import (
	"reflect"
	"testing"
)

func TestWithinTolerance(t *testing.T) {
	abs := Tolerance{Path: "$.a", Kind: "abs", Value: 0.01}
	rel := Tolerance{Path: "$.a", Kind: "rel", Value: 0.001}
	cases := []struct {
		t        Tolerance
		ref, got float64
		want     bool
	}{
		{abs, 10, 10.01, true}, {abs, 10, 10.0100001, false}, {abs, 10, 9.99, true}, {abs, 0, 0.011, false},
		{rel, 1000, 1001, true}, {rel, 1000, 1001.0001, false}, {rel, 1000, 999, true}, {rel, -1000, -1001, true},
		{rel, 0, 0, true}, {rel, 0, 0.0000001, false}, // relative to the reference: a zero reference admits only zero
	}
	for _, c := range cases {
		if got := WithinTolerance(c.t, c.ref, c.got); got != c.want {
			t.Errorf("%+v ref=%v got=%v: %v, want %v", c.t, c.ref, c.got, got, c.want)
		}
	}
}

func TestJudgeClaim(t *testing.T) {
	p, f, e := OutcomePassed, OutcomeFailed, OutcomeErrored
	cases := []struct {
		name     string
		outcomes []string
		pct      float64
		state    CellState
		n, held  int
		cnr      int
	}{
		{"all pass", []string{p, p, p}, 100, StateIdentical, 3, 3, 0},
		{"one fail of three at 100", []string{p, p, f}, 100, StateDiffers, 3, 2, 0},
		{"two of three at 66%", []string{p, p, f}, 66, StateIdentical, 3, 2, 0},
		{"two of three at 67%", []string{p, p, f}, 67, StateDiffers, 3, 2, 0},
		{"errored is excluded, not a failure", []string{p, p, e}, 100, StateIdentical, 2, 2, 1},
		{"degraded counts as passed", []string{OutcomeDegraded, p}, 100, StateIdentical, 2, 2, 0},
		{"all errored", []string{e, e}, 100, StateCouldNotRun, 0, 0, 2},
		{"none", nil, 100, StateNotMeasured, 0, 0, 0},
		{"unknown outcome is could not run", []string{"skipped", p}, 100, StateIdentical, 1, 1, 1},
		{"exact boundary 90%", []string{p, p, p, p, p, p, p, p, p, f}, 90, StateIdentical, 10, 9, 0},
	}
	for _, c := range cases {
		j := JudgeClaim(c.outcomes, c.pct)
		if j.State != c.state || j.Runs != c.n || j.Held != c.held || j.CouldNotRun != c.cnr {
			t.Errorf("%s: %+v, want state=%s n=%d held=%d cnr=%d", c.name, j, c.state, c.n, c.held, c.cnr)
		}
	}
	// the statistics rule rides along
	j := JudgeClaim([]string{OutcomePassed, OutcomePassed, OutcomePassed, OutcomePassed, OutcomePassed}, 99)
	if j.State != StateIdentical || j.ClaimSupported {
		t.Errorf("5 clean runs hold 99%% on these runs but cannot support it: %+v", j)
	}
	if j.Supports == "" || j.Unsupported == "" {
		t.Errorf("sentences missing: %+v", j)
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{{[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5}, {[]float64{7}, 7}, {nil, 0}}
	for _, c := range cases {
		in := append([]float64(nil), c.in...)
		if got := Median(in); got != c.want {
			t.Errorf("Median(%v) = %v, want %v", c.in, got, c.want)
		}
		if !reflect.DeepEqual(in, c.in) {
			t.Errorf("Median must not reorder its argument: %v", in)
		}
	}
}

func TestJudgeBand(t *testing.T) {
	pct := Band{Metric: "p95", Value: 20, Unit: "%"}
	pp := Band{Metric: "error_rate", Value: 0.5, Unit: "pp"}
	cases := []struct {
		name     string
		b        Band
		ref, got []float64
		state    BandState
	}{
		{"within", pct, []float64{100, 100, 100}, []float64{110, 119, 120}, BandNotWorse}, // median 119 <= 120
		{"exactly at the edge", pct, []float64{100}, []float64{120}, BandNotWorse},
		{"over", pct, []float64{100}, []float64{120.01}, BandWorse},
		{"median, not mean", pct, []float64{100}, []float64{100, 100, 500}, BandNotWorse},
		{"better is fine", pct, []float64{100}, []float64{50}, BandNotWorse},
		{"no member data", pct, []float64{100}, nil, BandNotMeasured},
		{"no reference data", pct, nil, []float64{100}, BandNotMeasured},
		{"pp within (fractions)", pp, []float64{0.010}, []float64{0.014}, BandNotWorse},
		{"pp over", pp, []float64{0.010}, []float64{0.0151}, BandWorse},
		{"pp edge", pp, []float64{0.010}, []float64{0.015}, BandNotWorse},
		{"zero reference, percent band", pct, []float64{0}, []float64{1}, BandWorse},
	}
	for _, c := range cases {
		j := JudgeBand(c.b, c.ref, c.got)
		if j.State != c.state {
			t.Errorf("%s: %+v, want %s", c.name, j, c.state)
		}
	}
	j := JudgeBand(pct, []float64{100, 120}, []float64{130})
	if j.Reference != 110 || j.Member != 130 || j.ReferenceRuns != 2 || j.MemberRuns != 1 {
		t.Errorf("medians and run counts are reported: %+v", j)
	}
}

func so(id, step string, sample int, hash string, parts Parts, vals ...ToleranceValue) ScenarioOutput {
	return ScenarioOutput{ScenarioID: id, OutputRecord: OutputRecord{V: 1, Step: step, Sample: sample, State: StateRecorded, Status: 200, Hash: hash, Parts: parts, Values: vals}}
}

func TestRecordsAgree(t *testing.T) {
	rules, _ := BuildRules(kv("Reference", "measured", "Output", "status, body, header:A", "Tolerance", "$.t abs 0.5"))
	pa := Parts{Status: h1, Headers: h1, Body: h1}
	ref := []ScenarioOutput{so("S", "", 1, h1, pa, ToleranceValue{Path: "$.t", Rule: 0, Value: 10})}

	same := []ScenarioOutput{so("S", "", 1, h1, pa, ToleranceValue{Path: "$.t", Rule: 0, Value: 10.4})}
	if ok, parts := RecordsAgree(rules, ref, same); !ok || len(parts) != 0 {
		t.Errorf("within tolerance: %v %v", ok, parts)
	}
	far := []ScenarioOutput{so("S", "", 1, h1, pa, ToleranceValue{Path: "$.t", Rule: 0, Value: 10.6})}
	if ok, parts := RecordsAgree(rules, ref, far); ok || !reflect.DeepEqual(parts, []string{"values"}) {
		t.Errorf("outside tolerance: %v %v", ok, parts)
	}
	missing := []ScenarioOutput{so("S", "", 1, h1, pa)}
	if ok, parts := RecordsAgree(rules, ref, missing); ok || !reflect.DeepEqual(parts, []string{"values"}) {
		t.Errorf("a tolerant leaf present on one side only: %v %v", ok, parts)
	}
	diff := []ScenarioOutput{so("S", "", 1, h2, Parts{Status: h1, Headers: h2, Body: h2}, ToleranceValue{Path: "$.t", Rule: 0, Value: 10})}
	if ok, parts := RecordsAgree(rules, ref, diff); ok || !reflect.DeepEqual(parts, []string{"headers", "body"}) {
		t.Errorf("part names, in fixed order: %v %v", ok, parts)
	}
	more := []ScenarioOutput{same[0], so("S", "", 2, h1, pa)}
	if ok, parts := RecordsAgree(rules, ref, more); ok || !reflect.DeepEqual(parts, []string{"samples"}) {
		t.Errorf("a different number of samples: %v %v", ok, parts)
	}
	otherStep := []ScenarioOutput{so("S", "read", 1, h1, pa, ToleranceValue{Path: "$.t", Rule: 0, Value: 10})}
	if ok, parts := RecordsAgree(rules, ref, otherStep); ok || !reflect.DeepEqual(parts, []string{"samples"}) {
		t.Errorf("a different step: %v %v", ok, parts)
	}
	// arrival order does not matter
	r2 := []ScenarioOutput{so("S", "b", 1, h1, pa), so("S", "a", 1, h2, pa)}
	g2 := []ScenarioOutput{so("S", "a", 1, h2, pa), so("S", "b", 1, h1, pa)}
	plain, _ := BuildRules(kv("Reference", "measured"))
	if ok, _ := RecordsAgree(plain, r2, g2); !ok {
		t.Error("rows are compared in (step, sample) order")
	}
}

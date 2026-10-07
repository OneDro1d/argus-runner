package compare

// cmp11_f7_test.go -- ARGUS-CMP-11 fix F7: what the median and a failed run mean in a performance band.
//   - with an even number of runs the median is the mean of the two middle values;
//   - a run in which the load check FAILED still contributes its measured numbers (they are what the system did);
//   - a run that ERRORED on the check did not measure: it contributes none and is counted in could_not_run.

import "testing"

func TestCMP11_F7_WithAnEvenNumberOfRunsTheMedianIsTheMeanOfTheTwoMiddleValues(t *testing.T) {
	if got := Median([]float64{500, 100, 300, 200}); got != 250 {
		t.Errorf("Median of 100, 200, 300, 500 = %v, want 250 (the mean of 200 and 300)", got)
	}
	if got := Median([]float64{3, 1, 2}); got != 2 {
		t.Errorf("Median of 1, 2, 3 = %v, want 2", got)
	}
	in := loadInput(propertyLoad(t, "p95 20%"),
		run("r1", "old", "v", "passed", loadRow("C-1", ln(100))), run("r2", "old", "v", "passed", loadRow("C-1", ln(200))),
		run("r3", "old", "v", "passed", loadRow("C-1", ln(300))), run("r4", "old", "v", "passed", loadRow("C-1", ln(500))),
		run("r5", "new", "v", "passed", loadRow("C-1", ln(100))), run("r6", "new", "v", "passed", loadRow("C-1", ln(150))))
	o := Result(in)
	p := o.Performance[0]
	if p.Reference.Value != 250 || p.Reference.Runs != 4 {
		t.Errorf("reference = %+v, want the mean of the two middle p95 values, 250, over 4 runs", p.Reference)
	}
	if pc := p.Cells[0]; pc.Value != 125 || pc.Runs != 2 {
		t.Errorf("member = %+v, want 125 over 2 runs (the mean of 100 and 150)", pc)
	}
}

func TestCMP11_F7_ARunInWhichTheLoadCheckFailedStillContributesItsNumbersAndAnErroredRunContributesNone(t *testing.T) {
	in := loadInput(propertyLoad(t, "p95 20%"),
		run("r1", "old", "v", "passed", loadRow("C-1", ln(100))),
		run("r2", "new", "v", "passed", loadRow("C-1", ln(100))),
		run("r3", "new", "v", OutcomeFailed, loadRow("C-1", ln(900))),   // its own absolute threshold was breached: it measured
		run("r4", "new", "v", OutcomeErrored, loadRow("C-1", ln(7000)))) // it did not measure: whatever row it left is not a number
	o := Result(in)
	pc := o.Performance[0].Cells[0]
	if pc.Runs != 2 || pc.Value != 500 {
		t.Errorf("member = %+v, want the failed run's 900 and the passed run's 100 (median 500) over 2 runs, the errored run's 7000 not among them", pc)
	}
	cell := cellOf(t, o, "C-1", "new")
	if cell.CouldNotRun != 1 {
		t.Errorf("the errored run is counted in could_not_run: %d, want 1", cell.CouldNotRun)
	}
	// an outcome this build does not know is the same as errored: classify counts it could not run, and so does the band
	in2 := loadInput(propertyLoad(t, "p95 20%"),
		run("r1", "old", "v", "passed", loadRow("C-1", ln(100))),
		run("r2", "new", "v", "passed", loadRow("C-1", ln(100))),
		run("r3", "new", "v", "from_the_future", loadRow("C-1", ln(7000))))
	if pc := Result(in2).Performance[0].Cells[0]; pc.Runs != 1 || pc.Value != 100 {
		t.Errorf("a run with an unknown outcome contributed: %+v", pc)
	}
}

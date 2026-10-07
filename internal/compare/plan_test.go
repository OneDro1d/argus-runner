package compare

import "testing"

// ARGUS-CMP-6: how many whole runs each member gets.
func TestPlanRuns(t *testing.T) {
	fixed := func(rep int) *Rules { return &Rules{Reference: RefFixed, Repeats: rep, Agreement: 100} }
	meas := func(rep int) *Rules { return &Rules{Reference: RefMeasured, Repeats: rep, Agreement: 100} }
	cases := []struct {
		name  string
		rules []*Rules
		want  RunPlan
	}{
		{"no rules at all", nil, RunPlan{Set: 1, Reference: 1, Candidate: 1}},
		{"measured, repeats not declared: the reference still runs twice", []*Rules{meas(1)}, RunPlan{Set: 1, Reference: 2, Candidate: 1}},
		{"measured, repeats 0 (hand built)", []*Rules{meas(0)}, RunPlan{Set: 1, Reference: 2, Candidate: 1}},
		{"measured, repeats 2", []*Rules{meas(2)}, RunPlan{Set: 2, Reference: 2, Candidate: 2}},
		{"measured, repeats 5", []*Rules{meas(5)}, RunPlan{Set: 5, Reference: 5, Candidate: 5}},
		{"the largest repeats of any check", []*Rules{meas(3), meas(7), meas(1)}, RunPlan{Set: 7, Reference: 7, Candidate: 7}},
		{"a check that declares nothing is skipped", []*Rules{nil, meas(4)}, RunPlan{Set: 4, Reference: 4, Candidate: 4}},
		{"no measured check: no control run is asked for", []*Rules{fixed(1)}, RunPlan{Set: 1, Reference: 1, Candidate: 1}},
		{"mixed: the fixed check's repeats set R_set", []*Rules{fixed(4), meas(1)}, RunPlan{Set: 4, Reference: 4, Candidate: 4}},
		{"the cap is 20 runs", []*Rules{meas(20)}, RunPlan{Set: 20, Reference: 20, Candidate: 20}},
		{"above the cap is clamped, never queued", []*Rules{meas(25)}, RunPlan{Set: 20, Reference: 20, Candidate: 20}},
	}
	for _, c := range cases {
		if got := PlanRuns(c.rules); got != c.want {
			t.Errorf("%s: PlanRuns = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestSetRepeatsRefusal(t *testing.T) {
	ok := []PathRules{{Path: "a.md", Rules: &Rules{Repeats: 20}}, {Path: "b.md", Rules: nil}, {Path: "c.md", Rules: &Rules{Repeats: 1}}}
	if got := SetRepeatsRefusal(ok); got != "" {
		t.Fatalf("20 is within the cap: %q", got)
	}
	got := SetRepeatsRefusal([]PathRules{{Path: "a.md", Rules: &Rules{Repeats: 1}}, {Path: "cert/big.md", Rules: &Rules{Repeats: 21}}})
	if got == "" {
		t.Fatal("21 repeats is above the set-wide cap of 20 and must be refused")
	}
	want := "check cert/big.md declares more repeats than the set-wide cap of 20 runs per member"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

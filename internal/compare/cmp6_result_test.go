package compare

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"
)

// ARGUS-CMP-6: the control run, noise, repeats as whole runs, the agreement rate and what n supports.

// outS is a recorded row whose status part and body part carry their own hash.
func outS(id, statusHash, bodyHash string) ScenarioOutput {
	return ScenarioOutput{ScenarioID: id, OutputRecord: OutputRecord{
		V: 1, Sample: 1, State: StateRecorded, Status: 200, Hash: statusHash + bodyHash,
		Parts: Parts{Status: statusHash, Body: bodyHash}, BodyKind: KindJSON,
	}}
}

func TestCMP6_NoiseNamesTheVaryingPartsOnTheCellAndTheCheck(t *testing.T) {
	// three reference runs: the body varies in one, the status in another
	in := baseInput(
		run("r1", "old", "v1", "passed", outS("C-1", hx("a"), hx("1"))),
		run("r2", "old", "v1", "passed", outS("C-1", hx("a"), hx("2"))),
		run("r3", "old", "v1", "passed", outS("C-1", hx("b"), hx("1"))),
		// the candidate equals the reference's FIRST output in all three runs and is still noise
		run("n1", "new", "v2", "passed", outS("C-1", hx("a"), hx("1"))),
		run("n2", "new", "v2", "passed", outS("C-1", hx("a"), hx("1"))),
		run("n3", "new", "v2", "passed", outS("C-1", hx("a"), hx("1"))),
	)
	in.Checks[0].Rules = measured(t, "Repeats", "3")
	o := Result(in)
	ref := o.Checks[0].Reference
	if ref.Stable || ref.Runs != 3 || strings.Join(ref.VaryingParts, ",") != "status,body" {
		t.Fatalf("reference = %+v, want unstable over 3 runs varying status and body", ref)
	}
	const want = "The reference gave 3 different outputs in 3 runs. Mask the field that changes and seal a new set."
	if ref.Sentence != want {
		t.Fatalf("check sentence = %q, want %q", ref.Sentence, want)
	}
	c := cellOf(t, o, "C-1", "new")
	if c.State != StateNoise || c.Control != ControlUnstable || c.Sentence != want {
		t.Fatalf("cell = %+v", c)
	}
	if strings.Join(c.PartsDiffer, ",") != "status,body" {
		t.Fatalf("the cell carries the varying parts: %v", c.PartsDiffer)
	}
	if c.Agreed != 0 {
		t.Fatalf("a noise cell judges nothing: agreed = %d", c.Agreed)
	}
	if o.Verdict != VerdictIncomplete || o.Counts.Noise != 1 {
		t.Fatalf("verdict %s counts %+v", o.Verdict, o.Counts)
	}
}

// A stable reference carries no sentence and no varying parts.
func TestCMP6_StableReferenceHasNoNoiseSentence(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("n1", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	ref := Result(in).Checks[0].Reference
	if !ref.Stable || ref.Sentence != "" || len(ref.VaryingParts) != 0 || ref.Runs != 2 || ref.CouldNotRun != 0 {
		t.Fatalf("reference = %+v", ref)
	}
}

// Nothing auto-masks: the stored rules are exactly what they were, noise or not.
func TestCMP6_NoiseNeverChangesTheRules(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("2"))),
		run("n1", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	before := RulesHash([]PathRules{{Path: "a/C-1.md", Rules: in.Checks[0].Rules}})
	o := Result(in)
	if cellOf(t, o, "C-1", "new").State != StateNoise {
		t.Fatal("setup: the cell must be noise")
	}
	if after := RulesHash([]PathRules{{Path: "a/C-1.md", Rules: in.Checks[0].Rules}}); after != before {
		t.Fatalf("rules_hash moved from %s to %s: a noisy reference must never mask anything", before, after)
	}
	if len(in.Checks[0].Rules.Mask) != 0 {
		t.Fatalf("a mask appeared: %v", in.Checks[0].Rules.Mask)
	}
}

// A reference run that could not run is excluded and named; it is not "a different output".
func TestCMP6_AReferenceRunThatCouldNotRunIsExcludedAndNamed(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "errored", out("C-1", hx("9"))), // the check errored: its output is not evidence
		run("r3", "old", "v1", "passed", out("C-1", hx("1"))),
		run("n1", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	ref := o.Checks[0].Reference
	if !ref.Stable || ref.Runs != 2 || ref.CouldNotRun != 1 {
		t.Fatalf("reference = %+v, want stable over 2 usable runs with 1 named as could not run", ref)
	}
	refCell := cellOf(t, o, "C-1", "old")
	if refCell.Runs != 2 || refCell.CouldNotRun != 1 || !strings.Contains(refCell.Sentence, "1 run could not run and is not counted") {
		t.Fatalf("reference cell = %+v", refCell)
	}
	if c := cellOf(t, o, "C-1", "new"); c.State != StateIdentical {
		t.Fatalf("the candidate is judged against the usable reference runs: %+v", c)
	}
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("a reference gap is a gap: %s", o.Verdict)
	}
}

// Reference runs at different versions are NOT pooled: a reference whose outputs differ BETWEEN versions is
// not "unstable", and the candidate is judged against the chosen version only.
func TestCMP6_ReferenceRunsOfDifferentVersionsAreNotPooled(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "old", "v9", "passed", out("C-1", hx("2"))),
		run("r4", "old", "v9", "passed", out("C-1", hx("2"))),
		run("n1", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	// no choice: the table says so and judges nothing
	o := Result(in)
	if c := cellOf(t, o, "C-1", "new"); c.State != StateNotMeasured || o.Checks[0].Reference.Stable && o.Checks[0].Reference.Runs != 0 {
		t.Fatalf("two reference versions, none chosen: %+v / %+v", c, o.Checks[0].Reference)
	}
	if !hasNote(o, NoteReferenceVersionAmbiguous) {
		t.Fatalf("notes = %v", o.Notes)
	}
	// chosen: v1 is the reference; the v9 runs are their own column, and nothing is "noise"
	in.ReferenceVersion = "v1"
	o = Result(in)
	ref := o.Checks[0].Reference
	if !ref.Stable || ref.Runs != 2 {
		t.Fatalf("reference at v1 = %+v: the v9 runs must not be pooled into it", ref)
	}
	if c := cellOf(t, o, "C-1", "new"); c.State != StateIdentical {
		t.Fatalf("candidate = %+v", c)
	}
	var v9 *Cell
	for i, c := range o.Checks[0].Cells {
		if c.Member == "old" && c.VersionKey == "v9" {
			v9 = &o.Checks[0].Cells[i]
		}
	}
	if v9 == nil || v9.State == StateNoise {
		t.Fatalf("the other reference version is its own column, judged against v1: %+v", v9)
	}
}

// The agreement rate is read per check from that check's own declaration, over the runs that could run.
func TestCMP6_AgreementCountsOnlyRunsThatRan(t *testing.T) {
	refs := []Run{
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
	}
	cands := []Run{
		run("n1", "new", "v2", "passed", out("C-1", hx("1"))),
		run("n2", "new", "v2", "passed", out("C-1", hx("1"))),
		run("n3", "new", "v2", "passed", out("C-1", hx("1"))),
		run("n4", "new", "v2", "errored", out("C-1", hx("1"))),
	}
	in := baseInput(append(append([]Run{}, refs...), cands...)...)
	in.Checks[0].Rules = measured(t, "Repeats", "4")
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if c.Runs != 3 || c.Agreed != 3 || c.CouldNotRun != 1 {
		t.Fatalf("a could-not-run run must be outside n: %+v", c)
	}
	if !strings.Contains(c.Sentence, "1 run could not run and is not counted in n") {
		t.Fatalf("sentence = %q", c.Sentence)
	}
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("a cell with a could-not-run run is a gap: %s", o.Verdict)
	}
	// the same data with the fourth run agreeing is complete
	cands[3] = run("n4", "new", "v2", "passed", out("C-1", hx("1")))
	in = baseInput(append(append([]Run{}, refs...), cands...)...)
	in.Checks[0].Rules = measured(t, "Repeats", "4")
	c = cellOf(t, Result(in), "C-1", "new")
	if c.Runs != 4 || c.Agreed != 4 || c.CouldNotRun != 0 || c.State != StateIdentical || !c.ClaimSupported || Result(in).Verdict != VerdictSame {
		t.Fatalf("4 of 4: %+v verdict %s", c, Result(in).Verdict)
	}
}

// Each check declares its own agreement and its own repeats.
func TestCMP6_AgreementAndRepeatsAreReadPerCheck(t *testing.T) {
	strict, loose := measured(t, "Repeats", "10"), measured(t, "Repeats", "10", "Agreement", "70%")
	in := Input{SetHash: setH, Members: twoMembers(), Checks: []Check{{ID: "A-1", Path: "a/A-1.md", Rules: strict}, {ID: "B-1", Path: "a/B-1.md", Rules: loose}}}
	for i := 0; i < 2; i++ {
		in.Runs = append(in.Runs, run("r"+string(rune('0'+i)), "old", "v1", "passed", out("A-1", hx("1")), out("B-1", hx("1"))))
	}
	for i := 0; i < 10; i++ {
		h := hx("1")
		if i < 2 { // 2 of 10 disagree on both checks
			h = hx("2")
		}
		in.Runs = append(in.Runs, run("n"+string(rune('a'+i)), "new", "v2", "passed", out("A-1", h), out("B-1", h)))
	}
	o := Result(in)
	a, b := cellOf(t, o, "A-1", "new"), cellOf(t, o, "B-1", "new")
	if a.State != StateDiffers || b.State != StateIdentical || a.Agreed != 8 || b.Agreed != 8 || a.Runs != 10 || b.Runs != 10 {
		t.Fatalf("A (100%%) %+v, B (70%%) %+v", a, b)
	}
}

// What n supports. The sentences are closed templates; every percentage is printed with its n.
func TestCMP6_EverySentenceCarriesItsN(t *testing.T) {
	pct := regexp.MustCompile(`\d%`)
	runsWord := regexp.MustCompile(`(\d+ runs?|n = \d+)`)
	for _, agree := range []int{0, 1, 3, 5} {
		in := baseInput()
		in.Checks[0].Rules = measured(t, "Repeats", "5", "Agreement", "90%")
		in.Runs = append(in.Runs, run("r1", "old", "v1", "passed", out("C-1", hx("1"))), run("r2", "old", "v1", "passed", out("C-1", hx("1"))))
		for i := 0; i < 5; i++ {
			h := hx("1")
			if i >= agree {
				h = hx("2")
			}
			in.Runs = append(in.Runs, run("n"+string(rune('a'+i)), "new", "v2", "passed", out("C-1", h)))
		}
		for _, c := range Result(in).Checks[0].Cells {
			for _, s := range []string{c.Sentence, c.Supports, c.Unsupported} {
				if pct.MatchString(s) && !runsWord.MatchString(s) {
					t.Errorf("agree=%d: %q prints a percentage without its n", agree, s)
				}
			}
		}
	}
}

// n = 0 must never produce NaN, Inf or a division by zero: not in the numbers, not in the encoded output.
func TestCMP6_NoRunsIsNeverNaNOrInf(t *testing.T) {
	for _, name := range []string{"no run at all", "every run could not run", "queued only"} {
		in := baseInput()
		in.Checks[0].Rules = measured(t, "Repeats", "3", "Agreement", "95%")
		switch name {
		case "every run could not run":
			in.Runs = []Run{run("r1", "old", "v1", "passed", out("C-1", hx("1"))), run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
				run("n1", "new", "v2", "errored", out("C-1", hx("1")))}
		case "queued only":
			in.Runs = []Run{{ID: "q1", Member: "old", Status: "queued"}, {ID: "q2", Member: "new", Status: "queued"}}
		}
		o := Result(in)
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatalf("%s: the output does not encode (a NaN or Inf?): %v", name, err)
		}
		if strings.Contains(string(b), "NaN") || strings.Contains(string(b), "Inf") {
			t.Errorf("%s: %s", name, b)
		}
		for _, c := range o.Checks[0].Cells {
			if c.Runs != 0 && c.State != StateIdentical && c.State != StateDiffers && c.State != StateNoise {
				t.Errorf("%s: %+v", name, c)
			}
		}
		if o.Verdict == VerdictSame {
			t.Errorf("%s: no run can never be 'same'", name)
		}
	}
	for _, f := range []float64{UpperBound95(0, 0), UpperBound95(0, -3), UpperBound95(5, 0)} {
		if math.IsNaN(f) || math.IsInf(f, 0) || f != 1 {
			t.Errorf("UpperBound95 at n <= 0 = %v, want exactly 1", f)
		}
	}
	if SupportsSentence(0, 0) != "" || SupportsFor(0, 0, 100) != "" {
		t.Error("n = 0 supports nothing and says nothing")
	}
}

// A 100% rule: no finite number of runs proves it, and the cell says what n supports instead.
func TestCMP6_AHundredPercentRuleSaysNoNumberOfRunsProvesIt(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
	)
	for i := 0; i < 5; i++ {
		in.Runs = append(in.Runs, run("n"+string(rune('a'+i)), "new", "v2", "passed", out("C-1", hx("1"))))
	}
	in.Checks[0].Rules = measured(t, "Repeats", "5")
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateIdentical || !c.ClaimSupported {
		t.Fatalf("5 of 5 at the default 100%% is identical and met: %+v", c)
	}
	if !strings.Contains(c.Supports, "with 5 runs and no disagreement, the true disagreement rate is below 46% (95% confidence)") ||
		!strings.Contains(c.Supports, "no number of runs proves a 100% rate") {
		t.Fatalf("supports = %q", c.Supports)
	}
	if got := SupportsFor(0, 5, 90); got != SupportsSentence(0, 5) {
		t.Fatalf("a rule below 100%% carries no such clause: %q", got)
	}
	if got := UnsupportedSentence(5, 100); !strings.Contains(got, "no number of runs proves 100%") || strings.Contains(got, "needs 1 run") {
		t.Fatalf("unsupported sentence for 100%% = %q", got)
	}
}

// The numeric table the report pastes: UpperBound95 and MinRuns, printed from the same functions the tests assert.
func TestCMP6_StatisticsTable(t *testing.T) {
	t.Log("UpperBound95(d, n): exact one-sided 95% Clopper-Pearson upper bound on the true disagreement rate")
	for _, c := range [][2]int{{0, 1}, {0, 3}, {0, 5}, {0, 20}, {0, 29}, {0, 100}, {0, 299}, {1, 5}, {1, 20}, {2, 20}, {3, 10}, {5, 20}, {10, 100}} {
		t.Logf("  d=%-3d n=%-4d upper bound = %.6f   %s", c[0], c[1], UpperBound95(c[0], c[1]), SupportsSentence(c[0], c[1]))
	}
	t.Log("MinRuns(p): fewest all-agreeing runs that support p at 95% confidence = ceil(ln 0.05 / ln p)")
	want := map[float64]int{50: 5, 80: 14, 90: 29, 95: 59, 99: 299, 99.9: 2995, 100: 1}
	for _, p := range []float64{50, 80, 90, 95, 99, 99.9, 100} {
		t.Logf("  p=%-5v MinRuns = %d", p, MinRuns(p))
		if MinRuns(p) != want[p] {
			t.Errorf("MinRuns(%v) = %d, want %d", p, MinRuns(p), want[p])
		}
	}
}

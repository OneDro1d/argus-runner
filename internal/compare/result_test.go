package compare

import (
	"math/rand"
	"strings"
	"testing"
)

const setH = "set-hash-1"

func hx(c string) string { return strings.Repeat(c, 64) }

func measured(t *testing.T, kvs ...string) *Rules {
	t.Helper()
	return mustRules(t, append([]string{"Reference", "measured"}, kvs...)...)
}

// out builds one recorded row for check id with the given body-part hash; the total hash follows it.
func out(id, bodyHash string) ScenarioOutput {
	return ScenarioOutput{ScenarioID: id, OutputRecord: OutputRecord{
		V: 1, Sample: 1, State: StateRecorded, Status: 200, Hash: bodyHash,
		Parts: Parts{Status: hx("a"), Body: bodyHash}, BodyKind: KindJSON,
	}}
}

func run(id, member, version string, outcome string, rows ...ScenarioOutput) Run {
	oc := map[string]string{}
	for _, r := range rows {
		oc[r.ScenarioID] = outcome
	}
	return Run{ID: id, Member: member, VersionKey: version, Status: "completed", SetHash: setH, Outcomes: oc, Outputs: rows}
}

func twoMembers() []Member {
	return []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}}
}

func cellOf(t *testing.T, o Output, check, member string) Cell {
	t.Helper()
	for _, c := range o.Checks {
		if c.ID != check {
			continue
		}
		for _, cell := range c.Cells {
			if cell.Member == member {
				return cell
			}
		}
	}
	t.Fatalf("no cell for %s/%s in %+v", check, member, o.Checks)
	return Cell{}
}

// baseInput carries one measured check with default rules; tests that need other rules replace
// in.Checks[0].Rules.
func baseInput(runs ...Run) Input {
	r, _ := BuildRules(kv("Reference", "measured"))
	return Input{
		SetHash: setH,
		Checks:  []Check{{ID: "C-1", Path: "a/C-1.md", Rules: r}},
		Members: twoMembers(),
		Runs:    runs,
	}
}

func TestResult_SameWhenEveryCellIdentical(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if c.State != StateIdentical || c.Runs != 1 || c.Agreed != 1 || c.Control != ControlStable || !c.ClaimSupported {
		t.Fatalf("cell = %+v", c)
	}
	if o.Verdict != VerdictSame {
		t.Fatalf("verdict = %s, counts %+v", o.Verdict, o.Counts)
	}
	if o.Counts.Identical != 1 || o.Counts.Checks != 1 || o.Counts.Members != 2 || o.Counts.Runs != 3 {
		t.Fatalf("counts = %+v", o.Counts)
	}
}

func TestResult_Differs(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("2"))),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if c.State != StateDiffers || c.Agreed != 0 || strings.Join(c.PartsDiffer, ",") != "body" {
		t.Fatalf("cell = %+v", c)
	}
	if o.Verdict != VerdictDifferences || o.Counts.Differs != 1 {
		t.Fatalf("verdict %s counts %+v", o.Verdict, o.Counts)
	}
	if c.ReferenceHash == "" || c.MemberHash == "" || c.ReferenceHash == c.MemberHash {
		t.Fatalf("hashes for approval pinning: %+v", c)
	}
}

func TestResult_UnstableReferenceMakesEveryCandidateNoise(t *testing.T) {
	// the candidate equals one of the reference's two outputs and is still noise (design A4)
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("2"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if c.State != StateNoise || c.Control != ControlUnstable {
		t.Fatalf("cell = %+v", c)
	}
	ref := o.Checks[0].Reference
	if ref.Stable || ref.Runs != 2 || strings.Join(ref.VaryingParts, ",") != "body" {
		t.Fatalf("reference = %+v", ref)
	}
	if o.Verdict != VerdictIncomplete || o.Counts.Noise != 1 {
		t.Fatalf("verdict %s counts %+v", o.Verdict, o.Counts)
	}
	if !strings.Contains(c.Sentence, "Mask the field that changes and seal a new set") {
		t.Fatalf("sentence = %q", c.Sentence)
	}
}

func TestResult_SingleReferenceRunIsUntested(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if c.State != StateIdentical || c.Control != ControlUntested {
		t.Fatalf("cell = %+v", c)
	}
	if !hasNote(o, NoteReferenceSingleRun) {
		t.Fatalf("notes = %v", o.Notes)
	}
}

func hasNote(o Output, n string) bool {
	for _, x := range o.Notes {
		if x == n {
			return true
		}
	}
	return false
}

// There is no path from "absent" to "identical".
func TestResult_AbsentIsNeverIdentical(t *testing.T) {
	ref := run("r1", "old", "v1", "passed", out("C-1", hx("1")))
	cases := map[string]Run{
		"old executor, no outputs":  {ID: "r3", Member: "new", VersionKey: "v2", Status: "completed", SetHash: setH, Outcomes: map[string]string{"C-1": "passed"}},
		"row for another check":     run("r3", "new", "v2", "passed", out("C-OTHER", hx("1"))),
		"row not recorded":          {ID: "r3", Member: "new", VersionKey: "v2", Status: "completed", SetHash: setH, Outcomes: map[string]string{"C-1": "passed"}, Outputs: []ScenarioOutput{{ScenarioID: "C-1", OutputRecord: OutputRecord{V: 1, Sample: 1, State: StateNotRecorded, Reason: ReasonBodyTooLarge}}}},
		"run not terminal yet":      {ID: "r3", Member: "new", VersionKey: "v2", Status: "running", SetHash: setH},
		"check not reported by run": {ID: "r3", Member: "new", VersionKey: "v2", Status: "completed", SetHash: setH, Outcomes: map[string]string{}},
		"no run at all":             {},
	}
	for name, r := range cases {
		runs := []Run{ref}
		if r.ID != "" {
			runs = append(runs, r)
		}
		in := baseInput(runs...)
		in.Checks[0].Rules = measured(t)
		o := Result(in)
		c := cellOf(t, o, "C-1", "new")
		if c.State != StateNotMeasured {
			t.Errorf("%s: state = %s, want not_measured (%+v)", name, c.State, c)
		}
		if o.Verdict == VerdictSame || o.Verdict == VerdictSameWithApproved {
			t.Errorf("%s: verdict = %s, must never be green", name, o.Verdict)
		}
	}
}

func TestResult_CouldNotRun(t *testing.T) {
	ref := run("r1", "old", "v1", "passed", out("C-1", hx("1")))
	bad := map[string]Run{
		"check errored":  run("r3", "new", "v2", "errored", out("C-1", hx("1"))),
		"run failed":     {ID: "r3", Member: "new", VersionKey: "v2", Status: "failed", SetHash: setH},
		"run abandoned":  {ID: "r3", Member: "new", VersionKey: "v2", Status: "abandoned", SetHash: setH},
		"other set hash": {ID: "r3", Member: "new", VersionKey: "v2", Status: "completed", SetHash: "someone-elses", Outcomes: map[string]string{"C-1": "passed"}, Outputs: []ScenarioOutput{out("C-1", hx("1"))}},
	}
	for name, r := range bad {
		in := baseInput(ref, r)
		in.Checks[0].Rules = measured(t)
		o := Result(in)
		c := cellOf(t, o, "C-1", "new")
		if c.State != StateCouldNotRun || c.CouldNotRun != 1 || c.Runs != 0 {
			t.Errorf("%s: %+v", name, c)
		}
		if o.Verdict != VerdictIncomplete {
			t.Errorf("%s: verdict = %s", name, o.Verdict)
		}
	}
	// a wrong-set run contributes nothing even when its outputs would have matched
	in := baseInput(ref, bad["other set hash"])
	in.Checks[0].Rules = measured(t)
	if cellOf(t, Result(in), "C-1", "new").Agreed != 0 {
		t.Error("a run of a different set must not count as agreeing")
	}
}

func TestResult_SeedFailureMakesTheRunsOtherCellsCouldNotRun(t *testing.T) {
	rules := measured(t)
	checks := []Check{
		{ID: "00-SEED", Path: "a/00-SEED.md", Rules: nil, Seed: true},
		{ID: "C-1", Path: "a/C-1.md", Rules: rules},
	}
	mk := func(id, member, seedOutcome string, hash string) Run {
		r := run(id, member, "v", "passed", out("C-1", hash))
		r.Outcomes["00-SEED"] = seedOutcome
		return r
	}
	in := Input{SetHash: setH, Checks: checks, Members: twoMembers(), Runs: []Run{
		mk("r1", "old", "passed", hx("1")), mk("r2", "new", "failed", hx("1")),
	}}
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if c.State != StateCouldNotRun {
		t.Fatalf("a failed seed must stop the run's cells: %+v", c)
	}
	if !strings.Contains(c.Sentence, "starting state not established") {
		t.Fatalf("sentence = %q", c.Sentence)
	}
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %s", o.Verdict)
	}
	// and with a good seed the same data is identical
	in.Runs[1] = mk("r2", "new", "passed", hx("1"))
	if cellOf(t, Result(in), "C-1", "new").State != StateIdentical {
		t.Fatal("a passing seed lets the cell be judged")
	}
	if hasNote(Result(in), NoteNoSeedStep) {
		t.Fatal("a set with a seed check carries no 'no seed step' note")
	}
	plain := Result(baseInput())
	if !hasNote(plain, NoteNoSeedStep) {
		t.Fatalf("a set without a seed check says so once: %v", plain.Notes)
	}
}

func TestResult_SeedChecksAreNotInTheRollUp(t *testing.T) {
	seedRules := measured(t)
	in := Input{SetHash: setH, Members: twoMembers(), Checks: []Check{
		{ID: "00-SEED", Path: "a/00.md", Rules: seedRules, Seed: true},
		{ID: "C-1", Path: "a/C-1.md", Rules: measured(t)},
	}}
	mk := func(id, member string, seedHash string) Run {
		r := run(id, member, "v", "passed", out("00-SEED", seedHash), out("C-1", hx("1")))
		return r
	}
	in.Runs = []Run{mk("r1", "old", hx("5")), mk("r2", "old", hx("5")), mk("r3", "new", hx("6"))}
	o := Result(in)
	if cellOf(t, o, "00-SEED", "new").State != StateDiffers {
		t.Fatal("the seed check itself is still judged and reported")
	}
	// the counts are over non-seed cells; but the seed's own comparison not holding is never read as same
	if o.Counts.Differs != 0 || o.Counts.Identical != 1 {
		t.Fatalf("the counts are over non-seed cells: %s %+v", o.Verdict, o.Counts)
	}
	if o.Verdict != VerdictIncomplete || !hasNote(o, NoteSeedDidNotHold) {
		t.Fatalf("a seed comparison that does not hold reads incomplete with the closed note: %s %v", o.Verdict, o.Notes)
	}
}

func TestResult_AgreementRateAndStatistics(t *testing.T) {
	rules := measured(t, "Repeats", "5", "Agreement", "80%")
	mkRuns := func(agree int) []Run {
		runs := []Run{
			run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
			run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		}
		for i := 0; i < 5; i++ {
			h := hx("1")
			if i >= agree {
				h = hx("2")
			}
			runs = append(runs, run("n"+string(rune('a'+i)), "new", "v2", "passed", out("C-1", h)))
		}
		return runs
	}
	in := baseInput(mkRuns(4)...)
	in.Checks[0].Rules = rules
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateIdentical || c.Runs != 5 || c.Agreed != 4 {
		t.Fatalf("4 of 5 at 80%%: %+v", c)
	}
	if c.ClaimSupported {
		t.Fatalf("5 runs cannot support 80%% (needs 14 clean runs): %+v", c)
	}
	if !strings.Contains(c.Unsupported, "cannot support 80%") {
		t.Fatalf("unsupported sentence = %q", c.Unsupported)
	}
	if Result(in).Verdict != VerdictIncomplete {
		t.Fatalf("an unsupported claim keeps the verdict from being 'same': %s", Result(in).Verdict)
	}
	in2 := baseInput(mkRuns(3)...)
	in2.Checks[0].Rules = rules
	c2 := cellOf(t, Result(in2), "C-1", "new")
	if c2.State != StateDiffers || c2.Agreed != 3 {
		t.Fatalf("3 of 5 at 80%%: %+v", c2)
	}
	if !strings.Contains(c.Sentence, "4 of 5 runs") || !strings.Contains(c.Sentence, "n = 5") {
		t.Fatalf("every percentage is printed with its n: %q", c.Sentence)
	}
	if !strings.Contains(c.Supports, "true disagreement rate is below") {
		t.Fatalf("supports sentence = %q", c.Supports)
	}
}

func TestResult_FewerRunsThanRepeatsIsNotComplete(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t, "Repeats", "3")
	o := Result(in)
	c := cellOf(t, o, "C-1", "new")
	if !c.Short || o.Verdict != VerdictIncomplete {
		t.Fatalf("1 of 3 declared repeats: %+v verdict %s", c, o.Verdict)
	}
}

func TestResult_VersionsAreColumns(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
		run("r4", "new", "v3", "passed", out("C-1", hx("2"))),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	var states []string
	for _, c := range o.Checks[0].Cells {
		if c.Member == "new" {
			states = append(states, c.VersionKey+"="+string(c.State))
		}
	}
	if strings.Join(states, ",") != "v2=identical,v3=differs" {
		t.Fatalf("one cell per member version, sorted: %v", states)
	}
}

func TestResult_ReferenceVersionAmbiguousOrAbsent(t *testing.T) {
	two := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v9", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	two.Checks[0].Rules = measured(t)
	o := Result(two)
	if cellOf(t, o, "C-1", "new").State != StateNotMeasured || !hasNote(o, NoteReferenceVersionAmbiguous) {
		t.Fatalf("two reference versions and no choice: %v", o.Notes)
	}
	two.ReferenceVersion = "v1"
	if cellOf(t, Result(two), "C-1", "new").State != StateIdentical {
		t.Fatal("an explicit reference version resolves it")
	}
	none := baseInput(run("r3", "new", "v2", "passed", out("C-1", hx("1"))))
	none.Checks[0].Rules = measured(t)
	if cellOf(t, Result(none), "C-1", "new").State != StateNotMeasured {
		t.Fatal("no reference run: the candidate cannot be judged")
	}
	nomember := baseInput(run("r3", "new", "v2", "passed", out("C-1", hx("1"))))
	nomember.Members = []Member{{Name: "new", Role: RoleCandidate}}
	nomember.Checks[0].Rules = measured(t)
	o3 := Result(nomember)
	if cellOf(t, o3, "C-1", "new").State != StateNotMeasured || !hasNote(o3, NoteNoReferenceMember) {
		t.Fatalf("a measured check needs a reference member: %v", o3.Notes)
	}
}

func TestResult_FixedAndProperty(t *testing.T) {
	prop := mustRules(t, "Reference", "property", "Repeats", "5", "Agreement", "80%")
	in := Input{SetHash: setH, Checks: []Check{{ID: "P-1", Path: "a/P-1.md", Rules: prop}}, Members: twoMembers()}
	for i, o := range []string{"passed", "passed", "passed", "passed", "failed"} {
		in.Runs = append(in.Runs, Run{ID: "n" + string(rune('a'+i)), Member: "new", VersionKey: "v", Status: "completed", SetHash: setH, Outcomes: map[string]string{"P-1": o}})
	}
	in.Runs = append(in.Runs, Run{ID: "o1", Member: "old", VersionKey: "v", Status: "completed", SetHash: setH, Outcomes: map[string]string{"P-1": "passed"}})
	o := Result(in)
	c := cellOf(t, o, "P-1", "new")
	if c.State != StateIdentical || c.Runs != 5 || c.Agreed != 4 {
		t.Fatalf("property held in 4 of 5 at 80%%: %+v", c)
	}
	if !strings.Contains(c.Sentence, "held") {
		t.Fatalf("fixed/property wording differs from measured: %q", c.Sentence)
	}
	oc := cellOf(t, o, "P-1", "old")
	if oc.Reference {
		t.Error("a fixed/property check judges every member the same way: no 'reference' cell")
	}
	if o.Checks[0].Claim != ClaimProperty {
		t.Errorf("claim = %s", o.Checks[0].Claim)
	}
	fixed := mustRules(t, "Reference", "fixed")
	in2 := Input{SetHash: setH, Checks: []Check{{ID: "F-1", Path: "a/F-1.md", Rules: fixed}}, Members: []Member{{Name: "solo", Role: RoleCandidate}},
		Runs: []Run{{ID: "x", Member: "solo", VersionKey: "v", Status: "completed", SetHash: setH, Outcomes: map[string]string{"F-1": "failed"}}}}
	o2 := Result(in2)
	if cellOf(t, o2, "F-1", "solo").State != StateDiffers || o2.Verdict != VerdictDifferences {
		t.Fatalf("a failed fixed check differs: %+v", o2.Verdict)
	}
	if o2.Checks[0].Claim != ClaimFixed {
		t.Errorf("claim = %s", o2.Checks[0].Claim)
	}
	// fixed needs no reference member at all, and a roll-up of passing cells is 'same'
	in2.Runs[0].Outcomes["F-1"] = "passed"
	if Result(in2).Verdict != VerdictSame {
		t.Fatalf("verdict = %s", Result(in2).Verdict)
	}
}

func TestResult_Approvals(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("2"))),
	)
	in.Checks[0].Rules = measured(t)
	first := Result(in)
	c := cellOf(t, first, "C-1", "new")
	ap := Approval{Check: "C-1", Member: "new", VersionKey: "v2", ReferenceHash: c.ReferenceHash, MemberHash: c.MemberHash}
	in.Approvals = []Approval{ap}
	o := Result(in)
	c2 := cellOf(t, o, "C-1", "new")
	if c2.State != StateDiffers || !c2.Approved || o.Verdict != VerdictSameWithApproved || o.Counts.Approved != 1 || o.Counts.Differs != 0 {
		t.Fatalf("approved: %+v verdict %s counts %+v", c2, o.Verdict, o.Counts)
	}
	// pinned to both hashes: either side moving voids it
	for name, mod := range map[string]func(a *Approval){
		"reference moved": func(a *Approval) { a.ReferenceHash = hx("9") },
		"member moved":    func(a *Approval) { a.MemberHash = hx("9") },
		"other member":    func(a *Approval) { a.Member = "x" },
		"other version":   func(a *Approval) { a.VersionKey = "v9" },
		"other check":     func(a *Approval) { a.Check = "C-2" },
	} {
		a := ap
		mod(&a)
		in.Approvals = []Approval{a}
		o := Result(in)
		if o.Verdict != VerdictDifferences || cellOf(t, o, "C-1", "new").Approved {
			t.Errorf("%s: approval must not apply (%s)", name, o.Verdict)
		}
	}
	// only a differing cell can be approved
	same := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	same.Checks[0].Rules = measured(t)
	sc := cellOf(t, Result(same), "C-1", "new")
	same.Approvals = []Approval{{Check: "C-1", Member: "new", VersionKey: "v2", ReferenceHash: sc.ReferenceHash, MemberHash: sc.MemberHash}}
	so := Result(same)
	if so.Verdict != VerdictSame || cellOf(t, so, "C-1", "new").Approved {
		t.Fatalf("an approval on an identical cell does nothing: %s", so.Verdict)
	}
}

func TestResult_DifferencesBeatIncomplete(t *testing.T) {
	in := Input{SetHash: setH, Members: []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}},
		Checks: []Check{{ID: "A", Path: "a", Rules: measured(t)}, {ID: "B", Path: "b", Rules: measured(t)}}}
	in.Runs = []Run{
		run("r1", "old", "v", "passed", out("A", hx("1")), out("B", hx("1"))),
		run("r2", "new", "v2", "passed", out("A", hx("2"))), // B unrecorded -> not_measured
	}
	o := Result(in)
	if o.Verdict != VerdictDifferences {
		t.Fatalf("a real difference is reported even with gaps elsewhere: %s", o.Verdict)
	}
	if o.Counts.NotMeasured != 1 || o.Counts.Differs != 1 {
		t.Fatalf("counts = %+v", o.Counts)
	}
}

func TestResult_NoJudgedCellsIsIncomplete(t *testing.T) {
	o := Result(Input{SetHash: setH, Members: twoMembers()})
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("nothing judged is never 'same': %s", o.Verdict)
	}
	o2 := Result(Input{SetHash: setH, Members: []Member{{Name: "old", Role: RoleReference}}, Checks: []Check{{ID: "C-1", Path: "a", Rules: measured(t)}},
		Runs: []Run{run("r1", "old", "v", "passed", out("C-1", hx("1")))}})
	if o2.Verdict != VerdictIncomplete {
		t.Fatalf("a reference with no candidate compares nothing: %s", o2.Verdict)
	}
}

func TestResult_ChecksWithoutRulesAreNotPartOfTheComparison(t *testing.T) {
	in := baseInput(run("r1", "old", "v", "passed", out("C-1", hx("1"))), run("r2", "new", "v", "passed", out("C-1", hx("1"))))
	in.Checks = append(in.Checks, Check{ID: "PLAIN", Path: "z", Rules: nil})
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	if len(o.Checks) != 1 || o.Counts.Checks != 1 {
		t.Fatalf("checks = %d counts %+v", len(o.Checks), o.Counts)
	}
}

func TestResult_ChainRowsAreOneCellAndBothStepsCount(t *testing.T) {
	mk := func(id, member string, h1s, h2s string) Run {
		a, b := out("C-1", h1s), out("C-1", h2s)
		a.Step, b.Step = "create", "read"
		r := run(id, member, "v", "passed")
		r.Outcomes["C-1"] = "passed"
		r.Outputs = []ScenarioOutput{a, b}
		return r
	}
	in := baseInput(mk("r1", "old", hx("1"), hx("2")), mk("r2", "old", hx("1"), hx("2")), mk("r3", "new", hx("1"), hx("3")))
	in.Checks[0].Rules = measured(t)
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateDiffers || strings.Join(c.PartsDiffer, ",") != "body" {
		t.Fatalf("one differing step differs the check: %+v", c)
	}
}

func TestResult_ToleranceDecidesAgreement(t *testing.T) {
	rules := measured(t, "Tolerance", "$.t abs 0.5")
	mk := func(id, member string, v float64) Run {
		o := out("C-1", hx("1"))
		o.Values = []ToleranceValue{{Path: "$.t", Rule: 0, Value: v}}
		return run(id, member, "v", "passed", o)
	}
	for _, c := range []struct {
		v    float64
		want CellState
	}{{10.4, StateIdentical}, {10.6, StateDiffers}} {
		in := baseInput(mk("r1", "old", 10), mk("r2", "old", 10), mk("r3", "new", c.v))
		in.Checks[0].Rules = rules
		cell := cellOf(t, Result(in), "C-1", "new")
		if cell.State != c.want {
			t.Errorf("value %v: %+v, want %s", c.v, cell, c.want)
		}
		if c.want == StateDiffers && strings.Join(cell.PartsDiffer, ",") != "values" {
			t.Errorf("parts = %v", cell.PartsDiffer)
		}
	}
}

func TestResult_Performance(t *testing.T) {
	rules := measured(t, "Not Worse Than", "p95 20%")
	mk := func(id, member string, p95 float64) Run {
		o := out("C-1", hx("1"))
		o.Load = &LoadNumbers{Samples: 50, P50Ms: p95 / 2, P95Ms: p95, P99Ms: p95 * 2, ErrorRate: 0.01}
		return run(id, member, "v", "passed", o)
	}
	for _, c := range []struct {
		p95  float64
		want BandState
		v    Verdict
	}{{115, BandNotWorse, VerdictSame}, {130, BandWorse, VerdictDifferences}} {
		in := baseInput(mk("r1", "old", 100), mk("r2", "old", 100), mk("r3", "new", c.p95))
		in.Checks[0].Rules = rules
		o := Result(in)
		if len(o.Performance) != 1 || len(o.Performance[0].Cells) != 1 {
			t.Fatalf("performance = %+v", o.Performance)
		}
		pc := o.Performance[0]
		if pc.Cells[0].State != c.want || pc.Metric != "p95" || pc.Reference.Value != 100 || pc.Reference.Runs != 2 || pc.Cells[0].Value != c.p95 {
			t.Errorf("p95 %v: %+v", c.p95, pc)
		}
		if o.Verdict != c.v {
			t.Errorf("p95 %v: verdict %s, want %s", c.p95, o.Verdict, c.v)
		}
	}
	// no load numbers on the member: not measured, never 'same'
	in := baseInput(mk("r1", "old", 100), run("r3", "new", "v", "passed", out("C-1", hx("1"))))
	in.Checks[0].Rules = rules
	o := Result(in)
	if o.Performance[0].Cells[0].State != BandNotMeasured || o.Verdict != VerdictIncomplete {
		t.Fatalf("%+v verdict %s", o.Performance, o.Verdict)
	}
}

func TestResult_HashIsStableAndOrderIndependent(t *testing.T) {
	runs := []Run{
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
		run("r4", "new", "v3", "passed", out("C-1", hx("2"))),
		run("r5", "new", "v3", "passed", out("C-1", hx("2"))),
	}
	in := baseInput(runs...)
	in.Checks[0].Rules = measured(t)
	want := Result(in).Hash()
	if len(want) != 64 {
		t.Fatalf("hash = %q", want)
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20; i++ {
		perm := append([]Run(nil), runs...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		in2 := baseInput(perm...)
		in2.Checks[0].Rules = measured(t)
		if got := Result(in2).Hash(); got != want {
			t.Fatalf("shuffle %d changed the result hash", i)
		}
	}
	runs[2] = run("r3", "new", "v2", "passed", out("C-1", hx("3")))
	in3 := baseInput(runs...)
	in3.Checks[0].Rules = measured(t)
	if Result(in3).Hash() == want {
		t.Fatal("a different output must change the result hash")
	}
}

func TestResult_NoBodyOrHeaderValueInTheOutput(t *testing.T) {
	// the Output type has no place for response content: sentences are closed templates
	in := baseInput(run("r1", "old", "v1", "passed", out("C-1", hx("1"))), run("r3", "new", "v2", "passed", out("C-1", hx("2"))))
	in.Checks[0].Rules = measured(t)
	j := Result(in).JSON()
	for _, bad := range []string{"CANARY", "body_b64"} {
		if strings.Contains(string(j), bad) {
			t.Errorf("unexpected %q", bad)
		}
	}
}

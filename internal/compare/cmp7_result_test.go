package compare

// cmp7_result_test.go -- ARGUS-CMP-7 ( / -12): fixed and property claims judged per member from the
// check's own outcome, beside measured ones, and the seed rule judged per RUN. Pure: no store, no clock.

import (
	"strings"
	"testing"
)

// runOut is a terminal run of member with the given per-check outcomes and recorded rows.
//
// ARGUS-CMP-14: it stores status `completed` even beside a `failed` outcome, a pair the executor never sends (it
// sends status `failed` with the outcomes). It stays because Result judges a run from its outcomes, never from
// which of the two statuses carried them, and cmp14_result_test.go (ranFailed) runs the same inputs with the REAL
// pair; the state is reachable only as a hand-edited row, which is why a status of `completed` here proves nothing
// about the wire.
func runOut(id, member string, outcomes map[string]string, rows ...ScenarioOutput) Run {
	return Run{ID: id, Member: member, VersionKey: "v", Status: "completed", SetHash: setH, Outcomes: outcomes, Outputs: rows}
}

func claimIn(rules *Rules, checkID string, members []Member, runs ...Run) Input {
	return Input{SetHash: setH, Checks: []Check{{ID: checkID, Path: "a/" + checkID + ".md", Rules: rules}}, Members: members, Runs: runs}
}

func oneMember() []Member { return []Member{{Name: "solo", Role: RoleCandidate}} }

func outcomesRuns(member, check string, outcomes ...string) []Run {
	var runs []Run
	for i, o := range outcomes {
		runs = append(runs, runOut(member+"-r"+string(rune('a'+i)), member, map[string]string{check: o}))
	}
	return runs
}

// ── what a claim cell says ─────────────────────────────────────────────────────────────────────────

func TestCMP7_AFixedCellCarriesTheStatisticsRuleOfAHundredPercentClaim(t *testing.T) {
	in := claimIn(mustRules(t, "Reference", "fixed"), "F-1", oneMember(), outcomesRuns("solo", "F-1", "passed", "passed", "passed")...)
	c := cellOf(t, Result(in), "F-1", "solo")
	if c.State != StateIdentical || c.Runs != 3 || c.Agreed != 3 || !c.ClaimSupported {
		t.Fatalf("cell = %+v", c)
	}
	// the same sentence a measured 100% cell carries: the rule is about these runs
	if !strings.Contains(c.Supports, "no number of runs proves a 100% rate") {
		t.Errorf("supports = %q: a 100%% claim must say no number of runs proves it", c.Supports)
	}
}

func TestCMP7_AClaimCellNamesTheRunsThatCouldNotRunAndDoesNotCountThem(t *testing.T) {
	in := claimIn(mustRules(t, "Reference", "property", "Agreement", "60%", "Repeats", "4"), "P-1", oneMember(),
		outcomesRuns("solo", "P-1", "passed", "passed", "failed", "errored")...)
	c := cellOf(t, Result(in), "P-1", "solo")
	if c.Runs != 3 || c.Agreed != 2 || c.CouldNotRun != 1 || c.State != StateIdentical {
		t.Fatalf("cell = %+v, want n=3 agreed=2 could_not_run=1, identical at 60%%", c)
	}
	if !strings.Contains(c.Sentence, "1 run could not run and is not counted in n") {
		t.Errorf("sentence = %q: the excluded run must be named", c.Sentence)
	}
	if !strings.Contains(c.Sentence, "The property held in 2 of 3 runs (n = 3)") {
		t.Errorf("sentence = %q", c.Sentence)
	}
}

func TestCMP7_FixedDefaultsToEveryRunAndAnErroredRunIsNeverAFailure(t *testing.T) {
	// 9 of 10 passed and no Agreement declared: 100% is the default, so the claim does not hold
	c := cellOf(t, Result(claimIn(mustRules(t, "Reference", "fixed"), "F-1", oneMember(),
		outcomesRuns("solo", "F-1", "passed", "passed", "passed", "passed", "passed", "passed", "passed", "passed", "passed", "failed")...)), "F-1", "solo")
	if c.State != StateDiffers || c.Runs != 10 || c.Agreed != 9 {
		t.Fatalf("fixed with no Agreement must need every run: %+v", c)
	}
	// an errored run is excluded: 2 passed, 1 errored -> held in 2 of 2
	c2 := cellOf(t, Result(claimIn(mustRules(t, "Reference", "fixed"), "F-1", oneMember(), outcomesRuns("solo", "F-1", "passed", "passed", "errored")...)), "F-1", "solo")
	if c2.State != StateIdentical || c2.Runs != 2 || c2.CouldNotRun != 1 {
		t.Fatalf("an errored run counted as a failure: %+v", c2)
	}
	if c2.PartsDiffer == nil || len(c2.PartsDiffer) != 0 || c2.State == StateNoise {
		t.Errorf("a claim cell has no parts_differ and is never noise: %+v", c2)
	}
}

// ── one table for a mixed set ──────────────────────────────────────────────────────────────────────

func mixedInput(refOutcomeF, candOutcomeF string, candHash string) Input {
	m, f := &Rules{}, &Rules{}
	*m, *f = *baseRules("measured"), *baseRules("fixed")
	rows := func(h string) []ScenarioOutput { return []ScenarioOutput{out("M-1", hx(h))} }
	return Input{SetHash: setH, Members: twoMembers(),
		Checks: []Check{{ID: "M-1", Path: "a/M-1.md", Rules: m}, {ID: "F-1", Path: "a/F-1.md", Rules: f}},
		Runs: []Run{
			runOut("r1", "old", map[string]string{"M-1": "passed", "F-1": refOutcomeF}, rows("1")...),
			runOut("r2", "old", map[string]string{"M-1": "passed", "F-1": refOutcomeF}, rows("1")...),
			runOut("r3", "new", map[string]string{"M-1": "passed", "F-1": candOutcomeF}, rows(candHash)...),
		}}
}

func baseRules(kind string) *Rules {
	r, probs := BuildRules(kv("Reference", kind))
	if len(probs) > 0 {
		panic("rules")
	}
	return r
}

func TestCMP7_AMixedSetIsOneTableAndTheReferenceMembersClaimCellIsJudgedAndCounted(t *testing.T) {
	o := Result(mixedInput("passed", "passed", "1"))
	if o.Verdict != VerdictSame {
		t.Fatalf("verdict = %s, want same: %+v", o.Verdict, o.Counts)
	}
	// the measured check counts its candidate cell only; the fixed check counts BOTH members' cells
	if o.Counts.Identical != 3 || o.Counts.Checks != 2 {
		t.Errorf("counts = %+v, want identical 3 (new on M-1, old and new on F-1) over 2 checks", o.Counts)
	}
	if cellOf(t, o, "F-1", "old").Reference {
		t.Errorf("on a fixed check the reference member's cell is judged like any other, not a baseline")
	}
	if !cellOf(t, o, "M-1", "old").Reference {
		t.Errorf("on a measured check the reference member's cell is the baseline")
	}
	// a failing claim on the REFERENCE member makes the comparison differ
	if o := Result(mixedInput("failed", "passed", "1")); o.Verdict != VerdictDifferences || o.Counts.Differs != 1 {
		t.Errorf("a fixed check the reference member fails: verdict %s counts %+v, want differences", o.Verdict, o.Counts)
	}
	// ... and any differs wins over a gap
	if o := Result(mixedInput("passed", "failed", "2")); o.Verdict != VerdictDifferences || o.Counts.Differs != 2 {
		t.Errorf("both kinds differ: verdict %s counts %+v", o.Verdict, o.Counts)
	}
	// an errored claim run is a gap: incomplete, never same
	if o := Result(mixedInput("passed", "errored", "1")); o.Verdict != VerdictIncomplete {
		t.Errorf("an errored fixed run: verdict %s, want incomplete", o.Verdict)
	}
	// a planned run that has not finished is incomplete
	in := mixedInput("passed", "passed", "1")
	in.Runs = append(in.Runs, Run{ID: "r4", Member: "new", VersionKey: "v", Status: "queued"})
	if o := Result(in); o.Verdict != VerdictIncomplete || o.Counts.RunsPending != 1 {
		t.Errorf("a pending run: verdict %s counts %+v, want incomplete", o.Verdict, o.Counts)
	}
}

// ── the seed rule, judged per run ──────────────────────────────────────────────────────────────────

func seedInput(rules *Rules, runs ...Run) Input {
	return Input{SetHash: setH, Members: oneMember(), Runs: runs, Checks: []Check{
		{ID: "00-SEED", Path: "a/00-SEED.md", Seed: true},
		{ID: "P-1", Path: "a/P-1.md", Rules: rules}}}
}

func TestCMP7_AFailedSeedStopsOnlyItsOwnRun(t *testing.T) {
	prop := mustRules(t, "Reference", "property", "Agreement", "50%", "Repeats", "3")
	in := seedInput(prop,
		runOut("r1", "solo", map[string]string{"00-SEED": "passed", "P-1": "failed"}),
		runOut("r2", "solo", map[string]string{"00-SEED": "failed", "P-1": "passed"}), // would have helped the claim
		runOut("r3", "solo", map[string]string{"00-SEED": "errored", "P-1": "passed"}),
		runOut("r4", "solo", map[string]string{"00-SEED": "passed", "P-1": "passed"}),
	)
	c := cellOf(t, Result(in), "P-1", "solo")
	if c.Runs != 2 || c.Agreed != 1 || c.CouldNotRun != 2 {
		t.Fatalf("cell = %+v, want n=2 (the runs whose seed passed), agreed=1, could_not_run=2 (seed failed or errored)", c)
	}
}

func TestCMP7_AMemberWithNoRunWhoseSeedPassedCanNeverReadSame(t *testing.T) {
	prop := mustRules(t, "Reference", "property", "Agreement", "50%")
	in := seedInput(prop,
		runOut("r1", "solo", map[string]string{"00-SEED": "failed", "P-1": "passed"}),
		runOut("r2", "solo", map[string]string{"P-1": "passed"}), // the seed has no outcome at all: it did not pass
	)
	o := Result(in)
	c := cellOf(t, o, "P-1", "solo")
	if c.State != StateCouldNotRun || !strings.Contains(c.Sentence, "starting state not established") {
		t.Fatalf("cell = %+v", c)
	}
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %s, want incomplete: no run's starting state was established", o.Verdict)
	}
}

// seedRunsOf is n runs of solo where P-1 always passes and the seed's outcome is firstSeed in the first run and
// passed in the rest (5 runs with no disagreement are what a 50% property needs to be supported).
func seedRunsOf(firstSeed string, n int) []Run {
	var runs []Run
	for i := 0; i < n; i++ {
		s := "passed"
		if i == 0 {
			s = firstSeed
		}
		runs = append(runs, runOut("r"+string(rune('a'+i)), "solo", map[string]string{"00-SEED": s, "P-1": "passed"}))
	}
	return runs
}

// measuredSeedIn is a two-member set whose seed is MEASURED: both reference runs agree on the seed's output, the
// candidate's seed output is candHash. Every run passes and P-1 (a 50% property) has 5 runs per member, so nothing
// but the seed's own comparison can keep the roll-up from same.
func measuredSeedIn(t *testing.T, candHash string) Input {
	prop := mustRules(t, "Reference", "property", "Agreement", "50%")
	var runs []Run
	for i := 0; i < 5; i++ {
		runs = append(runs, runOut("o"+string(rune('a'+i)), "old", map[string]string{"00-SEED": "passed", "P-1": "passed"}, out("00-SEED", hx("1"))))
		runs = append(runs, runOut("n"+string(rune('a'+i)), "new", map[string]string{"00-SEED": "passed", "P-1": "passed"}, out("00-SEED", hx(candHash))))
	}
	return Input{SetHash: setH, Members: twoMembers(), Runs: runs, Checks: []Check{
		{ID: "00-SEED", Path: "a/00-SEED.md", Seed: true, Rules: baseRules("measured")},
		{ID: "P-1", Path: "a/P-1.md", Rules: prop}}}
}

func TestCMP7_AMeasuredSeedWhoseComparisonDoesNotHoldKeepsTheRollUpFromSame(t *testing.T) {
	o := Result(measuredSeedIn(t, "1"))
	if o.Verdict != VerdictSame || hasNote(o, NoteSeedDidNotHold) {
		t.Fatalf("control: a measured seed that holds: verdict %s notes %q", o.Verdict, o.Notes)
	}
	o = Result(measuredSeedIn(t, "2"))
	if c := cellOf(t, o, "00-SEED", "new"); c.State != StateDiffers {
		t.Fatalf("setup: the seed cell must differ: %+v", c)
	}
	if o.Counts.Differs != 0 || o.Counts.Identical != 2 {
		t.Errorf("counts = %+v: seed cells stay out of the counts", o.Counts)
	}
	if o.Verdict != VerdictIncomplete || !hasNote(o, NoteSeedDidNotHold) {
		t.Errorf("verdict %s notes %q: want incomplete with the closed seed note", o.Verdict, o.Notes)
	}
}

func TestCMP7_APassedSeedDeclaringCompareIsShownButNotCounted(t *testing.T) {
	prop := mustRules(t, "Reference", "property", "Agreement", "50%")
	seedRules := mustRules(t, "Reference", "fixed")
	in := seedInput(prop,
		seedRunsOf("failed", 6)...)
	in.Checks[0].Rules = seedRules
	o := Result(in)
	var seedCell *Cell
	for _, ck := range o.Checks {
		if ck.ID == "00-SEED" {
			if !ck.Seed {
				t.Errorf("the check row does not say seed: %+v", ck)
			}
			seedCell = &ck.Cells[0]
		}
	}
	if seedCell == nil || seedCell.State != StateDiffers || seedCell.Runs != 6 || seedCell.Agreed != 5 {
		t.Fatalf("the seed check's own cell is judged like any check: %+v", seedCell)
	}
	// only the property's cell is in the roll-up; the seed's `differs` is not
	if o.Counts.Differs != 0 || o.Counts.Identical != 1 {
		t.Errorf("counts = %+v: the seed cell must not be counted", o.Counts)
	}
	if hasNote(o, NoteNoSeedStep) {
		t.Errorf("a set with a seed check carries no 'no seed step' note")
	}
	// ... but the seed's own comparison not holding means the systems did not start alike: never same
	if o.Verdict != VerdictIncomplete || !hasNote(o, NoteSeedDidNotHold) {
		t.Errorf("verdict = %s notes = %q: a seed comparison that does not hold must read incomplete with the closed note", o.Verdict, o.Notes)
	}
	n := 0
	for _, x := range o.Notes {
		if x == NoteSeedDidNotHold {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the seed note appears %d times, want once", n)
	}
	// an unapproved differs elsewhere still wins
	in2 := seedInput(mustRules(t, "Reference", "fixed"),
		runOut("r1", "solo", map[string]string{"00-SEED": "failed", "P-1": "failed"}),
		runOut("r2", "solo", map[string]string{"00-SEED": "passed", "P-1": "failed"}))
	in2.Checks[0].Rules = seedRules
	if o2 := Result(in2); o2.Verdict != VerdictDifferences {
		t.Errorf("an unapproved differs elsewhere must win: verdict %s", o2.Verdict)
	}
	// a seed whose comparison holds adds neither the note nor a gap
	in3 := seedInput(prop,
		seedRunsOf("passed", 6)...)
	in3.Checks[0].Rules = seedRules
	if o3 := Result(in3); o3.Verdict != VerdictSame || hasNote(o3, NoteSeedDidNotHold) {
		t.Errorf("a held seed comparison: verdict %s notes %q, want same and no note: %+v", o3.Verdict, o3.Notes, cellOf(t, o3, "P-1", "solo"))
	}
}

// "A property check is never noise": even when the rule declares output parts and the recorded
// outputs differ run to run, a property check is judged from the check's own outcome alone.
func TestCMP7_APropertyCheckThatDeclaresOutputPartsIsNeverNoise(t *testing.T) {
	prop := mustRules(t, "Reference", "property", "Agreement", "50%")
	// the parser refuses **Output** on a property check, but a stored body edited by hand can carry it: set it directly
	prop.Output = OutputSel{Status: true, Body: true}
	in := Input{SetHash: setH, Members: twoMembers(),
		Checks: []Check{{ID: "P-1", Path: "a/P-1.md", Rules: prop}},
		Runs: []Run{
			runOut("r1", "old", map[string]string{"P-1": "passed"}, out("P-1", hx("1"))),
			runOut("r2", "old", map[string]string{"P-1": "passed"}, out("P-1", hx("2"))),
			runOut("r3", "new", map[string]string{"P-1": "passed"}, out("P-1", hx("3"))),
			runOut("r4", "new", map[string]string{"P-1": "passed"}, out("P-1", hx("4"))),
		}}
	o := Result(in)
	for _, m := range []string{"old", "new"} {
		c := cellOf(t, o, "P-1", m)
		if c.State != StateIdentical || len(c.PartsDiffer) != 0 || c.PartsDiffer == nil {
			t.Errorf("%s cell = %+v: a property held in every run is identical, never noise, with empty parts_differ", m, c)
		}
	}
	if o.Counts.Noise != 0 {
		t.Errorf("counts = %+v: a property check is never counted as noise", o.Counts)
	}
	// judged per member from the check's own outcome: the reference member's cell is a judged cell, not a baseline
	if cellOf(t, o, "P-1", "old").Reference || o.Counts.Identical != 2 {
		t.Errorf("a property check has no baseline member: reference=%v counts=%+v, want identical 2", cellOf(t, o, "P-1", "old").Reference, o.Counts)
	}
}

func TestCMP7_ASetWithNoSeedCheckSaysSoOnce(t *testing.T) {
	in := claimIn(mustRules(t, "Reference", "fixed"), "F-1", oneMember(), outcomesRuns("solo", "F-1", "passed")...)
	n := 0
	for _, x := range Result(in).Notes {
		if x == NoteNoSeedStep {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the 'no seed step' note appears %d times, want once", n)
	}
}

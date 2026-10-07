package compare

// cmp14_result_test.go -- ARGUS-CMP-14. A run in which a check FAILED is pushed
// by the executor with status `failed` AND one outcome per check (runner/mapper.go). That is a run that RAN its
// checks. A run that never ran them (an abort: status `failed`, no outcomes) is the only "failed before it ran".
// Pure: no store, no clock.

import (
	"strings"
	"testing"
)

// ranFailed is the run the real mapper pushes when some check failed: status failed, outcomes present.
func ranFailed(id, member string, outcomes map[string]string, rows ...ScenarioOutput) Run {
	r := runOut(id, member, outcomes, rows...)
	r.Status = "failed"
	return r
}

// aborted is the run an executor pushes when it produced no report: status failed, nothing else.
func aborted(id, member string) Run {
	return Run{ID: id, Member: member, VersionKey: "v", Status: "failed", SetHash: setH, Outcomes: map[string]string{}}
}

func TestCMP14_ARunThatRanWithAFailingCheckIsJudgedLikeAnyRunThatRan(t *testing.T) {
	in := claimIn(mustRules(t, "Reference", "fixed"), "F-1", twoMembers(),
		ranFailed("r1", "old", map[string]string{"F-1": "failed"}),
		ranFailed("r2", "old", map[string]string{"F-1": "failed"}),
		runOut("r3", "new", map[string]string{"F-1": "passed"}))
	o := Result(in)
	c := cellOf(t, o, "F-1", "old")
	if c.State != StateDiffers || c.Runs != 2 || c.Agreed != 0 || c.CouldNotRun != 0 {
		t.Fatalf("old cell = %+v, want differs over 2 runs, 0 agreed, none could not run", c)
	}
	if strings.Contains(c.Sentence, "failed before it ran") {
		t.Errorf("sentence = %q: the run ran its checks", c.Sentence)
	}
	if hasNote(o, NoteReferenceFailed) {
		t.Errorf("notes = %q: no run failed before running", o.Notes)
	}
	if o.Counts.Runs != 3 || o.Counts.RunsPending != 0 || o.Counts.CouldNotRun != 0 || o.Verdict != VerdictDifferences {
		t.Errorf("counts = %+v verdict %s, want 3 runs, none pending, differences", o.Counts, o.Verdict)
	}
	for _, s := range o.Systems {
		if s.Member == "old" && s.Runs != 2 {
			t.Errorf("system old Runs = %d, want 2: a run that ran is counted", s.Runs)
		}
	}
}

func TestCMP14_ARunThatNeverRanItsChecksStaysCouldNotRun(t *testing.T) {
	in := claimIn(mustRules(t, "Reference", "fixed"), "F-1", twoMembers(),
		aborted("r1", "old"), aborted("r2", "old"),
		runOut("r3", "new", map[string]string{"F-1": "passed"}))
	o := Result(in)
	c := cellOf(t, o, "F-1", "old")
	if c.State != StateCouldNotRun || !strings.Contains(c.Sentence, "the run failed before it ran the checks") {
		t.Fatalf("old cell = %+v, want could_not_run: the run failed before it ran the checks", c)
	}
	// the measured form carries the closed note
	m := Input{SetHash: setH, Members: twoMembers(), Checks: []Check{{ID: "M-1", Path: "a/M-1.md", Rules: baseRules("measured")}},
		Runs: []Run{aborted("r1", "old"), aborted("r2", "old")}}
	if mo := Result(m); !hasNote(mo, NoteReferenceFailed) {
		t.Errorf("notes = %q, want %q for a reference that truly did not run its checks", mo.Notes, NoteReferenceFailed)
	}
}

// THE FIXTURE RULE: the wire cannot carry a failed check beside status completed, but if a stored row ever says
// completed the judgement is the same; and an errored-only run is still a run that ran (its cells say errored).
func TestCMP14_AMeasuredCellComparesTheOutputsOfARunThatRanWithAFailingCheck(t *testing.T) {
	// a check whose own EXPECT failed still recorded its output (chain/http.go:298, argus.go:1026), so a measured
	// cell reads the recorded output exactly as it does for a passing run
	mk := func(candHash string) Input {
		return Input{SetHash: setH, Members: twoMembers(), Checks: []Check{{ID: "M-1", Path: "a/M-1.md", Rules: baseRules("measured")}},
			Runs: []Run{
				ranFailed("r1", "old", map[string]string{"M-1": "failed"}, out("M-1", hx("1"))),
				ranFailed("r2", "old", map[string]string{"M-1": "failed"}, out("M-1", hx("1"))),
				ranFailed("r3", "new", map[string]string{"M-1": "failed"}, out("M-1", hx(candHash))),
			}}
	}
	o := Result(mk("1"))
	if c := cellOf(t, o, "M-1", "new"); c.State != StateIdentical || c.Runs != 1 {
		t.Errorf("same recorded output: new cell = %+v, want identical", c)
	}
	if c := cellOf(t, o, "M-1", "old"); !c.Reference || c.Runs != 2 || c.State != StateIdentical {
		t.Errorf("reference cell = %+v, want the baseline over 2 runs", c)
	}
	if o2 := Result(mk("2")); cellOf(t, o2, "M-1", "new").State != StateDiffers || o2.Verdict != VerdictDifferences {
		t.Errorf("a different recorded output must differ: %+v verdict %s", cellOf(t, o2, "M-1", "new"), o2.Verdict)
	}
}

func TestCMP14_AFailedSeedInARunThatRanStillMakesThatRunEstablishNothing(t *testing.T) {
	prop := mustRules(t, "Reference", "property", "Agreement", "50%")
	in := seedInput(prop,
		ranFailed("r1", "solo", map[string]string{"00-SEED": "failed", "P-1": "passed"}), // seed failed: P-1 not judged here
		ranFailed("r2", "solo", map[string]string{"00-SEED": "passed", "P-1": "failed"}),
		runOut("r3", "solo", map[string]string{"00-SEED": "passed", "P-1": "passed"}))
	c := cellOf(t, Result(in), "P-1", "solo")
	if c.Runs != 2 || c.Agreed != 1 || c.CouldNotRun != 1 {
		t.Fatalf("cell = %+v, want n=2 agreed=1 could_not_run=1 (the run whose seed failed)", c)
	}
}

// D: NoteSeedDidNotHold says a seed comparison was JUDGED and did not hold. A seed cell that could not run was not
// judged, so it must not say it.
func TestCMP14_NoSeedNoteWhenNoSeedCheckWasJudged(t *testing.T) {
	seedRules := mustRules(t, "Reference", "fixed")
	prop := mustRules(t, "Reference", "property", "Agreement", "50%")
	in := Input{SetHash: setH, Members: twoMembers(), Checks: []Check{
		{ID: "00-SEED", Path: "a/00-SEED.md", Seed: true, Rules: seedRules},
		{ID: "P-1", Path: "a/P-1.md", Rules: prop}},
		Runs: []Run{
			runOut("o1", "old", map[string]string{"00-SEED": "passed", "P-1": "passed"}),
			runOut("o2", "old", map[string]string{"00-SEED": "passed", "P-1": "passed"}),
			aborted("n1", "new"), aborted("n2", "new")}}
	o := Result(in)
	if c := cellOf(t, o, "00-SEED", "new"); c.State != StateCouldNotRun {
		t.Fatalf("setup: the candidate's seed cell = %+v, want could_not_run", c)
	}
	if hasNote(o, NoteSeedDidNotHold) {
		t.Errorf("notes = %q: no seed check of the candidate was judged, so no seed comparison failed to hold", o.Notes)
	}
	if o.Verdict != VerdictIncomplete {
		t.Errorf("verdict = %s, want incomplete: the candidate did not run", o.Verdict)
	}
	// a seed that WAS judged and failed still says so
	in.Runs[2] = ranFailed("n1", "new", map[string]string{"00-SEED": "failed", "P-1": "passed"})
	in.Runs[3] = ranFailed("n2", "new", map[string]string{"00-SEED": "failed", "P-1": "passed"})
	if o2 := Result(in); !hasNote(o2, NoteSeedDidNotHold) || o2.Verdict != VerdictIncomplete {
		t.Errorf("judged-and-failed seed: verdict %s notes %q, want incomplete with the seed note", o2.Verdict, o2.Notes)
	}
}

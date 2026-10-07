package compare

import "testing"

// ARGUS-CMP-6 fixes (verifier findings at d356fd60): counts.runs counts only terminal runs, a table with
// planned runs outstanding is never `same`, and a reference that ran once is never `same`.

func pendingRun(id, member, version, status string) Run {
	return Run{ID: id, Member: member, VersionKey: version, Status: status, SetHash: setH, Outcomes: map[string]string{}}
}

// Defect 1: three planned runs, one landed, two still queued -> counts.runs is 1 and runs_pending is 2.
func TestCMP6Fix_CountsRunsAreTerminalAndPendingAreCountedApart(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		pendingRun("q1", "old", "", "queued"),
		pendingRun("q2", "new", "", "queued"),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	if o.Counts.Runs != 1 || o.Counts.RunsPending != 2 {
		t.Fatalf("counts.runs = %d (want 1), counts.runs_pending = %d (want 2)", o.Counts.Runs, o.Counts.RunsPending)
	}
	// a failed run is terminal: it is shown as could-not-run and counted in runs
	in.Runs = append(in.Runs, pendingRun("f1", "new", "v2", "failed"))
	o = Result(in)
	if o.Counts.Runs != 2 || o.Counts.RunsPending != 2 {
		t.Fatalf("with a failed run: counts.runs = %d (want 2), counts.runs_pending = %d (want 2)", o.Counts.Runs, o.Counts.RunsPending)
	}
}

// Defect 2: two landed reference runs, a third still running at the same version, an identical candidate.
func TestCMP6Fix_PlannedRunsOutstandingIsNeverSame(t *testing.T) {
	mk := func(extra ...Run) Input {
		in := baseInput(
			run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
			run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
			run("n1", "new", "v2", "passed", out("C-1", hx("1"))),
		)
		in.Checks[0].Rules = measured(t)
		in.Runs = append(in.Runs, extra...)
		return in
	}
	if v := Result(mk()).Verdict; v != VerdictSame {
		t.Fatalf("control: with every run terminal the verdict is %s, want same", v)
	}
	o := Result(mk(pendingRun("r3", "old", "v1", "running")))
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("one reference run still running: verdict = %s, want incomplete", o.Verdict)
	}
	if !hasNote(o, RunsPendingNote(1)) || RunsPendingNote(1) != "1 planned run has not finished" {
		t.Fatalf("notes = %v, want %q", o.Notes, "1 planned run has not finished")
	}
	o = Result(mk(pendingRun("r3", "old", "v1", "running"), pendingRun("r4", "old", "v1", "queued")))
	if o.Verdict != VerdictIncomplete || !hasNote(o, "2 planned runs have not finished") {
		t.Fatalf("two outstanding: verdict = %s notes = %v, want incomplete and %q", o.Verdict, o.Notes, "2 planned runs have not finished")
	}
	// differences still wins over incomplete
	d := mk(pendingRun("r3", "old", "v1", "running"))
	d.Runs[2] = run("n1", "new", "v2", "passed", out("C-1", hx("9")))
	if o := Result(d); o.Verdict != VerdictDifferences {
		t.Fatalf("a difference with a run outstanding: verdict = %s, want differences", o.Verdict)
	}
}

// Defect 3: a reference with exactly one usable run reads incomplete, the candidate cell keeps its own state.
func TestCMP6Fix_ReferenceThatRanOnceIsIncomplete(t *testing.T) {
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("n1", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	in.Checks[0].Rules = measured(t)
	o := Result(in)
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("a reference that ran once: verdict = %s, want incomplete", o.Verdict)
	}
	if !hasNote(o, NoteReferenceSingleRun) {
		t.Fatalf("notes = %v, want the single-run note kept", o.Notes)
	}
	if c := cellOf(t, o, "C-1", "new"); c.State != StateIdentical {
		t.Fatalf("the candidate cell keeps its own state: %s, want identical", c.State)
	}
	// a second agreeing reference run of that version makes it same
	in.Runs = append(in.Runs, run("r2", "old", "v1", "passed", out("C-1", hx("1"))))
	if v := Result(in).Verdict; v != VerdictSame {
		t.Fatalf("two agreeing reference runs: verdict = %s, want same", v)
	}
}

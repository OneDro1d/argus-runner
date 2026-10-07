package compare

// approval_test.go -- ARGUS-CMP-10: an approved difference in the pure core. An approval applies to
// a cell only while the cell DIFFERS, only while both of its hashes are exactly the ones it was given for, only when
// it is not revoked, and (for a chain check) only for the steps it was given for. No store, no clock.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func srow(id, step, hash string) ScenarioOutput {
	return ScenarioOutput{ScenarioID: id, OutputRecord: OutputRecord{V: 1, Step: step, Sample: 1, State: StateRecorded, Status: 200, Hash: hash,
		Parts: Parts{Status: hx("a"), Body: hash}, BodyKind: KindJSON}}
}

// differing returns an input with one measured check C-1 whose member `new` differs from the stable reference.
func differing(t *testing.T) (Input, Cell) {
	t.Helper()
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("2"))),
	)
	in.Checks[0].Rules = measured(t)
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateDiffers {
		t.Fatalf("fixture: cell = %+v", c)
	}
	return in, c
}

func approvalFor(c Cell, id string) Approval {
	return Approval{ID: id, Check: "C-1", Member: "new", VersionKey: "v2", ReferenceHash: c.ReferenceHash, MemberHash: c.MemberHash}
}

func TestCMP10_ALiveApprovalCountsUnderApprovedNotDiffersAndTheRollUpReadsApproved(t *testing.T) {
	in, c := differing(t)
	in.Approvals = []Approval{approvalFor(c, "apr_1")}
	o := Result(in)
	got := cellOf(t, o, "C-1", "new")
	if got.State != StateDiffers || !got.Approved {
		t.Fatalf("cell = %+v, want state differs and approved", got)
	}
	if len(got.Approvals) != 1 || got.Approvals[0] != (ApprovalRef{ID: "apr_1", Step: "", Status: ApprovalLive}) {
		t.Fatalf("approvals on the cell = %+v", got.Approvals)
	}
	if o.Counts.Approved != 1 || o.Counts.Differs != 0 {
		t.Fatalf("counts = %+v, want approved 1 and differs 0 (a cell is counted once)", o.Counts)
	}
	if o.Verdict != VerdictSameWithApproved {
		t.Fatalf("verdict = %s", o.Verdict)
	}
}

func TestCMP10_ARevokedApprovalLeavesTheCellDiffersAndIsStillListedAsRevoked(t *testing.T) {
	in, c := differing(t)
	a := approvalFor(c, "apr_1")
	a.Revoked = true
	in.Approvals = []Approval{a}
	o := Result(in)
	got := cellOf(t, o, "C-1", "new")
	if got.Approved || got.State != StateDiffers {
		t.Fatalf("a revoked approval counts: %+v", got)
	}
	if len(got.Approvals) != 1 || got.Approvals[0].Status != ApprovalRevoked || got.Approvals[0].ID != "apr_1" {
		t.Fatalf("a revoked approval must stay listed as revoked: %+v", got.Approvals)
	}
	if o.Counts.Approved != 0 || o.Counts.Differs != 1 || o.Verdict != VerdictDifferences {
		t.Fatalf("counts %+v verdict %s", o.Counts, o.Verdict)
	}
}

func TestCMP10_AnApprovalWhoseHashesMovedIsStaleListedAndDoesNotCount(t *testing.T) {
	in, c := differing(t)
	for name, mod := range map[string]func(a *Approval){
		"the reference hash moved": func(a *Approval) { a.ReferenceHash = hx("9") },
		"the member hash moved":    func(a *Approval) { a.MemberHash = hx("9") },
	} {
		a := approvalFor(c, "apr_1")
		mod(&a)
		in.Approvals = []Approval{a}
		o := Result(in)
		got := cellOf(t, o, "C-1", "new")
		if got.Approved || o.Verdict != VerdictDifferences || o.Counts.Differs != 1 || o.Counts.Approved != 0 {
			t.Errorf("%s: the approval counts: %+v verdict %s counts %+v", name, got, o.Verdict, o.Counts)
		}
		if len(got.Approvals) != 1 || got.Approvals[0].Status != ApprovalStale {
			t.Errorf("%s: want one approval listed as stale, got %+v", name, got.Approvals)
		}
	}
	// a NEW output on the member after the approval makes it stale in a real run of events
	in.Approvals = []Approval{approvalFor(c, "apr_1")}
	in.Runs[2] = run("r3", "new", "v2", "passed", out("C-1", hx("3")))
	o := Result(in)
	if got := cellOf(t, o, "C-1", "new"); got.Approved || o.Verdict != VerdictDifferences {
		t.Fatalf("the member's output changed after the approval and the approval still counts: %+v", got)
	}
	// and a NEW output on the reference side
	in2, c2 := differing(t)
	in2.Approvals = []Approval{approvalFor(c2, "apr_1")}
	in2.Runs[0] = run("r1", "old", "v1", "passed", out("C-1", hx("7")))
	in2.Runs[1] = run("r2", "old", "v1", "passed", out("C-1", hx("7")))
	o2 := Result(in2)
	if got := cellOf(t, o2, "C-1", "new"); got.Approved || o2.Verdict != VerdictDifferences {
		t.Fatalf("the reference's output changed after the approval and the approval still counts: %+v", got)
	}
}

func TestCMP10_OnlyADiffersCellCanBeApprovedNeverNoiseNotMeasuredCouldNotRunOrIdentical(t *testing.T) {
	// Each of these cells carries EMPTY hashes (or equal ones); an approval holding the same values must still not apply.
	type tc struct {
		name  string
		in    Input
		state CellState
	}
	noise := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("2"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	notMeasured := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed"),
	)
	notMeasured.Runs[2].Outcomes = map[string]string{"C-1": "passed"} // ran, nothing recorded
	couldNotRun := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	couldNotRun.Runs[2].SetHash = "another-set"
	identical := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("1"))),
	)
	for _, c := range []tc{{"noise", noise, StateNoise}, {"not_measured", notMeasured, StateNotMeasured}, {"could_not_run", couldNotRun, StateCouldNotRun}, {"identical", identical, StateIdentical}} {
		c.in.Checks[0].Rules = measured(t)
		before := Result(c.in)
		cell := cellOf(t, before, "C-1", "new")
		if cell.State != c.state {
			t.Fatalf("%s: fixture cell = %+v", c.name, cell)
		}
		c.in.Approvals = []Approval{
			{ID: "a1", Check: "C-1", Member: "new", VersionKey: "v2", ReferenceHash: cell.ReferenceHash, MemberHash: cell.MemberHash}, // exactly the cell's own hashes
			{ID: "a2", Check: "C-1", Member: "new", VersionKey: "v2"},                                                                 // empty hashes, which an empty cell holds
		}
		o := Result(c.in)
		got := cellOf(t, o, "C-1", "new")
		if got.Approved || o.Counts.Approved != 0 || o.Verdict == VerdictSameWithApproved {
			t.Errorf("%s: an approval was applied to a %s cell: %+v counts %+v verdict %s", c.name, c.state, got, o.Counts, o.Verdict)
		}
		if o.Verdict != before.Verdict {
			t.Errorf("%s: approvals moved the verdict from %s to %s", c.name, before.Verdict, o.Verdict)
		}
		for _, ref := range got.Approvals {
			if ref.Status == ApprovalLive {
				t.Errorf("%s: an approval is listed live on a %s cell: %+v", c.name, c.state, got.Approvals)
			}
		}
	}
}

func TestCMP10_AnApprovedCellNeverTurnsAGapGreen(t *testing.T) {
	// two checks: C-1 differs and is approved, C-2 is noise -> incomplete, not same_with_approved_differences
	in := Input{SetHash: setH, Members: twoMembers(),
		Checks: []Check{{ID: "C-1", Path: "a", Rules: measured(t)}, {ID: "C-2", Path: "b", Rules: measured(t)}}}
	in.Runs = []Run{
		run("r1", "old", "v1", "passed", out("C-1", hx("1")), out("C-2", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1")), out("C-2", hx("2"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("5")), out("C-2", hx("1"))),
	}
	first := Result(in)
	c := cellOf(t, first, "C-1", "new")
	if c.State != StateDiffers || cellOf(t, first, "C-2", "new").State != StateNoise {
		t.Fatalf("fixture: %+v / %+v", c, cellOf(t, first, "C-2", "new"))
	}
	in.Approvals = []Approval{approvalFor(c, "a1")}
	o := Result(in)
	if !cellOf(t, o, "C-1", "new").Approved {
		t.Fatalf("the difference was not approved: %+v", cellOf(t, o, "C-1", "new"))
	}
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %s, want incomplete (a noise cell is a gap that an approval must not paper over)", o.Verdict)
	}
	if o.Counts.Approved != 1 || o.Counts.Noise != 1 {
		t.Fatalf("counts = %+v", o.Counts)
	}

	// a run still pending is a gap too
	in2, c2 := differing(t)
	in2.Runs = append(in2.Runs, Run{ID: "r4", Member: "new", VersionKey: "v2", Status: "running", SetHash: setH})
	in2.Approvals = []Approval{approvalFor(c2, "a1")}
	if o2 := Result(in2); o2.Verdict != VerdictIncomplete || o2.Counts.Approved != 1 {
		t.Fatalf("pending run + approved difference: verdict %s counts %+v", o2.Verdict, o2.Counts)
	}
}

func TestCMP10_AnUnapprovedDifferenceBesideAnApprovedOneReadsDifferences(t *testing.T) {
	in := Input{SetHash: setH, Members: twoMembers(),
		Checks: []Check{{ID: "C-1", Path: "a", Rules: measured(t)}, {ID: "C-2", Path: "b", Rules: measured(t)}}}
	in.Runs = []Run{
		run("r1", "old", "v1", "passed", out("C-1", hx("1")), out("C-2", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1")), out("C-2", hx("1"))),
		run("r3", "new", "v2", "passed", out("C-1", hx("5")), out("C-2", hx("6"))),
	}
	c := cellOf(t, Result(in), "C-1", "new")
	in.Approvals = []Approval{approvalFor(c, "a1")}
	o := Result(in)
	if o.Verdict != VerdictDifferences || o.Counts.Approved != 1 || o.Counts.Differs != 1 {
		t.Fatalf("verdict %s counts %+v", o.Verdict, o.Counts)
	}
}

func TestCMP10_TheCountsOfJudgedCellsAddUpToTheJudgedCellsWhateverIsApproved(t *testing.T) {
	in := Input{SetHash: setH, Members: []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}},
		Checks: []Check{{ID: "A", Path: "a", Rules: measured(t)}, {ID: "B", Path: "b", Rules: measured(t)}, {ID: "C", Path: "c", Rules: measured(t)}}}
	in.Runs = []Run{
		run("r1", "old", "v", "passed", out("A", hx("1")), out("B", hx("1")), out("C", hx("1"))),
		run("r2", "old", "v", "passed", out("A", hx("1")), out("B", hx("1")), out("C", hx("1"))),
		run("r3", "new", "v", "passed", out("A", hx("1")), out("B", hx("2")), out("C", hx("3"))),
	}
	b := cellOf(t, Result(in), "B", "new")
	in.Approvals = []Approval{{ID: "a1", Check: "B", Member: "new", VersionKey: "v", ReferenceHash: b.ReferenceHash, MemberHash: b.MemberHash}}
	o := Result(in)
	c := o.Counts
	judged := 0
	for _, ck := range o.Checks {
		for _, cell := range ck.Cells {
			if !cell.Reference {
				judged++
			}
		}
	}
	if sum := c.Identical + c.Differs + c.Approved + c.Noise + c.NotMeasured + c.CouldNotRun; sum != judged || judged != 3 {
		t.Fatalf("identical+differs+approved+noise+not_measured+could_not_run = %d, judged cells = %d, counts %+v", sum, judged, c)
	}
	if c.Identical != 1 || c.Approved != 1 || c.Differs != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

func TestCMP10_AnApprovedCellWhoseClaimIsUnsupportedStillReadsApprovedAndShowsTheUnsupportedClaim(t *testing.T) {
	// 80% declared over 5 runs: 3 of the member's runs disagree, so the cell differs and its claim is unsupported by
	// definition. DECISION: approving the cell accepts exactly that observed difference, so the
	// unsupported claim of an APPROVED differs cell does not hold the roll-up back; it stays visible on the cell.
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("m1", "new", "v2", "passed", out("C-1", hx("1"))),
		run("m2", "new", "v2", "passed", out("C-1", hx("1"))),
		run("m3", "new", "v2", "passed", out("C-1", hx("2"))),
		run("m4", "new", "v2", "passed", out("C-1", hx("3"))),
		run("m5", "new", "v2", "passed", out("C-1", hx("4"))),
	)
	in.Checks[0].Rules = mustRules(t, "Reference", "measured", "Agreement", "80%", "Repeats", "5")
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateDiffers || c.ClaimSupported {
		t.Fatalf("fixture: %+v", c)
	}
	in.Approvals = []Approval{approvalFor(c, "a1")}
	o := Result(in)
	got := cellOf(t, o, "C-1", "new")
	if !got.Approved || got.ClaimSupported || got.Unsupported == "" {
		t.Fatalf("approved cell = %+v, want approved with the unsupported claim still shown", got)
	}
	if o.Verdict != VerdictSameWithApproved {
		t.Fatalf("verdict = %s", o.Verdict)
	}
}

func TestCMP10_AMemberWhoseRunsDoNotAllGiveTheSameOutputIsPinnedToEveryOutputSeen(t *testing.T) {
	// DECISION: the member's hash is the hash over the SET of distinct outputs its usable runs gave; an approval given
	// for one of them alone does not apply, and a new output on a later run voids the approval.
	in := baseInput(
		run("r1", "old", "v1", "passed", out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("C-1", hx("1"))),
		run("m1", "new", "v2", "passed", out("C-1", hx("2"))),
		run("m2", "new", "v2", "passed", out("C-1", hx("3"))),
	)
	in.Checks[0].Rules = measured(t)
	c := cellOf(t, Result(in), "C-1", "new")
	only2 := checkKey([]ScenarioOutput{out("C-1", hx("2"))})
	only3 := checkKey([]ScenarioOutput{out("C-1", hx("3"))})
	if c.MemberHash == only2 || c.MemberHash == only3 || c.MemberHash == "" {
		t.Fatalf("the member hash %s must cover both outputs, not one (%s, %s)", c.MemberHash, only2, only3)
	}
	for name, h := range map[string]string{"the first output alone": only2, "the second output alone": only3} {
		a := approvalFor(c, "a1")
		a.MemberHash = h
		in.Approvals = []Approval{a}
		if o := Result(in); cellOf(t, o, "C-1", "new").Approved {
			t.Errorf("an approval for %s covers a cell that shows two outputs", name)
		}
	}
	in.Approvals = []Approval{approvalFor(c, "a1")}
	if !cellOf(t, Result(in), "C-1", "new").Approved {
		t.Fatalf("an approval at the cell's own hashes does not apply")
	}
	// a third output lands: the approval no longer covers what the cell shows
	in.Runs = append(in.Runs, run("m3", "new", "v2", "passed", out("C-1", hx("4"))))
	if o := Result(in); cellOf(t, o, "C-1", "new").Approved || o.Verdict != VerdictDifferences {
		t.Fatalf("a new output on a later run left the approval standing")
	}
}

func TestCMP10_TheReferenceHashCoversEveryUsableReferenceRunNotTheFirstOnly(t *testing.T) {
	// the reference's two runs agree within a declared tolerance but are not the same output; an approval given while the
	// second run held 10.3 must not cover a table in which it holds 10.4
	rules := mustRules(t, "Reference", "measured", "Tolerance", "$.t abs 0.5")
	tv := func(v float64) ToleranceValue { return ToleranceValue{Path: "$.t", Rule: 0, Value: v} }
	pa := Parts{Status: hx("a"), Body: hx("1")}
	mk := func(id, member string, v float64) Run {
		r := so("C-1", "", 1, hx("1"), pa, tv(v))
		return Run{ID: id, Member: member, VersionKey: "v1", Status: "completed", SetHash: setH, Outcomes: map[string]string{"C-1": "passed"}, Outputs: []ScenarioOutput{r}}
	}
	mkIn := func(second float64) Input {
		in := baseInput(mk("r1", "old", 10), mk("r2", "old", second), mk("m1", "new", 12))
		in.Checks[0].Rules = rules
		return in
	}
	in := mkIn(10.3)
	c := cellOf(t, Result(in), "C-1", "new")
	if c.State != StateDiffers {
		t.Fatalf("fixture: %+v", c)
	}
	in.Approvals = []Approval{{ID: "a1", Check: "C-1", Member: "new", VersionKey: "v1", ReferenceHash: c.ReferenceHash, MemberHash: c.MemberHash}}
	if !cellOf(t, Result(in), "C-1", "new").Approved {
		t.Fatalf("the approval at the cell's own hashes does not apply")
	}
	moved := mkIn(10.4)
	moved.Approvals = in.Approvals
	if o := Result(moved); cellOf(t, o, "C-1", "new").Approved {
		t.Fatalf("the reference's SECOND run changed and the approval still counts: the reference hash covers only the first run")
	}
}

func TestCMP10_AChainCheckIsApprovedStepByStepAndOnlyWhenEveryDifferingStepIs(t *testing.T) {
	mk := func(create, read string) []ScenarioOutput {
		return []ScenarioOutput{srow("C-1", "create", create), srow("C-1", "read", read)}
	}
	build := func(memberCreate, memberRead string) Input {
		in := baseInput(
			Run{ID: "r1", Member: "old", VersionKey: "v1", Status: "completed", SetHash: setH, Outcomes: map[string]string{"C-1": "passed"}, Outputs: mk(hx("1"), hx("1"))},
			Run{ID: "r2", Member: "old", VersionKey: "v1", Status: "completed", SetHash: setH, Outcomes: map[string]string{"C-1": "passed"}, Outputs: mk(hx("1"), hx("1"))},
			Run{ID: "m1", Member: "new", VersionKey: "v2", Status: "completed", SetHash: setH, Outcomes: map[string]string{"C-1": "passed"}, Outputs: mk(memberCreate, memberRead)},
		)
		in.Checks[0].Rules = measured(t)
		return in
	}
	step := func(c Cell, id, step string) Approval {
		a := approvalFor(c, id)
		a.Step = step
		return a
	}

	// only `read` differs
	in := build(hx("1"), hx("2"))
	c := cellOf(t, Result(in), "C-1", "new")
	if strings.Join(c.DiffSteps, ",") != "read" {
		t.Fatalf("DiffSteps = %v, want [read]", c.DiffSteps)
	}
	if strings.Join(c.Steps, ",") != "create,read" {
		t.Fatalf("Steps = %v, want [create read]", c.Steps)
	}
	in.Approvals = []Approval{step(c, "a1", "read")}
	if o := Result(in); !cellOf(t, o, "C-1", "new").Approved || o.Verdict != VerdictSameWithApproved {
		t.Fatalf("an approval for the step that differs does not apply")
	}
	in.Approvals = []Approval{step(c, "a2", "create")}
	o := Result(in)
	got := cellOf(t, o, "C-1", "new")
	if got.Approved || o.Verdict != VerdictDifferences {
		t.Fatalf("an approval for a step that does NOT differ covered the cell that differs in another step")
	}
	if len(got.Approvals) != 1 || got.Approvals[0].Status != ApprovalStale {
		t.Fatalf("the approval for the wrong step must be listed as stale: %+v", got.Approvals)
	}
	in.Approvals = []Approval{step(c, "a3", "")}
	if cellOf(t, Result(in), "C-1", "new").Approved {
		t.Fatalf("an approval for no step covered a cell that differs in the step read")
	}

	// both steps differ: each must be approved
	in2 := build(hx("3"), hx("2"))
	c2 := cellOf(t, Result(in2), "C-1", "new")
	if strings.Join(c2.DiffSteps, ",") != "create,read" {
		t.Fatalf("DiffSteps = %v, want [create read]", c2.DiffSteps)
	}
	in2.Approvals = []Approval{step(c2, "a1", "read")}
	if o := Result(in2); cellOf(t, o, "C-1", "new").Approved || o.Verdict != VerdictDifferences || o.Counts.Differs != 1 {
		t.Fatalf("one approved step of two covered the whole cell: verdict %s", o.Verdict)
	}
	in2.Approvals = []Approval{step(c2, "a1", "read"), step(c2, "a2", "create")}
	if o := Result(in2); !cellOf(t, o, "C-1", "new").Approved || o.Verdict != VerdictSameWithApproved {
		t.Fatalf("both differing steps approved and the cell does not read approved: %s", o.Verdict)
	}
	// revoking one of them uncovers the cell again
	in2.Approvals[1].Revoked = true
	if o := Result(in2); cellOf(t, o, "C-1", "new").Approved || o.Verdict != VerdictDifferences {
		t.Fatalf("a revoked step approval still covers its step")
	}
}

func TestCMP10_ASeedCellIsNeverApproved(t *testing.T) {
	in := Input{SetHash: setH, Members: twoMembers(),
		Checks: []Check{{ID: "A-SEED", Path: "s", Rules: measured(t), Seed: true}, {ID: "C-1", Path: "c", Rules: measured(t)}}}
	in.Runs = []Run{
		run("r1", "old", "v1", "passed", out("A-SEED", hx("1")), out("C-1", hx("1"))),
		run("r2", "old", "v1", "passed", out("A-SEED", hx("1")), out("C-1", hx("1"))),
		run("r3", "new", "v2", "passed", out("A-SEED", hx("2")), out("C-1", hx("1"))),
	}
	c := cellOf(t, Result(in), "A-SEED", "new")
	if c.State != StateDiffers {
		t.Fatalf("fixture: %+v", c)
	}
	a := Approval{ID: "a1", Check: "A-SEED", Member: "new", VersionKey: "v2", ReferenceHash: c.ReferenceHash, MemberHash: c.MemberHash}
	in.Approvals = []Approval{a}
	o := Result(in)
	if got := cellOf(t, o, "A-SEED", "new"); got.Approved {
		t.Fatalf("a seed cell was approved: %+v", got)
	}
	if o.Verdict != VerdictIncomplete {
		t.Fatalf("verdict = %s: a seed that did not hold keeps the roll-up incomplete whatever is approved", o.Verdict)
	}
}

func TestCMP10_ClaimCellsOfFixedAndPropertyChecksAreApprovedAtTheirOwnHash(t *testing.T) {
	in := Input{SetHash: setH, Members: []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}},
		Checks: []Check{{ID: "F-1", Path: "f", Rules: mustRules(t, "Reference", "fixed")}}}
	mk := func(id, member, outcome string) Run {
		return Run{ID: id, Member: member, VersionKey: "v", Status: "completed", SetHash: setH, Outcomes: map[string]string{"F-1": outcome}}
	}
	in.Runs = []Run{mk("r1", "old", "passed"), mk("r2", "new", "failed")}
	c := cellOf(t, Result(in), "F-1", "new")
	if c.State != StateDiffers || c.ReferenceHash != "" || c.MemberHash == "" {
		t.Fatalf("fixture: %+v", c)
	}
	in.Approvals = []Approval{{ID: "a1", Check: "F-1", Member: "new", VersionKey: "v", MemberHash: c.MemberHash}}
	o := Result(in)
	if !cellOf(t, o, "F-1", "new").Approved || o.Verdict != VerdictSameWithApproved {
		t.Fatalf("a failed claim approved at its own hash does not read approved: %s", o.Verdict)
	}
	// the claim moves (the member now fails 1 of 2 runs): the approval was for 0 of 1
	in.Runs = append(in.Runs, mk("r3", "new", "passed"))
	if o2 := Result(in); cellOf(t, o2, "F-1", "new").Approved {
		t.Fatalf("a claim that moved after the approval still counts it")
	}
}

func TestCMP10_TheResultDoesNotDependOnTheOrderTheApprovalsArriveIn(t *testing.T) {
	in, c := differing(t)
	a1, a2, a3 := approvalFor(c, "apr_1"), approvalFor(c, "apr_2"), approvalFor(c, "apr_3")
	a2.Revoked = true
	a3.MemberHash = hx("9")
	in.Approvals = []Approval{a1, a2, a3}
	h1 := Result(in).Hash()
	in.Approvals = []Approval{a3, a1, a2}
	h2 := Result(in).Hash()
	in.Approvals = []Approval{a2, a3, a1}
	if h3 := Result(in).Hash(); h1 != h2 || h1 != h3 {
		t.Fatalf("the result hash depends on the order of the approvals: %s %s %s", h1, h2, h3)
	}
}

func TestCMP10_TheEncodingIsUnchangedWithoutApprovalsAndCarriesNoFreeText(t *testing.T) {
	in, c := differing(t)
	base := string(Result(in).JSON())
	if strings.Contains(base, `"approvals"`) {
		t.Fatalf("a comparison with no approval encodes an approvals key: the result hash of every existing comparison would move")
	}
	in.Approvals = []Approval{approvalFor(c, "apr_1")}
	with := Result(in)
	var m map[string]any
	if err := json.Unmarshal(with.JSON(), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with.JSON()), `"id":"apr_1"`) || !strings.Contains(string(with.JSON()), `"status":"live"`) {
		t.Fatalf("the approval is not in the encoding: %s", with.JSON())
	}
	if with.Hash() == Result(Input{SetHash: in.SetHash, Checks: in.Checks, Members: in.Members, Runs: in.Runs}).Hash() {
		t.Fatalf("approving did not change the result hash, so a settled verdict could not tell")
	}
}

func TestCMP10_ApprovalHashIsOverTheCanonicalFormAndNothingElse(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 123456000, time.UTC)
	rec := ApprovalRecord{ApprovalID: "apr_1", ComparisonID: "cmp_1", Check: "C-1", Step: "read", Member: "new", VersionKey: "v2",
		ReferenceHash: hx("1"), MemberHash: hx("2"), At: at}
	h := ApprovalHash(rec, false)
	if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("approval hash = %q, want 64 lowercase hex", h)
	}
	if h != ApprovalHash(rec, false) {
		t.Fatalf("the hash is not deterministic")
	}
	if rev := ApprovalHash(rec, true); rev == h || len(rev) != 64 {
		t.Fatalf("a revocation of the same record hashes like the approval: %s", rev)
	}
	// every field moves the hash
	for name, mod := range map[string]func(r *ApprovalRecord){
		"approval id":    func(r *ApprovalRecord) { r.ApprovalID = "apr_2" },
		"comparison id":  func(r *ApprovalRecord) { r.ComparisonID = "cmp_2" },
		"check":          func(r *ApprovalRecord) { r.Check = "C-2" },
		"step":           func(r *ApprovalRecord) { r.Step = "create" },
		"member":         func(r *ApprovalRecord) { r.Member = "old" },
		"version":        func(r *ApprovalRecord) { r.VersionKey = "v3" },
		"reference hash": func(r *ApprovalRecord) { r.ReferenceHash = hx("3") },
		"member hash":    func(r *ApprovalRecord) { r.MemberHash = hx("3") },
		"time":           func(r *ApprovalRecord) { r.At = at.Add(time.Microsecond) },
	} {
		r := rec
		mod(&r)
		if ApprovalHash(r, false) == h {
			t.Errorf("changing the %s does not change the approval hash", name)
		}
	}
	// the encoding is unambiguous: a value cannot run into its neighbour
	a := ApprovalRecord{ApprovalID: "ab", ComparisonID: "c"}
	b := ApprovalRecord{ApprovalID: "a", ComparisonID: "bc"}
	if ApprovalHash(a, false) == ApprovalHash(b, false) {
		t.Errorf("(ab, c) and (a, bc) hash alike")
	}
	// the time is read in UTC: the same instant in another zone is the same hash
	r := rec
	r.At = at.In(time.FixedZone("x", 7200))
	if ApprovalHash(r, false) != h {
		t.Errorf("the same instant in another zone hashes differently")
	}
}

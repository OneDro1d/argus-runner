package compare

// outputruns_test.go -- ARGUS-CMP-11 fix 2, G1: which recorded runs a cell's two hashes stand for, so that the author's page can open them
// (the control plane view adds them as `output_runs`). Built on the golden input of golden_f4_test.go.

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

func orCell(t *testing.T, in Input, check, member string) (Output, Cell) {
	t.Helper()
	o := Result(in)
	for _, cr := range o.Checks {
		if cr.ID != check {
			continue
		}
		for _, c := range cr.Cells {
			if c.Member == member {
				return o, c
			}
		}
	}
	t.Fatalf("no cell %s / %s", check, member)
	return o, Cell{}
}

func orWant(t *testing.T, got *OutputRunRef, run, member string) {
	t.Helper()
	if got == nil {
		t.Fatalf("run ref missing, want %s/%s", run, member)
	}
	if got.RunID != run || got.Member != member {
		t.Errorf("run ref = %+v, want %s/%s", *got, run, member)
	}
}

func TestCMP11_Fix2_G1_ADifferingCellNamesTheMemberRunItsHashStandsForNotItsFirstRun(t *testing.T) {
	in := goldInput(t)
	// G-3: the member's FIRST run (r4) agrees with the reference; its second (r5) does not. The cell's member_hash covers both and the
	// difference is in r5: the drawer must open r5, not r4.
	_, c := orCell(t, in, "G-3", "new")
	if c.State != StateDiffers {
		t.Fatalf("G-3 new = %s, want differs", c.State)
	}
	got := OutputRunsFor(in, "G-3", c)
	orWant(t, got.Reference, "r10", "old")
	orWant(t, got.Member, "r5", "new")
	if got.Control != nil {
		t.Errorf("a differing cell of a stable reference has a control: %+v", *got.Control)
	}
	// G-5: both member runs differ: the first is the pair
	_, c = orCell(t, in, "G-5", "new")
	got = OutputRunsFor(in, "G-5", c)
	orWant(t, got.Reference, "r10", "old")
	orWant(t, got.Member, "r4", "new")
}

func TestCMP11_Fix2_G1_AnIdenticalCellNamesTheFirstUsableReferenceRunAndTheMembersFirstAgreeingRun(t *testing.T) {
	m := goldRules(t, "Reference", "measured", "Agreement", "50%")
	in := Input{SetHash: "gold-set", Checks: []Check{{ID: "A-1", Path: "a.md", Rules: m}},
		Members: []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}},
		Runs: []Run{
			goldRun("r1", "old", "passed", goldRow("A-1", "1")),
			goldRun("r2", "old", "passed", goldRow("A-1", "1")),
			goldRun("r3", "new", "passed", goldRow("A-1", "9")), // disagrees
			goldRun("r4", "new", "passed", goldRow("A-1", "1")), // the first run that agrees
		}}
	_, c := orCell(t, in, "A-1", "new")
	if c.State != StateIdentical {
		t.Fatalf("A-1 new = %s, want identical (1 of 2 agreed at 50%%)", c.State)
	}
	got := OutputRunsFor(in, "A-1", c)
	orWant(t, got.Reference, "r1", "old")
	orWant(t, got.Member, "r4", "new")
	if got.Control != nil {
		t.Errorf("control on an identical cell: %+v", *got.Control)
	}
}

func TestCMP11_Fix2_G1_TheReferencesOwnCellHasAReferenceOnlyAndAnUnstableOneAlsoAControlThatDiffers(t *testing.T) {
	in := goldInput(t)
	// G-1: stable reference: its own cell names its first run and nothing else
	_, c := orCell(t, in, "G-1", "old")
	got := OutputRunsFor(in, "G-1", c)
	orWant(t, got.Reference, "r10", "old")
	if got.Member != nil || got.Control != nil {
		t.Errorf("the reference's own cell on a stable check = %+v, want reference only", got)
	}
	// G-2: r2 differs from r10 and r3: unstable. Reference r10, control r2 (the second usable run whose output differs from the first)
	_, c = orCell(t, in, "G-2", "old")
	if c.State != StateNoise {
		t.Fatalf("G-2 old = %s, want noise", c.State)
	}
	got = OutputRunsFor(in, "G-2", c)
	orWant(t, got.Reference, "r10", "old")
	orWant(t, got.Control, "r2", "old")
	if got.Member != nil {
		t.Errorf("the reference cell has a member ref: %+v", *got.Member)
	}
	// and the candidate's cell of the same noise check carries all three
	_, c = orCell(t, in, "G-2", "new")
	if c.State != StateNoise {
		t.Fatalf("G-2 new = %s, want noise", c.State)
	}
	got = OutputRunsFor(in, "G-2", c)
	orWant(t, got.Reference, "r10", "old")
	orWant(t, got.Member, "r4", "new")
	orWant(t, got.Control, "r2", "old")
}

func TestCMP11_Fix2_G1_ACellThatIsNotMeasuredCouldNotRunOrOfAFixedPropertyOrLoadOnlyCheckNamesNoRun(t *testing.T) {
	in := goldInput(t)
	// G-4 is a property check
	_, c := orCell(t, in, "G-4", "new")
	if got := OutputRunsFor(in, "G-4", c); got.Reference != nil || got.Member != nil || got.Control != nil {
		t.Errorf("a property cell names runs: %+v", got)
	}
	// a member with no run at all: not measured
	in.Members = append(in.Members, Member{Name: "ghost", Role: RoleCandidate})
	_, c = orCell(t, in, "G-1", "ghost")
	if c.State != StateNotMeasured {
		t.Fatalf("ghost = %s, want not_measured", c.State)
	}
	if got := OutputRunsFor(in, "G-1", c); got.Reference != nil || got.Member != nil || got.Control != nil {
		t.Errorf("a not_measured cell names runs: %+v", got)
	}
	// a member whose only run failed: could not run
	in.Runs = append(in.Runs, Run{ID: "r7", Member: "lost", VersionKey: "v", Status: "failed", SetHash: "gold-set"})
	in.Members = append(in.Members, Member{Name: "lost", Role: RoleCandidate})
	_, c = orCell(t, in, "G-1", "lost")
	if c.State != StateCouldNotRun {
		t.Fatalf("lost = %s, want could_not_run", c.State)
	}
	if got := OutputRunsFor(in, "G-1", c); got.Reference != nil || got.Member != nil || got.Control != nil {
		t.Errorf("a could_not_run cell names runs: %+v", got)
	}
}

func TestCMP11_Fix2_G1_ATolerantValueDifferenceNamesTheSamePairOfRunsValueDiffsComesFrom(t *testing.T) {
	r := goldRules(t, "Reference", "measured", "Tolerance", "$.total abs 0.01")
	row := func(id string, v float64) ScenarioOutput {
		s := goldRow(id, "1")
		s.Values = []ToleranceValue{{Path: "$.total", Rule: 0, Value: v}}
		return s
	}
	in := Input{SetHash: "gold-set", Checks: []Check{{ID: "T-1", Path: "t.md", Rules: r}},
		Members: []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}},
		Runs: []Run{
			goldRun("r1", "old", "passed", row("T-1", 100)),
			goldRun("r2", "old", "passed", row("T-1", 100)),
			goldRun("r3", "new", "passed", row("T-1", 100.004)), // within tolerance
			goldRun("r4", "new", "passed", row("T-1", 100.5)),   // outside it: value_diffs come from r4 against r1
		}}
	_, c := orCell(t, in, "T-1", "new")
	if c.State != StateDiffers {
		t.Fatalf("T-1 new = %s, want differs", c.State)
	}
	if vd := ValueDiffsFor(in, "T-1", "new", c.VersionKey); len(vd) != 1 || vd[0].Member == nil || *vd[0].Member != 100.5 {
		t.Fatalf("value_diffs = %+v, want the number of r4", vd)
	}
	got := OutputRunsFor(in, "T-1", c)
	orWant(t, got.Reference, "r1", "old")
	orWant(t, got.Member, "r4", "new")
}

// G2: `unsupported` is a number of the control plane's VIEW. compare.Counts (which result_hash and every anchored verdict payload are
// computed over) holds exactly the keys it always held; the golden hash test holds the hash itself.
func TestCMP11_Fix2_G2_CompareCountsHoldNoUnsupportedKeyAndTheVerdictPayloadKeysAreUnchanged(t *testing.T) {
	b, err := json.Marshal(Result(goldInput(t)).Counts)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"approved", "checks", "could_not_run", "differs", "identical", "members", "noise", "not_measured", "runs", "runs_pending", "worse"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("compare.Counts keys = %v, want %v", keys, want)
	}
}

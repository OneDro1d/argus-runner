package compare

// golden_f4_test.go -- ARGUS-CMP-11 fix F4: a comparison that declares no Tolerance, no Not Worse Than and no target is judged exactly as
// it was before ARGUS-CMP-11 and before the pairwise rule. The input below uses nothing but fields and helpers of the released shape, so the
// SAME file, copied unchanged into a worktree of origin/dev or of the PR's starting head, must print the SAME hash.

import (
	"strings"
	"testing"
)

// goldResultHash is Output.Hash() of goldInput, measured at 83deb25e (the PR's starting head) and at origin/dev (050608b): equal.
const goldResultHash = "fee178f22018114efc33f58f9ce3010b006ddd1a3ba67d898f14a3b3d1f033e1"

func goldHex(c string) string { return strings.Repeat(c, 64) }

func goldRow(id, body string) ScenarioOutput {
	return ScenarioOutput{ScenarioID: id, OutputRecord: OutputRecord{
		V: 1, Sample: 1, State: StateRecorded, Status: 200, Hash: goldHex(body),
		Parts: Parts{Status: goldHex("a"), Body: goldHex(body)}, BodyKind: KindJSON,
	}}
}

func goldRun(id, member, outcome string, rows ...ScenarioOutput) Run {
	oc := map[string]string{}
	for _, r := range rows {
		oc[r.ScenarioID] = outcome
	}
	return Run{ID: id, Member: member, VersionKey: "v", Status: "completed", SetHash: "gold-set", Outcomes: oc, Outputs: rows}
}

func goldRules(t *testing.T, kvs ...string) *Rules {
	t.Helper()
	var raw []RawKV
	for i := 0; i+1 < len(kvs); i += 2 {
		raw = append(raw, RawKV{Key: kvs[i], Value: kvs[i+1], Line: i/2 + 1})
	}
	r, probs := BuildRules(raw)
	if len(probs) > 0 {
		t.Fatalf("BuildRules(%v): %+v", kvs, probs)
	}
	return r
}

func goldInput(t *testing.T) Input {
	measured := goldRules(t, "Reference", "measured")
	property := goldRules(t, "Reference", "property", "Agreement", "100%")
	return Input{
		SetHash: "gold-set",
		Checks: []Check{
			{ID: "G-1", Path: "g/1.md", Rules: measured},
			{ID: "G-2", Path: "g/2.md", Rules: measured},
			{ID: "G-3", Path: "g/3.md", Rules: measured},
			{ID: "G-4", Path: "g/4.md", Rules: property},
			{ID: "G-5", Path: "g/5.md", Rules: measured},
		},
		Members: []Member{{Name: "old", Role: RoleReference}, {Name: "new", Role: RoleCandidate}},
		Runs: []Run{
			// the run ids sort as text on purpose: "r10" < "r2"
			goldRun("r10", "old", "passed", goldRow("G-1", "1"), goldRow("G-2", "2"), goldRow("G-3", "3"), goldRow("G-4", "4"), goldRow("G-5", "5")),
			goldRun("r2", "old", "passed", goldRow("G-1", "1"), goldRow("G-2", "6"), goldRow("G-3", "3"), goldRow("G-4", "4"), goldRow("G-5", "5")),
			goldRun("r3", "old", "passed", goldRow("G-1", "1"), goldRow("G-2", "2"), goldRow("G-3", "3"), goldRow("G-4", "4"), goldRow("G-5", "5")),
			goldRun("r4", "new", "passed", goldRow("G-1", "1"), goldRow("G-2", "2"), goldRow("G-3", "3"), goldRow("G-4", "4"), goldRow("G-5", "7")),
			goldRun("r5", "new", "passed", goldRow("G-1", "1"), goldRow("G-2", "2"), goldRow("G-3", "8"), goldRow("G-4", "4"), goldRow("G-5", "7")),
			{ID: "r6", Member: "new", VersionKey: "v", Status: "failed", SetHash: "gold-set"},
		},
	}
}

func TestCMP11_F4_AComparisonWithNoToleranceNoBandAndNoTargetHasTheResultHashItAlwaysHad(t *testing.T) {
	o := Result(goldInput(t))
	got := o.Hash()
	t.Logf("GOLDEN result_hash = %s", got)
	if got != goldResultHash {
		t.Errorf("result_hash = %s, want %s", got, goldResultHash)
	}
}

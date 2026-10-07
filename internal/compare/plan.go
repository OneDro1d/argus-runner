package compare

import "strconv"

// RunPlan is how many whole runs each member of a comparison gets (ARGUS-CMP-6, design 5 and 7.2).
// Repeats are WHOLE RUNS: each is its own run request, ledger row and evidence hash.
type RunPlan struct {
	Set       int // R_set: the largest Repeats any check of the set declares (1 when none does), never above MaxRepeats
	Reference int // runs of the reference member: the first is the reference, the others are controls
	Candidate int // runs of each candidate member
}

// PlanRuns reads the checks' rules (nil entries are checks that declare no `## COMPARE` and are skipped).
//
// The reference runs max(2, R_set) times when at least one check is `measured`: with fewer than two runs
// the reference's own variation cannot be seen, so the noise rule could never fire. Without a measured
// check there is nothing to be noisy about, and the reference runs R_set times like every other member.
// R_set is clamped to MaxRepeats here as a second line behind SetRepeatsRefusal: no stored or hand-built
// rule can make the control plane queue an unbounded number of runs.
func PlanRuns(rules []*Rules) RunPlan {
	set, measured := 1, false
	for _, r := range rules {
		if r == nil {
			continue
		}
		if r.Repeats > set {
			set = r.Repeats
		}
		if r.Reference == RefMeasured {
			measured = true
		}
	}
	if set > MaxRepeats {
		set = MaxRepeats
	}
	p := RunPlan{Set: set, Reference: set, Candidate: set}
	if measured && p.Reference < 2 {
		p.Reference = 2
	}
	return p
}

// SetRepeatsRefusal is the set-wide cap (MaxRepeats) of a set about to be compared: "" when every check is
// within it, else a closed sentence naming the check's path (the author's own) and the cap. The `## COMPARE`
// parser already bounds Repeats, so this refuses a rule that did not come through it, at create.
func SetRepeatsRefusal(entries []PathRules) string {
	for _, e := range entries {
		if e.Rules != nil && e.Rules.Repeats > MaxRepeats {
			return "check " + e.Path + " declares more repeats than the set-wide cap of " + strconv.Itoa(MaxRepeats) + " runs per member"
		}
	}
	return ""
}

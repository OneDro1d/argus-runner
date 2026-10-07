package compare

// outputruns.go -- ARGUS-CMP-11 fix 2, G1: which recorded runs a cell's two hashes stand for, so that the author's page can open them
// through the output read.
//
// ⛔ CUSTODY. A run id is author data in the same class as value_diffs: OutputRunsFor is called by the author's comparison read
// (author_get_comparison and GET /api/comparison, author hat) and by nothing else. Nothing here is part of Output, Cell, result_hash or an
// anchor payload.

// OutputRunRef names one run and the member that ran it.
type OutputRunRef struct {
	RunID  string `json:"run_id"`
	Member string `json:"member"`
}

// OutputRuns are the runs a cell opens: the reference's, the member's, and (for a reference that disagrees with itself) a control run of
// the reference whose output differs from the reference's. A nil entry is a run that does not exist.
type OutputRuns struct {
	Reference *OutputRunRef `json:"reference,omitempty"`
	Member    *OutputRunRef `json:"member,omitempty"`
	Control   *OutputRunRef `json:"control,omitempty"`
}

// Empty says no run is named.
func (o OutputRuns) Empty() bool { return o.Reference == nil && o.Member == nil && o.Control == nil }

func refOf(k *cls) *OutputRunRef { return &OutputRunRef{RunID: k.run.ID, Member: k.run.Member} }

// firstDisagreement is the first (member run in id order, reference run in id order) pair that does not agree. With valuesOnly the pair
// must differ in its tolerant values (ValueDiffs non-empty): that is the pair value_diffs is read from.
func firstDisagreement(rules *Rules, refUsable, own []cls, valuesOnly bool) (ref, mem *cls) {
	for i := range own {
		for j := range refUsable {
			if valuesOnly {
				if len(ValueDiffs(rules, refUsable[j].recs, own[i].recs)) > 0 {
					return &refUsable[j], &own[i]
				}
			} else if ok, _ := RecordsAgree(rules, refUsable[j].recs, own[i].recs); !ok {
				return &refUsable[j], &own[i]
			}
		}
	}
	return nil, nil
}

// OutputRunsFor names the runs of ONE cell (a cell of check checkID, as Result returned it) of the comparison in.
//
//   - a cell that differs: the pair its hashes differ in. When the cell carries value_diffs, the SAME pair of runs those numbers come from
//     (ValueDiffsFor); otherwise the first member run (in run-id order) that disagrees with some usable reference run, against the first
//     such reference run;
//   - an identical cell: the first usable reference run and the member's first run that agrees with every usable reference run;
//   - the reference's own cell: its first usable run; and, when the reference is unstable on the check, a control: the first usable
//     reference run whose output differs from the first (the first disagreeing pair when none disagrees with the first, a tolerance not
//     being transitive);
//   - a candidate's cell of an unstable (noise) check: the reference and control as above, and the member's first usable run.
//
// A cell that is not measured or could not run, and any cell of a fixed, property or load-only check, names nothing. The runs are the
// usable ones, chosen exactly as Result chooses them (classify), so each is one the output read accepts for the cell's check.
func OutputRunsFor(in Input, checkID string, cell Cell) OutputRuns {
	var none OutputRuns
	switch cell.State {
	case StateIdentical, StateDiffers, StateNoise:
	default:
		return none
	}
	c := newCalc(in)
	var ck *Check
	for i := range c.in.Checks {
		if c.in.Checks[i].ID == checkID {
			ck = &c.in.Checks[i]
		}
	}
	if ck == nil || ck.Rules == nil || ck.Rules.Reference != RefMeasured || c.refMember == "" || c.refAmbig {
		return none
	}
	var refUsable, own []cls
	for _, g := range c.groups(ck) {
		switch {
		case g.member == c.refMember && g.version == c.refVer:
			refUsable = statsOf(g).ok
		case g.member == cell.Member && g.version == cell.VersionKey:
			own = statsOf(g).ok
		}
	}
	if len(refUsable) == 0 {
		return none
	}
	var out OutputRuns
	out.Reference = refOf(&refUsable[0])
	isRef := cell.Member == c.refMember && cell.VersionKey == c.refVer

	if cell.State == StateNoise {
		refK, ctlK := &refUsable[0], (*cls)(nil)
		for j := 1; j < len(refUsable) && ctlK == nil; j++ {
			if ok, _ := RecordsAgree(ck.Rules, refUsable[0].recs, refUsable[j].recs); !ok {
				ctlK = &refUsable[j]
			}
		}
		for i := 0; i < len(refUsable) && ctlK == nil; i++ {
			for j := i + 1; j < len(refUsable) && ctlK == nil; j++ {
				if ok, _ := RecordsAgree(ck.Rules, refUsable[i].recs, refUsable[j].recs); !ok {
					refK, ctlK = &refUsable[i], &refUsable[j]
				}
			}
		}
		out.Reference = refOf(refK)
		if ctlK != nil {
			out.Control = refOf(ctlK)
		}
		if !isRef && len(own) > 0 {
			out.Member = refOf(&own[0])
		}
		return out
	}
	if isRef || len(own) == 0 {
		return out
	}
	switch cell.State {
	case StateDiffers:
		var refK, memK *cls
		for _, p := range cell.PartsDiffer {
			if p == "values" {
				refK, memK = firstDisagreement(ck.Rules, refUsable, own, true)
			}
		}
		if memK == nil {
			refK, memK = firstDisagreement(ck.Rules, refUsable, own, false)
		}
		if memK == nil { // a difference the pairwise rule does not show (a claim by agreement rate): the first runs
			refK, memK = &refUsable[0], &own[0]
		}
		out.Reference, out.Member = refOf(refK), refOf(memK)
	default: // identical
		memK := &own[0]
		for i := range own {
			agreed := true
			for j := range refUsable {
				if ok, _ := RecordsAgree(ck.Rules, refUsable[j].recs, own[i].recs); !ok {
					agreed = false
					break
				}
			}
			if agreed {
				memK = &own[i]
				break
			}
		}
		out.Member = refOf(memK)
	}
	return out
}

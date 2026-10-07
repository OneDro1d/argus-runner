package compare

import (
	"sort"
	"strconv"
)

// valuediffs.go -- ARGUS-CMP-11 fix F6: the two numbers that made a tolerant cell differ, for the AUTHOR to read.
//
// ⛔ CUSTODY. These are the measured numbers of a response. They are response data, in the same class as the stored `outputs` rows they
// come from: ValueDiffsFor is called by the author's comparison read (author_get_comparison and GET /api/comparison, author hat) and by
// nothing else. They are not part of Output, Cell, result_hash, an anchor payload, an alert, an attention item, a log line or a public
// page, and no builder-readable answer carries them.

// MaxValueDiffs bounds the value_diffs of one cell.
const MaxValueDiffs = 64

// ValueDiff is one tolerant leaf that is outside its declared tolerance, for one pair of runs. Path is the leaf's own recorded path,
// Declared the `**Tolerance**` rule's path, Tolerance the declared amount ("abs 0.01"). Reference / Member are null for a leaf that the
// run did not record (present on one side only).
type ValueDiff struct {
	Step      string   `json:"step,omitempty"`
	Path      string   `json:"path"`
	Declared  string   `json:"declared"`
	Reference *float64 `json:"reference"`
	Member    *float64 `json:"member"`
	Tolerance string   `json:"tolerance"`
}

func fptr(f float64) *float64 { return &f }

// ValueDiffs lists the tolerant leaves at which got is outside the declared tolerance of ref, in (step, path) order, at most
// MaxValueDiffs. It is empty when the two runs do not record the same (step, sample) rows (that difference is `samples`, not `values`).
// The path of a leaf is the record's own (ToleranceValue.Path) and its rule is the declaration's: nothing is parsed out of a string.
func ValueDiffs(rules *Rules, ref, got []ScenarioOutput) []ValueDiff {
	if rules == nil {
		return nil
	}
	a, b := sortedRows(ref), sortedRows(got)
	if len(a) != len(b) {
		return nil
	}
	tols := rules.normalized().Tolerance
	describe := func(rule int) (declared, tol string) {
		if rule < 0 || rule >= len(tols) {
			return "", "exact"
		}
		return tols[rule].Path, tols[rule].Kind + " " + strconv.FormatFloat(tols[rule].Value, 'f', -1, 64)
	}
	var out []ValueDiff
	for i := range a {
		if a[i].Step != b[i].Step || a[i].Sample != b[i].Sample {
			return nil
		}
		byPath := map[string]ToleranceValue{}
		for _, v := range b[i].Values {
			byPath[v.Path] = v
		}
		seen := map[string]bool{}
		for _, r := range a[i].Values {
			seen[r.Path] = true
			g, ok := byPath[r.Path]
			declared, tol := describe(r.Rule)
			switch {
			case !ok || g.Rule != r.Rule:
				d := ValueDiff{Step: a[i].Step, Path: r.Path, Declared: declared, Reference: fptr(r.Value), Tolerance: tol}
				if ok {
					d.Member = fptr(g.Value)
				}
				out = append(out, d)
			case r.Rule < 0 || r.Rule >= len(tols):
				if r.Value != g.Value {
					out = append(out, ValueDiff{Step: a[i].Step, Path: r.Path, Declared: declared, Reference: fptr(r.Value), Member: fptr(g.Value), Tolerance: tol})
				}
			case !TolerancesAgree(tols[r.Rule], r.Value, g.Value):
				out = append(out, ValueDiff{Step: a[i].Step, Path: r.Path, Declared: declared, Reference: fptr(r.Value), Member: fptr(g.Value), Tolerance: tol})
			}
		}
		for _, g := range b[i].Values {
			if !seen[g.Path] {
				declared, tol := describe(g.Rule)
				out = append(out, ValueDiff{Step: b[i].Step, Path: g.Path, Declared: declared, Member: fptr(g.Value), Tolerance: tol})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Step != out[j].Step {
			return out[i].Step < out[j].Step
		}
		return out[i].Path < out[j].Path
	})
	if len(out) > MaxValueDiffs {
		out = out[:MaxValueDiffs]
	}
	return out
}

// ValueDiffsFor is the value_diffs of ONE cell (check, member, version) of the comparison `in`, for the author's read: the usable
// runs are chosen exactly as Result chooses them (classify), and the pair is the first member run, in run-id order, that does not agree
// with some usable reference run in its tolerant values, against the first such reference run. Nil when there is no such pair.
func ValueDiffsFor(in Input, checkID, member, version string) []ValueDiff {
	c := newCalc(in)
	var ck *Check
	for i := range c.in.Checks {
		if c.in.Checks[i].ID == checkID {
			ck = &c.in.Checks[i]
		}
	}
	if ck == nil || ck.Rules == nil || ck.Rules.Reference != RefMeasured || c.refMember == "" || c.refAmbig || member == c.refMember && version == c.refVer {
		return nil
	}
	var refUsable, own []cls
	for _, g := range c.groups(ck) {
		switch {
		case g.member == c.refMember && g.version == c.refVer:
			refUsable = statsOf(g).ok
		case g.member == member && g.version == version:
			own = statsOf(g).ok
		}
	}
	for _, u := range own {
		for _, ru := range refUsable {
			if d := ValueDiffs(ck.Rules, ru.recs, u.recs); len(d) > 0 {
				return d
			}
		}
	}
	return nil
}

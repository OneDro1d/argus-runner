package compare

import (
	"math"
	"sort"
)

const eps = 1e-9

// WithinTolerance reports whether got is within the declared tolerance of ref. abs: |got-ref| <= v.
// rel: |got-ref| <= v*|ref|, relative to the REFERENCE value, so a zero reference admits only zero.
func WithinTolerance(t Tolerance, ref, got float64) bool {
	d := math.Abs(got - ref)
	switch t.Kind {
	case "abs":
		return d <= t.Value+eps*math.Max(1, math.Abs(ref))*1e-3
	case "rel":
		return d <= t.Value*math.Abs(ref)*(1+1e-12)
	}
	return false
}

// TolerancesAgree is the ONE agreement of two runs' tolerant values (ARGUS-CMP-11 fix F4): each is within the tolerance of the other.
// For `abs` that is |a-b| <= v. For `rel` it means |a-b| <= v * the SMALLER of |a| and |b|: relative to the smaller magnitude of the two
// values, so it does not matter which of two runs is called the reference, and a zero admits only zero. (WithinTolerance, one-sided, is
// relative to ref; the judge never uses it alone any more.) The relation is still not transitive, which is why the reference's stability
// and a candidate's agreement are judged over every pair / every reference run (result_calc.go), never against one chosen run.
func TolerancesAgree(t Tolerance, a, b float64) bool {
	return WithinTolerance(t, a, b) && WithinTolerance(t, b, a)
}

// JudgeClaim judges a fixed or property claim from a member's per-run outcomes (design 3): it holds
// when the check passed in at least pct percent of the runs that could be judged. An errored run
// (or an outcome this build does not know) is "could not run": excluded from n and reported, never
// counted as a failure of the property.
func JudgeClaim(outcomes []string, pct float64) ClaimJudgement {
	var j ClaimJudgement
	pct = EffectiveAgreement(pct)
	failed := 0
	for _, o := range outcomes {
		switch o {
		case OutcomePassed, OutcomeDegraded:
			j.Held++
		case OutcomeFailed:
			failed++
		default:
			j.CouldNotRun++
		}
	}
	j.Runs = j.Held + failed
	switch {
	case j.Runs == 0 && j.CouldNotRun > 0:
		j.State = StateCouldNotRun
	case j.Runs == 0:
		j.State = StateNotMeasured
	case float64(j.Held)*100 >= pct*float64(j.Runs)-eps:
		j.State = StateIdentical
	default:
		j.State = StateDiffers
	}
	if j.Runs > 0 {
		d := j.Runs - j.Held
		j.ClaimSupported = ClaimSupported(d, j.Runs, pct)
		j.Supports = SupportsFor(d, j.Runs, pct)
		if !j.ClaimSupported {
			j.Unsupported = UnsupportedFor(d, j.Runs, pct)
		}
	}
	return j
}

// Median of v; v is not reordered. 0 for no values.
func Median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// JudgeBand judges one `Not Worse Than` entry for one member: the median over its runs against the
// median over the reference's. A percent band allows member <= reference x (1 + band); a pp band
// (error_rate, held as a fraction) allows member <= reference + band percentage points. Better is
// always fine. With no numbers on either side the cell is not measured.
func JudgeBand(b Band, ref, got []float64) BandJudgement {
	j := BandJudgement{
		Reference: Median(ref), Member: Median(got),
		ReferenceRuns: len(ref), MemberRuns: len(got),
	}
	if len(ref) == 0 || len(got) == 0 {
		j.State = BandNotMeasured
		return j
	}
	var ok bool
	switch b.Unit {
	case "pp":
		ok = j.Member*100 <= j.Reference*100+b.Value+eps
	default:
		ok = j.Member <= j.Reference*(1+b.Value/100)+eps*math.Max(1, math.Abs(j.Reference))
	}
	if ok {
		j.State = BandNotWorse
	} else {
		j.State = BandWorse
	}
	return j
}

var partOrder = []string{"status", "headers", "body"}

func sortedRows(rows []ScenarioOutput) []ScenarioOutput {
	cp := append([]ScenarioOutput(nil), rows...)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].Step != cp[j].Step {
			return cp[i].Step < cp[j].Step
		}
		return cp[i].Sample < cp[j].Sample
	})
	return cp
}

func partOf(p Parts, name string) string {
	switch name {
	case "status":
		return p.Status
	case "headers":
		return p.Headers
	}
	return p.Body
}

// RecordsAgree compares one run's rows for a check with the reference's: the same (step, sample)
// rows, the same hash on each, and every tolerant value within its declared tolerance. parts names
// what differs, drawn from a closed list in a fixed order: samples, status, headers, body, values.
func RecordsAgree(rules *Rules, ref, got []ScenarioOutput) (bool, []string) {
	a, b := sortedRows(ref), sortedRows(got)
	if len(a) != len(b) {
		return false, []string{"samples"}
	}
	for i := range a {
		if a[i].Step != b[i].Step || a[i].Sample != b[i].Sample {
			return false, []string{"samples"}
		}
	}
	flag := map[string]bool{}
	for i := range a {
		if a[i].Hash != b[i].Hash {
			any := false
			for _, p := range partOrder {
				if partOf(a[i].Parts, p) != partOf(b[i].Parts, p) {
					flag[p] = true
					any = true
				}
			}
			if !any {
				flag["body"] = true
			}
		}
		if !valuesAgree(rules, a[i].Values, b[i].Values) {
			flag["values"] = true
		}
	}
	var parts []string
	for _, p := range append(append([]string{}, partOrder...), "values") {
		if flag[p] {
			parts = append(parts, p)
		}
	}
	return len(parts) == 0, parts
}

// ValueDiffPaths names the DECLARED `**Tolerance**` paths (the rule's own path, from the check's sealed declaration)
// at which one run's tolerant values do not agree with the reference's: a value outside its tolerance, or a tolerant
// leaf present on one side only. Sorted and unique. It never reads a value into its answer, and it is empty when the
// two runs do not record the same (step, sample) rows (that difference is `samples`, not `values`).
func ValueDiffPaths(rules *Rules, ref, got []ScenarioOutput) []string {
	if rules == nil {
		return nil
	}
	a, b := sortedRows(ref), sortedRows(got)
	if len(a) != len(b) {
		return nil
	}
	tols := rules.normalized().Tolerance
	marked := map[int]bool{}
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
			switch {
			case !ok || g.Rule != r.Rule:
				marked[r.Rule] = true
			case r.Rule < 0 || r.Rule >= len(tols):
				if r.Value != g.Value {
					marked[r.Rule] = true
				}
			case !TolerancesAgree(tols[r.Rule], r.Value, g.Value):
				marked[r.Rule] = true
			}
		}
		for _, g := range b[i].Values {
			if !seen[g.Path] {
				marked[g.Rule] = true
			}
		}
	}
	var out []string
	for idx := range marked {
		if idx >= 0 && idx < len(tols) {
			out = unionSorted(out, []string{tols[idx].Path})
		}
	}
	return out
}

func valuesAgree(rules *Rules, ref, got []ToleranceValue) bool {
	if len(ref) != len(got) {
		return false
	}
	var tols []Tolerance
	if rules != nil {
		tols = rules.normalized().Tolerance
	}
	byPath := map[string]ToleranceValue{}
	for _, v := range got {
		byPath[v.Path] = v
	}
	for _, r := range ref {
		g, ok := byPath[r.Path]
		if !ok || g.Rule != r.Rule {
			return false
		}
		if r.Rule < 0 || r.Rule >= len(tols) {
			if r.Value != g.Value {
				return false
			}
			continue
		}
		if !TolerancesAgree(tols[r.Rule], r.Value, g.Value) {
			return false
		}
	}
	return true
}

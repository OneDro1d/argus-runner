package compare

import (
	"fmt"
	"math"
	"strconv"
)

// The statistics rule (design 7.3). All of it is bounded: every loop below runs a fixed number of
// times or up to d, never until a floating-point condition happens to hold.

const alpha = 0.05 // one-sided 95%

// UpperBound95 is the exact one-sided 95% Clopper-Pearson upper bound on the true disagreement rate
// after d disagreements in n runs: the p at which P(X <= d | n, p) = 0.05. No claim is possible
// from no runs (n <= 0) or from all runs disagreeing (d >= n): the bound is 1. For d = 0 it is the
// closed form 1 - 0.05^(1/n), about 3/n.
func UpperBound95(d, n int) float64 {
	if n <= 0 || d >= n {
		return 1
	}
	if d <= 0 {
		return -math.Expm1(math.Log(alpha) / float64(n))
	}
	lgN := lgamma(float64(n) + 1)
	cdf := func(p float64) float64 {
		lp, lq := math.Log(p), math.Log1p(-p)
		s := 0.0
		for k := 0; k <= d; k++ {
			s += math.Exp(lgN - lgamma(float64(k)+1) - lgamma(float64(n-k)+1) + float64(k)*lp + float64(n-k)*lq)
		}
		return s
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 200 && hi-lo > 1e-15; i++ {
		mid := (lo + hi) / 2
		if cdf(mid) > alpha {
			lo = mid // still too likely: the true rate can be higher
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

func lgamma(x float64) float64 {
	v, _ := math.Lgamma(x)
	return v
}

// EffectiveAgreement is the ONE place an Agreement is read for judging: a value outside (0, 100] (zero
// from a hand-built or decoded Rules, a negative, NaN, above 100) is 100, the strictest reading, so a
// missing or corrupt Agreement can never make a claim easier to meet.
func EffectiveAgreement(pct float64) float64 {
	if !(pct > 0 && pct <= 100) {
		return 100
	}
	return pct
}

// MinRuns is the fewest runs, all agreeing, that support an agreement of pct percent at 95%
// confidence: ceil(ln 0.05 / ln p). DECISION: a rule of 100% is the plain statement "every run made
// agreed", which no number of runs makes more or less true, so it needs one run.
func MinRuns(pct float64) int {
	pct = EffectiveAgreement(pct)
	if pct >= 100 {
		return 1
	}
	n := int(math.Ceil(math.Log(alpha)/math.Log(pct/100) - 1e-9))
	if n < 1 {
		return 1
	}
	return n
}

// ClaimSupported reports whether d disagreements in n runs support an agreement of pct percent: the
// upper bound on the disagreement rate must not exceed 1 - pct. A 100% rule is supported by any
// n >= 1 with d = 0 (see MinRuns).
func ClaimSupported(d, n int, pct float64) bool {
	if n <= 0 {
		return false
	}
	pct = EffectiveAgreement(pct)
	if pct >= 100 {
		return d <= 0
	}
	return UpperBound95(d, n) <= 1-pct/100+1e-12
}

func runsWord(n int) string {
	if n == 1 {
		return "1 run"
	}
	return strconv.Itoa(n) + " runs"
}

// SupportsSentence says what n runs support, from a closed template. "" when n <= 0.
func SupportsSentence(d, n int) string {
	if n <= 0 {
		return ""
	}
	if d < 0 {
		d = 0
	}
	pct := int(math.Ceil(UpperBound95(d, n)*100 - 1e-9))
	if pct > 100 {
		pct = 100
	}
	if d == 0 {
		return fmt.Sprintf("with %s and no disagreement, the true disagreement rate is below %d%% (95%% confidence)", runsWord(n), pct)
	}
	return fmt.Sprintf("with %s, %d of %d runs disagreeing, the true disagreement rate is below %d%% (95%% confidence)", runsWord(n), d, n, pct)
}

// SupportsFor is SupportsSentence for a cell whose check declared an agreement of pct percent. For a 100%
// rule it adds the one clause that keeps the cell honest: the rule is about the runs made ("every run
// agreed"), and no number of runs proves it holds for every run to come. "" when n <= 0.
func SupportsFor(d, n int, pct float64) string {
	s := SupportsSentence(d, n)
	if s != "" && EffectiveAgreement(pct) >= 100 {
		s += "; no number of runs proves a 100% rate"
	}
	return s
}

// UnsupportedSentence is the refusal for a declared agreement that n runs cannot support. For 100% there
// is no number of runs that would: it says so instead of naming a run count.
func UnsupportedSentence(n int, pct float64) string {
	pct = EffectiveAgreement(pct)
	if pct >= 100 {
		return fmt.Sprintf("no number of runs proves 100%%; these %s did not all agree", runsWord(n))
	}
	p := strconv.FormatFloat(pct, 'f', -1, 64)
	return fmt.Sprintf("%s cannot support %s%%; that needs %s with no disagreement", runsWord(n), p, runsWord(MinRuns(pct)))
}

// UnsupportedFor is UnsupportedSentence for a cell with d disagreements in n runs. When there
// were ENOUGH runs and the claim is unsupported because runs disagreed, "that needs N runs with no disagreement"
// names a number the cell already has (seen live: "5 runs cannot support 20%; that needs 2 runs with no
// disagreement" on a property that held 0 of 5). The reason then is the disagreements, and the sentence says so.
// Too few runs (with or without a disagreement) and the 100% rule keep UnsupportedSentence.
func UnsupportedFor(d, n int, pct float64) string {
	eff := EffectiveAgreement(pct)
	if d > 0 && eff < 100 && n >= MinRuns(eff) {
		p := strconv.FormatFloat(eff, 'f', -1, 64)
		return fmt.Sprintf("%d of %s disagreed; that does not support %s%%", d, runsWord(n), p)
	}
	return UnsupportedSentence(n, pct)
}

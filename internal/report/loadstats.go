package report

import "sort"

// ComputeLoadStats derives percentiles + the error rate from one scenario's SUT-trigger samples
// (AC-11): elapsedMs is every fired request's elapsed time in file order; errored is how many of
// those requests were a failed/errored RESPONSE (ScenarioResult.ReqFailed + ReqError) — NOT the
// scenario's own verdict, exactly like ReqFailed/ReqError themselves.
//
// Percentiles are nearest-rank over the ascending-sorted samples — no interpolation, so a returned
// value is always one that was actually observed.
func ComputeLoadStats(elapsedMs []int, errored int) LoadStats {
	n := len(elapsedMs)
	st := LoadStats{Samples: n}
	if n == 0 {
		return st
	}
	sorted := append([]int(nil), elapsedMs...)
	sort.Ints(sorted)
	st.P50Ms = nearestRankPercentile(sorted, 50)
	st.P95Ms = nearestRankPercentile(sorted, 95)
	st.P99Ms = nearestRankPercentile(sorted, 99)
	st.ErrorRate = float64(errored) / float64(n)
	return st
}

// nearestRankPercentile returns the p-th nearest-rank percentile of an ASCENDING-sorted, non-empty
// slice (p in [0,100]): rank = ceil(p/100 * N), 1-based.
func nearestRankPercentile(sorted []int, p int) int {
	idx := (p*len(sorted)+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

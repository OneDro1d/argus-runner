package argus

// cmp11_f3_test.go -- ARGUS-CMP-11 fix F3: a real run is never dropped by the ordering rule p50 <= p95 <= p99. The numbers the
// executor records are report.ComputeLoadStats' (nearest-rank over one ascending-sorted list), through the real LoadRecord
// shape and the control plane's own reader.

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/compare"
	"github.com/OneDro1d/argus-runner/internal/report"
)

func TestCMP11_F3_EveryLoadStatsTheExecutorCanComputeSurvivesTheOrderingRule(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	check := func(name string, elapsed []int, errored int) {
		t.Helper()
		ls := report.ComputeLoadStats(elapsed, errored)
		row := compare.ScenarioOutput{ScenarioID: "C-1", OutputRecord: compare.LoadRecord(compare.LoadNumbers{
			Samples: ls.Samples, P50Ms: float64(ls.P50Ms), P95Ms: float64(ls.P95Ms), P99Ms: float64(ls.P99Ms), ErrorRate: ls.ErrorRate})}
		raw, err := json.Marshal([]compare.ScenarioOutput{row})
		if err != nil {
			t.Fatal(err)
		}
		kept, _, _, dropped, err := compare.SanitizeOutputs(raw)
		if err != nil || dropped != 0 || len(kept) != 1 {
			t.Errorf("%s: a real run's numbers %+v were dropped (kept %d, dropped %d, err %v)", name, ls, len(kept), dropped, err)
		}
	}
	check("no sample", nil, 0)
	check("one sample", []int{42}, 0)
	check("one failed sample", []int{42}, 1)
	check("two samples", []int{9, 3}, 1)
	check("all equal", []int{5, 5, 5, 5, 5, 5, 5}, 0)
	for n := 1; n <= 400; n++ {
		elapsed := make([]int, n)
		for i := range elapsed {
			elapsed[i] = rng.Intn(5000)
		}
		check("random list", elapsed, rng.Intn(n+1))
	}
}

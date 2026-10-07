package report

import "testing"

// NeverReachedSUT is the ONE definition of "the SUT never answered", shared by the HTTP, MCP and
// chain rollups. Its boundaries are the contract, so they are pinned here rather than in any one
// executor's tests.
//
// The bug it exists for, measured 2026-08-09: orderservice-compose had been down for 24 hours and a
// full run scored 33 failed / 0 errored, every row carrying req_error=1. "33 failed" reads as a SUT
// broken in 33 ways when it was absent.
func TestNeverReachedSUT_Boundaries(t *testing.T) {
	cases := []struct {
		name                   string
		success, failed, erred int
		want                   bool
		why                    string
	}{
		{"nothing got through", 0, 0, 1, true,
			"the exact shape of all 33 rows in run 20260809T172606532"},
		{"several requests, all transport errors", 0, 0, 5, true,
			"a multi-request scenario against an absent SUT is just as unrun"},

		// The conservatism IS the contract. Anything that got a real response means the SUT was
		// reachable, so the scenario genuinely failed and must stay failed.
		{"one real 5xx alongside an error", 0, 1, 1, false,
			"a partial outage must not be excused as infrastructure"},
		{"one success alongside an error", 1, 0, 1, false,
			"the SUT answered at least once"},
		{"all succeeded", 3, 0, 0, false, "nothing to reclassify"},
		{"all real failures", 0, 3, 0, false,
			"real negative RESPONSES are a product signal, not an infra one"},

		// No evidence of unreachability is not evidence of unreachability.
		{"no requests fired at all", 0, 0, 0, false,
			"a scenario that fired nothing proves nothing; inventing an error is the same mistake inverted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &ScenarioResult{ReqSuccess: c.success, ReqFailed: c.failed, ReqError: c.erred}
			if got := r.NeverReachedSUT(); got != c.want {
				t.Fatalf("NeverReachedSUT(ok=%d,fail=%d,err=%d) = %v, want %v — %s",
					c.success, c.failed, c.erred, got, c.want, c.why)
			}
		})
	}
}

// The reclassification must NEVER turn a red run green. Errored is a different KIND of non-green,
// not a lesser one — if this ever stopped holding, an unreachable SUT would become a passing run.
func TestErrored_IsStillNonGreen(t *testing.T) {
	r := &Report{Summary: Summary{Total: 33, Passed: 0, Failed: 0, Errored: 33}}
	if !r.Failed() {
		t.Fatal("a run whose every scenario errored reported itself as NOT failed — an absent SUT would score green")
	}
	// And the mixed case, which is what a partial outage produces.
	mixed := &Report{Summary: Summary{Total: 3, Passed: 1, Failed: 1, Errored: 1}}
	if !mixed.Failed() {
		t.Fatal("a run with one failure and one error reported itself as not failed")
	}
	green := &Report{Summary: Summary{Total: 3, Passed: 3}}
	if green.Failed() {
		t.Fatal("an all-passed run reported itself as failed")
	}
}

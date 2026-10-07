package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// VR10-R1-9 is a GLOBAL rule: the number on the Runs page must equal the number in the report. The
// page reads the LEDGER, not report.json, so the tallies push is the join — a summary field with no
// wire field is a count nobody ever sees.
func TestMapReport_CarriesTheRateLimitCounts(t *testing.T) {
	rep := &report.Report{Project: "p"}
	rep.Summary = report.Summary{Total: 10, Passed: 7, Failed: 1, Errored: 2, RateLimitedScenarios: 2, RateLimitPauses: 3}

	push := mapReport(rep, "run-1", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
	if push.Tallies.RateLimited != 2 || push.Tallies.RateLimitPauses != 3 {
		t.Fatalf("tallies = %+v; want rate_limited 2 / rate_limit_pauses 3 — the report's own numbers", push.Tallies)
	}
}

// VR10-R1-9: a run that never met a rate limit puts NOTHING on the wire, so an old ledger row and a
// new one are the same bytes.
func TestMapReport_CleanRunCarriesNoRateLimitKeys(t *testing.T) {
	rep := &report.Report{Project: "p"}
	rep.Summary = report.Summary{Total: 3, Passed: 3}

	push := mapReport(rep, "run-2", "", "full", "", "", federation.Annotations{}, time.Time{}, time.Time{}, "")
	b, err := json.Marshal(push.Tallies)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"rate_limited", "rate_limit_pauses"} {
		if strings.Contains(string(b), k) {
			t.Errorf("a clean run's tallies carry %q: %s", k, b)
		}
	}
}

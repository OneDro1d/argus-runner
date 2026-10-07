package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// TestRelayedReport_NeverCarriesAnExpectedValue: whatever shape the EXPECT text takes, no part of it
// reaches the relayed answer. Written as the holdout for msgbus tester item 5 (2026-09-28), where a
// first cut derived "failing field names" from Failure.Expected's free text by taking each
// ;-separated bullet's leading token — and every case below leaked (a semicolon inside a quoted
// value, a value-first bullet, a bare value). The report records checks only as free text, so a
// field name for the builder needs a STRUCTURED field path recorded at judge time, never a parse.
func TestRelayedReport_NeverCarriesAnExpectedValue(t *testing.T) {
	const sentinel = "LEAKSENTINEL"
	cases := []string{
		`body contains "prefix; ` + sentinel + ` tail"`,
		sentinel,
		sentinel + ` == body.status`,
		`status 200; ` + sentinel,
		`body.id == "x";` + sentinel + `_2`,
	}
	for _, exp := range cases {
		e := exp
		rep := &report.Report{Mode: "build", RunID: "r1"}
		rep.Summary.Failed, rep.Summary.Total = 1, 1
		rep.Layers = []report.Layer{{Scenarios: []report.ScenarioResult{{
			ID: "S-1", Status: "failed", Failure: &report.Failure{Expected: &e},
		}}}}
		b, _ := json.Marshal(reduceReport(rep, ""))
		if strings.Contains(string(b), sentinel) {
			t.Errorf("Expected %q leaked into the relayed report: %s", exp, b)
		}
	}
}

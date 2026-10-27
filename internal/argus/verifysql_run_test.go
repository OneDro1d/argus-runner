package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// The single-SELECT rule for `## VERIFY` SQL at the LAST door. The author path
// (scenario.Validate) already applies it; a file that reaches a run without passing the validator (a
// catalog seeded earlier, a hand-edited kit) used to hand whatever SQL it held to the JDBC sampler.

// verifyRecRunner records every Run it is handed and writes a passing .jtl row, so a test can assert
// both "nothing was fired" and "the props handed to JMeter are exactly these".
type verifyRecRunner struct {
	calls []map[string]string
}

func (r *verifyRecRunner) Run(templateBase, jtlPath string, props map[string]string, _ time.Duration) error {
	cp := map[string]string{}
	for k, v := range props {
		cp[k] = v
	}
	r.calls = append(r.calls, cp)
	id := props["scenario.id"]
	row := "timeStamp,elapsed,label,responseCode,responseMessage,threadName,success,failureMessage\n" +
		"1781024939842,42," + id + ",200,OK," + templateBase + " " + id + " 1-1,true,\n"
	return os.WriteFile(jtlPath, []byte(row), 0o644)
}

// writeVerifyScenario writes an HTTP Ingestion scenario. verify is the body of the ## VERIFY section
// ("" omits the section entirely).
func writeVerifyScenario(t *testing.T, scDir, id, verify string) {
	t.Helper()
	d := filepath.Join(scDir, "http-ingestion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: smoke", "",
		"## TRIGGER", "GET \x60http://sut.invalid/health\x60", "",
	}
	if verify != "" {
		lines = append(lines, "## VERIFY", verify, "")
	}
	lines = append(lines, "## EXPECT", "### Runnable", "- status=200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sqlBlock(q string) string { return "\x60\x60\x60sql\n" + q + "\n\x60\x60\x60" }

func runVerifyScenario(t *testing.T, verify string) (*report.ScenarioResult, *verifyRecRunner) {
	t.Helper()
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeVerifyScenario(t, scDir, "VSQL-001", verify)
	cfg := &config.Config{}
	cfg.Project.Name = "p"
	cfg.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	rr := &verifyRecRunner{}
	res, err := RunAll(cfg, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", rr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	row, _ := res.Report.Find("VSQL-001")
	if row == nil {
		t.Fatal("VSQL-001 is missing from the report")
	}
	return row, rr
}

func TestRunAll_VerifySQLRefusedBeforeFiring(t *testing.T) {
	cases := []struct {
		name, query, sentence string
	}{
		{"two statements", "SELECT id FROM orders WHERE corr = '${correlation_id}'; SELECT 1 FROM orders", "more than one statement"},
		{"not a select", "DELETE FROM orders WHERE corr = '${correlation_id}'", "must begin with `SELECT` or `WITH`"},
		{"mutating keyword in a WITH", "WITH gone AS (DELETE FROM orders WHERE corr = '${correlation_id}' RETURNING id) SELECT id FROM gone", "contains `DELETE`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, rr := runVerifyScenario(t, sqlBlock(tc.query))
			// ⛔ the load-bearing half: nothing reached the runner, so no JDBC query was issued.
			if len(rr.calls) != 0 {
				t.Fatalf("the runner was called %d time(s) for a refused VERIFY query, want 0: %v", len(rr.calls), rr.calls)
			}
			if row.Status != report.StatusError {
				t.Errorf("status = %q, want %q (it says nothing about the SUT)", row.Status, report.StatusError)
			}
			if row.Failure == nil || !strings.HasPrefix(row.Failure.Observed, VerifySQLRefusal) {
				t.Fatalf("observed does not begin with VerifySQLRefusal: %+v", row.Failure)
			}
			if !strings.Contains(row.Failure.Observed, tc.sentence) {
				t.Errorf("observed lacks the rule's sentence %q: %s", tc.sentence, row.Failure.Observed)
			}
		})
	}
}

func TestRunAll_ValidVerifySQLStillRunsUnchanged(t *testing.T) {
	q := "SELECT id FROM orders WHERE corr = '${correlation_id}'"
	row, rr := runVerifyScenario(t, sqlBlock(q))
	if row.Status != "passed" {
		t.Fatalf("a valid single SELECT did not run: status=%q failure=%+v", row.Status, row.Failure)
	}
	if len(rr.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(rr.calls))
	}
	corr := rr.calls[0]["correlation.id"]
	if corr == "" {
		t.Fatal("no correlation.id prop reached the runner")
	}
	want := strings.ReplaceAll(q, "${correlation_id}", corr)
	if got := rr.calls[0]["db.query"]; got != want {
		t.Errorf("db.query = %q, want %q (byte-identical to today)", got, want)
	}
	if strings.Contains(rr.calls[0]["db.query"], "${") {
		t.Errorf("the placeholder was not substituted: %q", rr.calls[0]["db.query"])
	}
}

// The run door enforces ONLY the read-only rules. A query that breaks a MEASURING rule (not scoped to
// the run, no FROM, a foreign placeholder) is refused where it is written, never at run time: a live
// check written before such a rule existed must keep running, and it changes nothing in the database.
func TestRunAll_MeasuringRuleViolationsStillRun(t *testing.T) {
	for name, q := range map[string]string{
		"not scoped to the run": "SELECT id FROM orders",
		"no FROM":               "SELECT 1",
		"foreign placeholder":   "SELECT id FROM orders WHERE corr = '${correlation_id}' AND t = '${tenant}'",
	} {
		t.Run(name, func(t *testing.T) {
			row, rr := runVerifyScenario(t, sqlBlock(q))
			if len(rr.calls) != 1 {
				t.Fatalf("runner calls = %d, want 1 (a measuring-rule violation is not a run-door refusal): %+v", len(rr.calls), row.Failure)
			}
			if row.Failure != nil && strings.HasPrefix(row.Failure.Observed, VerifySQLRefusal) {
				t.Fatalf("refused at the run door: %s", row.Failure.Observed)
			}
		})
	}
}

func TestRunAll_NoVerifySQLIsUnaffected(t *testing.T) {
	row, rr := runVerifyScenario(t, "")
	if row.Status != "passed" || len(rr.calls) != 1 {
		t.Fatalf("a scenario with no VERIFY changed: status=%q calls=%d failure=%+v", row.Status, len(rr.calls), row.Failure)
	}
	if _, has := rr.calls[0]["db.query"]; has {
		t.Errorf("db.query appeared on a scenario with no VERIFY: %q", rr.calls[0]["db.query"])
	}
}

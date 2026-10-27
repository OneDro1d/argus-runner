package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
)

// The other two refusals whose own comment promises "no CLEANUP" behave the same:
// the read-only ## VERIFY rule and R10 (a scenario that declares nothing). Neither
// is a money rule, so no money_handling flag is set.
func TestRefusedBeforeFiring_VerifyAndR10RunNoCleanup(t *testing.T) {
	cases := []struct{ name, tags, trigger, verify, expect string }{
		{"VERIFY is not a single read-only SELECT", "smoke", "GET `http://sut.invalid/health`",
			"```sql\nDELETE FROM orders WHERE corr = '${correlation_id}'\n```", "### Runnable\n- status=200"},
		{"R10 declares nothing", "mcp", "POST `${MCP_URL}`\n\n```json\n{\"transport\":\"streamable-http\",\"tool\":\"t\",\"args\":{}}\n```",
			"", "### Non-runnable\n- the tool answers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			d := dir + "/scenarios/http-ingestion"
			mustMkdirAll(t, d)
			lines := []string{"# Scenario: c", "", "## Metadata", "- **ID**: CLN-001", "- **Layer**: HTTP Ingestion",
				"- **Tags**: " + tc.tags, "", "## TRIGGER", tc.trigger, ""}
			if tc.verify != "" {
				lines = append(lines, "## VERIFY", tc.verify, "")
			}
			lines = append(lines, "## EXPECT", tc.expect, "", "## TIMEOUT", "30s", "",
				"## CLEANUP", "```bash\nrm -f /tmp/marker\n```", "")
			mustWriteFile(t, d+"/CLN-001.md", strings.Join(lines, "\n"))
			r := &recordingCleanupRunner{fakeRunner: fakeRunner{pass: map[string]bool{"CLN-001": true}}}
			rr, err := RunAll(httpConfig(), dir+"/scenarios", dir+"/results", "p", "", "", "", "", r)
			if err != nil {
				t.Fatalf("RunAll: %v", err)
			}
			res, _ := rr.Report.Find("CLN-001")
			if res == nil || res.Status != report.StatusError {
				t.Fatalf("precondition: refused at the run door: %+v", res)
			}
			if len(r.calls) != 0 {
				t.Fatalf("a scenario refused before firing must run NO cleanup block: %v", r.calls)
			}
			if len(res.Cleanup) != 1 || res.Cleanup[0].Outcome != report.CleanupNotRun ||
				!strings.Contains(res.Cleanup[0].Observed, "refused before firing") {
				t.Errorf("the block must be recorded as not run, with the reason: %+v", res.Cleanup)
			}
		})
	}
}

// Only a refusal before firing skips cleanup: a scenario that ran and FAILED still cleans up
// (RunCleanup is unchanged on that path).
func TestRefusedBeforeFiring_AFailedScenarioStillCleansUp(t *testing.T) {
	dir := t.TempDir()
	sc := writeCleanupScenario(t, dir, "```bash\nrm -f /tmp/marker\n```")
	r := &recordingCleanupRunner{} // CLN-001 is not in pass: the scenario fails
	rr, err := RunAll(httpConfig(), sc, dir+"/results", "p", "", "", "", "", r)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("CLN-001")
	if res == nil || res.Status == "passed" {
		t.Fatalf("precondition: the scenario must not pass: %+v", res)
	}
	if len(r.calls) != 1 || len(res.Cleanup) != 1 || res.Cleanup[0].Outcome != report.CleanupOK {
		t.Fatalf("a scenario that ran and failed still cleans up: calls=%v cleanup=%+v", r.calls, res.Cleanup)
	}
}

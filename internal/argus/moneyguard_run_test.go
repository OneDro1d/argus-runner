package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// T5.4 — the money-path guard at the LAST door: a money-handling SUT's scenario that is not a plain
// read is refused before anything is fired, even when it never went through the validator.

func writeMoneyScenario(t *testing.T, scDir, id, trigger string) {
	t.Helper()
	d := filepath.Join(scDir, "http-ingestion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: smoke", "",
		"## TRIGGER", trigger, "",
		"## EXPECT", "### Runnable", "- status=200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunAll_MoneyHandlingRefusesAWriteBeforeFiringAndRunsARead(t *testing.T) {
	for _, money := range []bool{true, false} {
		dir := t.TempDir()
		scDir := filepath.Join(dir, "scenarios")
		writeMoneyScenario(t, scDir, "MONEY-POST", "POST \x60http://sut.invalid/api/v1/strategies\x60")
		writeMoneyScenario(t, scDir, "MONEY-GET", "GET \x60http://sut.invalid/health\x60")

		cfg := &config.Config{MoneyHandling: money}
		cfg.Project.Name = "p"
		cfg.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
		fr := &fakeRunner{pass: map[string]bool{"MONEY-POST": true, "MONEY-GET": true}} // both would PASS if run
		rr, err := RunAll(cfg, scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
		if err != nil {
			t.Fatalf("RunAll: %v", err)
		}
		post, _ := rr.Report.Find("MONEY-POST")
		get, _ := rr.Report.Find("MONEY-GET")
		if post == nil || get == nil {
			t.Fatalf("a scenario is missing from the report: post=%v get=%v", post, get)
		}
		if money {
			if post.Status != report.StatusError || post.Failure == nil || !strings.HasPrefix(post.Failure.Observed, MoneyGuardRefusal) {
				t.Errorf("money_handling: the POST ran or was misreported: status=%q failure=%+v", post.Status, post.Failure)
			}
			// ⛔ the load-bearing half: nothing was fired at a system that moves money.
			if post.ReqSuccess+post.ReqFailed+post.ReqError != 0 {
				t.Errorf("money_handling: the POST fired %d requests, want 0", post.ReqSuccess+post.ReqFailed+post.ReqError)
			}
			if get.Status != "passed" {
				t.Errorf("money_handling: the GET health check did not run: status=%q failure=%+v", get.Status, get.Failure)
			}
		} else if post.Status != "passed" {
			t.Errorf("without money_handling the guard must change nothing, but the POST reported %q (%+v)", post.Status, post.Failure)
		}
	}
}

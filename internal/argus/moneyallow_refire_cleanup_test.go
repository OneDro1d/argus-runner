package argus

import (
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// A scenario whose ANY attempt fired is never "refused before firing": its cleanup runs as on any other path
// ( skipped the cleanup of a refused row, and the throttle re-fire's Reserve refusal marked a
// row refused although the first attempt had been sent and answered 429).

// refireRunner answers 429 on its first run and records every cleanup block it is handed.
type refireRunner struct {
	throttling429Runner
	cleanups []string
}

func (r *refireRunner) ExecCleanup(form scenario.CleanupForm, body string, _ map[string]string, _ time.Duration) error {
	r.cleanups = append(r.cleanups, string(form)+":"+strings.TrimSpace(body))
	return nil
}

func refireScenario(t *testing.T, dir string) string {
	t.Helper()
	d := dir + "/scenarios/http-ingestion"
	mustMkdirAll(t, d)
	md := strings.Join([]string{"# Scenario: s", "", "## Metadata", "- **ID**: SPD-001", "- **Layer**: HTTP Ingestion", "- **Tags**: http, order", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "", "```json", `{"amount": 1}`, "```", "",
		"## EXPECT", "### Runnable", "- status=202", "", "## TIMEOUT", "30s", "",
		"## CLEANUP", "```bash", "rm -f /tmp/marker", "```", ""}, "\n")
	mustWriteFile(t, d+"/SPD-001.md", md)
	return dir + "/scenarios"
}

func refireCfg(t *testing.T) *config.Config {
	t.Helper()
	c := httpRLCfg(t, true)
	c.MoneyHandling = true
	yes := true
	c.MoneyWrites = &config.MoneyWritesConfig{Allow: []config.MoneyWriteAllowEntry{
		{Method: "POST", Path: "/api/v1/orders", Spends: &yes, AmountField: "amount", MaxAmount: 100, MaxPerRun: 1},
		{Engine: "cleanup", Form: "bash"},
	}}
	return c
}

// THE REVIEWER'S PROBE: max_per_run 1, a bash cleanup entry, rate_limit declared, 429 on attempt 1.
func TestRefire_AScenarioWhoseFirstAttemptFiredStillRunsItsCleanup(t *testing.T) {
	fakeSleep(t)
	dir := t.TempDir()
	sc := refireScenario(t, dir)
	fr := &refireRunner{throttling429Runner: throttling429Runner{throttleFirst: 1, retryAfterHdr: "1"}}
	rr, err := RunAll(refireCfg(t), sc, dir+"/results", "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("SPD-001")
	if res == nil {
		t.Fatal("no row")
	}
	if fr.runs != 1 {
		t.Fatalf("precondition: attempt 1 fires, the re-fire is stopped by max_per_run: runner runs = %d", fr.runs)
	}
	if res.RefusedBeforeFiring {
		t.Errorf("a scenario whose first attempt fired is marked refused-before-firing")
	}
	if len(fr.cleanups) != 1 {
		t.Errorf("the cleanup must run once for a scenario that fired: ran %v", fr.cleanups)
	}
	if len(res.Cleanup) != 1 || res.Cleanup[0].Outcome != report.CleanupOK {
		t.Errorf("the cleanup block must be recorded as run: %+v", res.Cleanup)
	}
}

// The control: a scenario refused on its FIRST attempt (nothing fired) still skips its cleanup.
func TestRefire_AScenarioRefusedOnItsFirstAttemptStillSkipsItsCleanup(t *testing.T) {
	fakeSleep(t)
	dir := t.TempDir()
	sc := refireScenario(t, dir)
	c := refireCfg(t)
	c.MoneyWrites.Allow[0].MaxAmount = 0.5 // the literal amount 1 is over the limit: refused before firing
	fr := &refireRunner{throttling429Runner: throttling429Runner{throttleFirst: 1, retryAfterHdr: "1"}}
	rr, err := RunAll(c, sc, dir+"/results", "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("SPD-001")
	if res == nil || !res.RefusedBeforeFiring || fr.runs != 0 || len(fr.cleanups) != 0 {
		t.Errorf("control: want refused, nothing fired, no cleanup: %+v runs=%d cleanups=%v", res, fr.runs, fr.cleanups)
	}
}

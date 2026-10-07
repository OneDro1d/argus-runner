package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// moneywrites_run_test.go — "money writes" (T5.4 follow-up, 2026-09-26), item 3, at the PLAIN-TRIGGER
// execution door (runOneScenario, argus.go): the door that sees props["trigger.payload"] — resolved
// exactly as the request itself will be. moneyguard_run_test.go proves the structural (GET-only)
// door end to end through RunAll; this file proves the spend check the same way.

// writeMoneyWriteScenario is writeMoneyScenario (moneyguard_run_test.go) plus a literal JSON body, so
// the plain-TRIGGER door has a resolved amount to check.
func writeMoneyWriteScenario(t *testing.T, scDir, id, method, url, body string) {
	t.Helper()
	d := filepath.Join(scDir, "http-ingestion")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := strings.Join([]string{
		"# Scenario: " + id, "",
		"## Metadata", "- **ID**: " + id, "- **Layer**: HTTP Ingestion", "- **Tags**: smoke", "",
		"## TRIGGER", method + " `" + url + "`", "```json", body, "```", "",
		"## EXPECT", "### Runnable", "- status=200", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	if err := os.WriteFile(filepath.Join(d, id+".md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func moneyWritesTestConfig() *config.Config {
	yes := true
	no := false
	cfg := &config.Config{MoneyHandling: true, MoneyWrites: &config.MoneyWritesConfig{
		Allow: []config.MoneyWriteAllowEntry{
			{Method: "POST", Path: "/api/v1/trading/quote", Spends: &no},
			{Method: "POST", Path: "/api/v1/trading/order", Spends: &yes,
				AmountField: "source_amount", MaxAmount: 25, MaxPerRun: 2},
		},
	}}
	cfg.Project.Name = "p"
	cfg.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	return cfg
}

func TestRunAll_MoneyWrites_UnderCapIsRunAndPasses(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMoneyWriteScenario(t, scDir, "MW-OK", "POST", "http://sut.invalid/api/v1/trading/order", `{"source_amount": 10}`)

	fr := &fakeRunner{pass: map[string]bool{"MW-OK": true}}
	rr, err := RunAll(moneyWritesTestConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("MW-OK")
	if res == nil || res.Status != "passed" {
		t.Fatalf("an allowlisted under-cap spend did not run: %+v", res)
	}
}

func TestRunAll_MoneyWrites_OverMaxAmountNotSent(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMoneyWriteScenario(t, scDir, "MW-OVER", "POST", "http://sut.invalid/api/v1/trading/order", `{"source_amount": 999}`)

	fr := &fakeRunner{pass: map[string]bool{"MW-OVER": true}} // would PASS if actually fired
	rr, err := RunAll(moneyWritesTestConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("MW-OVER")
	if res == nil || res.Status != report.StatusError {
		t.Fatalf("an over-max_amount spend was not refused: %+v", res)
	}
	if res.ReqSuccess+res.ReqFailed+res.ReqError != 0 {
		t.Errorf("an over-max_amount spend fired %d requests, want 0", res.ReqSuccess+res.ReqFailed+res.ReqError)
	}
	if !strings.Contains(res.Failure.Observed, "999") {
		t.Errorf("observed does not name the value: %q", res.Failure.Observed)
	}
}

func TestRunAll_MoneyWrites_MissingAmountFieldNotSent(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMoneyWriteScenario(t, scDir, "MW-MISSING", "POST", "http://sut.invalid/api/v1/trading/order", `{"other": 1}`)

	fr := &fakeRunner{pass: map[string]bool{"MW-MISSING": true}}
	rr, err := RunAll(moneyWritesTestConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("MW-MISSING")
	if res == nil || res.Status != report.StatusError {
		t.Fatalf("a missing amount_field was not refused (fail-closed): %+v", res)
	}
}

// The scenario in item 3 itself: max_per_run enforced ACROSS TWO SCENARIOS in one run.
func TestRunAll_MoneyWrites_MaxPerRunEnforcedAcrossTwoScenarios(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	// max_per_run is 2 (moneyWritesTestConfig). Three scenarios each spend once.
	writeMoneyWriteScenario(t, scDir, "MW-S1", "POST", "http://sut.invalid/api/v1/trading/order", `{"source_amount": 5}`)
	writeMoneyWriteScenario(t, scDir, "MW-S2", "POST", "http://sut.invalid/api/v1/trading/order", `{"source_amount": 5}`)
	writeMoneyWriteScenario(t, scDir, "MW-S3", "POST", "http://sut.invalid/api/v1/trading/order", `{"source_amount": 5}`)

	fr := &fakeRunner{pass: map[string]bool{"MW-S1": true, "MW-S2": true, "MW-S3": true}}
	rr, err := RunAll(moneyWritesTestConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	var passed, refused int
	for _, id := range []string{"MW-S1", "MW-S2", "MW-S3"} {
		res, _ := rr.Report.Find(id)
		if res == nil {
			t.Fatalf("%s missing from report", id)
		}
		switch res.Status {
		case "passed":
			passed++
		case report.StatusError:
			refused++
		default:
			t.Errorf("%s: unexpected status %q", id, res.Status)
		}
	}
	if passed != 2 || refused != 1 {
		t.Fatalf("passed=%d refused=%d, want 2 passed (the run's max_per_run budget) and 1 refused", passed, refused)
	}
}

// A write NOT in the allowlist stays refused exactly as it was before money_writes (defence in
// depth alongside the config-level test).
func TestRunAll_MoneyWrites_UnlistedWriteStillRefused(t *testing.T) {
	dir := t.TempDir()
	scDir := filepath.Join(dir, "scenarios")
	writeMoneyWriteScenario(t, scDir, "MW-UNLISTED", "POST", "http://sut.invalid/api/v1/trading/rebalance", `{}`)

	fr := &fakeRunner{pass: map[string]bool{"MW-UNLISTED": true}}
	rr, err := RunAll(moneyWritesTestConfig(), scDir, filepath.Join(dir, "results"), "p", "", "", "", "", fr)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	res, _ := rr.Report.Find("MW-UNLISTED")
	if res == nil || res.Status != report.StatusError || !strings.HasPrefix(res.Failure.Observed, MoneyGuardRefusal) {
		t.Fatalf("an unlisted write was not refused by the structural guard: %+v", res)
	}
}

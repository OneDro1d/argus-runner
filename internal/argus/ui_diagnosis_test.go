package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// #139 — "N of M declared ui assertions were not evaluated" must not replace the cause. When
// page.goto fails (argus-declared.spec.ts: the goto runs before the per-check try), the spec writes
// no outcome at all, and Playwright's real error is on stdout (discarded) and in its JSON report
// argus-out/results.json. The row stays `error` — the SUT was not measured — and now says why.
// (Found by Aleksander on memstore-dev WEBUI-001, 2026-09-18: a wrong app_url port read as
// "not measured" with no reason.)
func TestUIScenario_NotEvaluatedKeepsTheCause(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()

	// the JSON reporter's shape: suites → specs → tests → results → errors, messages ANSI-coloured
	const pwReport = `{"suites":[{"title":"argus-declared.spec.ts","specs":[],"suites":[{"title":"declared","specs":[{"title":"declared checks","tests":[{"results":[{"status":"failed","errors":[{"message":"\u001b[31mError: page.goto: net::ERR_CONNECTION_REFUSED at http://memstore-gateway:8090/\u001b[39m\nCall log:\n  - navigating to \"http://memstore-gateway:8090/\""}]}]}]}]}]}],"errors":[]}`

	writeReport := func(t *testing.T, vendorDir, body string) {
		t.Helper()
		dir := filepath.Join(vendorDir, "argus-out")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "results.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the goto failed: the Playwright error is in the row", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			writeReport(t, vendorDir, pwReport) // no assertions.json: the spec threw before writing it
			return 1, true, ""
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .cl-rootBox\n- no console errors"), "tr-139-1", vendor)
		if res.Status != report.StatusError {
			t.Fatalf("status = %q, want %q — nothing was evaluated", res.Status, report.StatusError)
		}
		obs := res.Failure.Observed
		if !strings.Contains(obs, "2 of 2 declared ui assertions were not evaluated") {
			t.Errorf("the verdict line must stay: %q", obs)
		}
		if !strings.Contains(obs, "page.goto: net::ERR_CONNECTION_REFUSED at http://memstore-gateway:8090/") {
			t.Errorf("the cause Playwright recorded must be in the row: %q", obs)
		}
		if strings.Contains(obs, "\u001b[") || strings.Contains(obs, "Call log") {
			t.Errorf("one clean line, no colour codes or call log: %q", obs)
		}
	})

	t.Run("no JSON report: the first stderr line is the cause", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			return 1, true, "Error: Cannot find module '@playwright/test'\n    at Module._resolveFilename"
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .banner"), "tr-139-2", vendor)
		if obs := res.Failure.Observed; !strings.Contains(obs, "Cannot find module '@playwright/test'") || strings.Contains(obs, "_resolveFilename") {
			t.Errorf("want the first stderr line only: %q", obs)
		}
	})

	t.Run("nothing recorded: Classify's own observation is kept", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			return 1, true, ""
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .banner"), "tr-139-3", vendor)
		if obs := res.Failure.Observed; !strings.Contains(obs, "UI spec failed") {
			t.Errorf("Classify's observation must not be dropped: %q", obs)
		}
		// ⛔ AND IT MUST NOT BE SIGNED "Playwright". Playwright wrote nothing on this path — no JSON
		// report, no stderr — so the text above is Argus's own Classify() output. Quoting it under
		// Playwright's name sent readers looking for a log line that does not exist, and made the row
		// assert a DOM/backend failure one clause after stating nothing was measured. The observation
		// is kept (assertion above); only the false attribution is gone.
		if obs := res.Failure.Observed; strings.Contains(obs, "Playwright:") {
			t.Errorf("no Playwright error was recorded, so none may be quoted: %q", obs)
		}
	})

	t.Run("a partial outcomes file on a passing spec: nothing to add, no invented cause", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			writeOutcomes(t, vendorDir, []uiOutcomeEntry{{Bullet: "dom has .banner", OK: true}})
			return 0, true, ""
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .banner\n- no console errors"), "tr-139-4", vendor)
		if res.Status != report.StatusError {
			t.Fatalf("status = %q, want %q", res.Status, report.StatusError)
		}
		if obs := res.Failure.Observed; strings.Contains(obs, "Playwright:") {
			t.Errorf("no Playwright error was recorded, so none may be quoted: %q", obs)
		}
	})
}

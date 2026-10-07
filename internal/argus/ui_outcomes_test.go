package argus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// V29-021 (VR13-UI) — THE RUNNER READS BACK WHAT THE SPEC EVALUATED.
//
// The grammar lets an author DECLARE a check; this is what makes the product prove it ran one. The
// spec writes `argus-out/assertions.json` — one `{bullet, ok, observed}` per declared bullet —
// and the runner reads it back, records it, and REFUSES to call a run green when an entry is
// missing.

func uiScenarioMD(t *testing.T, runnable string) *scenario.Scenario {
	t.Helper()
	return scenario.Parse(strings.Join([]string{
		"# Scenario: u", "",
		"## Metadata", "- **ID**: WEBUI-001", "- **Layer**: Web UI", "- **Tags**: ui", "",
		"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
		"## EXPECT", "### Runnable", runnable, "",
		"### Non-runnable", "- the flow is described in the spec file", "",
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n"))
}

// writeOutcomes puts an assertions.json where the spec would have written one.
func writeOutcomes(t *testing.T, vendorDir string, entries []uiOutcomeEntry) {
	t.Helper()
	dir := filepath.Join(vendorDir, "argus-out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(dir, "assertions.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunUIScenario_ReadsThePerBulletOutcomes(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()

	t.Run("every declared bullet evaluated and ok -> passed, and it says how many", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			// the runner hands the spec the parsed asserts; the spec answers one entry per bullet
			if len(asserts) != 2 {
				t.Errorf("the runner must pass the declared asserts to the spec, got %d", len(asserts))
			}
			writeOutcomes(t, vendorDir, []uiOutcomeEntry{
				{Bullet: "dom has .banner", OK: true, Observed: ".banner is present"},
				{Bullet: "no console errors", OK: true, Observed: "0 console errors"},
			})
			return 0, true, ""
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .banner\n- no console errors"), "tr-ui-1", vendor)
		if res.Status != "passed" {
			t.Fatalf("status = %q (%+v), want passed", res.Status, res.Failure)
		}
		if res.AssertionsEnforcedCount != 2 {
			t.Errorf("AssertionsEnforcedCount = %d, want 2 — both hats keep the count", res.AssertionsEnforcedCount)
		}
		if len(res.AssertionsEnforced) != 2 {
			t.Errorf("AssertionsEnforced = %v, want both bullets (test hat)", res.AssertionsEnforced)
		}
	})

	t.Run("a bullet that FAILED -> failed, with a reality-only observed", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			writeOutcomes(t, vendorDir, []uiOutcomeEntry{
				{Bullet: "dom has .total containing 42.00", OK: false, Observed: ".total text is 7.00"},
			})
			return 1, true, ""
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .total containing 42.00"), "tr-ui-2", vendor)
		if res.Status != "failed" {
			t.Fatalf("status = %q, want failed — the SUT answered and the answer was wrong", res.Status)
		}
		if res.Failure == nil || !strings.Contains(res.Failure.Observed, "7.00") {
			t.Errorf("observed must name what was actually there: %+v", res.Failure)
		}
		// VR-C8: observed reaches the product hat, so it must never echo the asserted value
		if res.Failure != nil && strings.Contains(res.Failure.Observed, "42.00") {
			t.Errorf("observed leaked the asserted value: %q", res.Failure.Observed)
		}
		// …and it is still counted as enforced: it WAS evaluated
		if res.AssertionsEnforcedCount != 1 {
			t.Errorf("AssertionsEnforcedCount = %d, want 1", res.AssertionsEnforcedCount)
		}
	})

	// ⛔ U-C — A MISSING ENTRY IS AN EXECUTION ERROR, NEVER A PASS AND NEVER A SUT FAILURE.
	t.Run("a PARTIAL outcomes file is an execution error", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			writeOutcomes(t, vendorDir, []uiOutcomeEntry{{Bullet: "dom has .banner", OK: true}})
			return 0, true, ""
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .banner\n- no console errors"), "tr-ui-3", vendor)
		if res.Status != report.StatusError {
			t.Fatalf("status = %q, want %q — one bullet was never evaluated, so the SUT was not measured",
				res.Status, report.StatusError)
		}
		if res.Failure == nil || !strings.Contains(res.Failure.Observed, "not evaluated") {
			t.Errorf("observed must say which were not evaluated: %+v", res.Failure)
		}
	})

	t.Run("a MISSING outcomes file is an execution error", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			return 0, true, "" // writes nothing
		}
		res := runUIScenario(uiScenarioMD(t, "- dom has .banner"), "tr-ui-4", vendor)
		if res.Status != report.StatusError {
			t.Fatalf("status = %q, want %q", res.Status, report.StatusError)
		}
	})
}

// ⛔ THE SPEC AND THE RUNNER MUST NAME THE SAME FILE, WHATEVER THE VENDOR DIR LOOKS LIKE.
//
// Production passes ui.VendorDir = "testkit/ui" — RELATIVE — and uiRun starts Playwright with
// cmd.Dir = vendorDir, so a relative ARGUS_UI_OUT is resolved a second time from inside the kit:
// the spec wrote testkit/ui/testkit/ui/argus-out/assertions.json while the runner read
// testkit/ui/argus-out/assertions.json, and every declared ui run came back "3 of 3 declared ui
// assertions were not evaluated" (measured: memstore-dev WEBUI-001, run 20260918T150128060). The other
// tests here use an absolute t.TempDir() and a fake that ignores outPath, which is why it slipped.
// This fake does what the real spec does: it writes to ARGUS_UI_OUT, resolved from its own cwd.
func TestRunUIScenario_ARelativeVendorDirStillReadsWhatTheSpecWrote(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()

	t.Chdir(t.TempDir())
	const vendor = "testkit/ui" // the production shape: relative, like ui.VendorDir
	if err := os.MkdirAll(vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		p := outPath
		if !filepath.IsAbs(p) {
			p = filepath.Join(vendorDir, p) // Playwright's cwd is cmd.Dir = vendorDir
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal([]uiOutcomeEntry{{Bullet: "dom has .banner", OK: true, Observed: ".banner is present"}})
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return 0, true, ""
	}
	res := runUIScenario(uiScenarioMD(t, "- dom has .banner"), "tr-ui-rel", vendor)
	if res.Status != "passed" {
		t.Fatalf("status = %q (%+v), want passed — the runner read a different file from the one the spec wrote",
			res.Status, res.Failure)
	}
}

// ⛔⛔ U-B — A STALE OUTCOMES FILE IS NEVER READ AS THIS RUN'S EVIDENCE.
//
// This is the worst thing this row could ship: an all-green assertions.json left by a PREVIOUS run,
// read as proof of a run that wrote nothing. `uiRun` already clears `test-results/` for exactly this
// reason ("Playwright APPENDS per-test dirs; start each run fresh") and `argus-out/` needs the
// same line — the evidence directory is no less deserving of it than the artifact one.
func TestRunUIScenario_AStaleOutcomesFileIsNotReadAsThisRun(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()

	vendor := t.TempDir()
	// a previous run's evidence: everything green
	writeOutcomes(t, vendor, []uiOutcomeEntry{
		{Bullet: "dom has .banner", OK: true, Observed: "from a run that is over"},
	})
	uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		return 0, true, "" // this run writes NOTHING
	}
	res := runUIScenario(uiScenarioMD(t, "- dom has .banner"), "tr-ui-stale", vendor)
	if res.Status == "passed" {
		t.Fatalf("a PREVIOUS run's all-green outcomes file was read as this run's evidence: %+v", res)
	}
	if res.Status != report.StatusError {
		t.Errorf("status = %q, want %q — nothing was measured", res.Status, report.StatusError)
	}
}

// U-H — nothing this row writes lands in `test-results/`, because `hasTestArtifacts` reads that
// directory to tell a SUT failure from a harness failure (VR-L3 / UC-62). Putting our evidence there
// would make an execution failure look like a SUT one.
func TestRunUIScenario_NothingThisRowWritesLandsInTestResults(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()

	vendor := t.TempDir()
	uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		writeOutcomes(t, vendorDir, []uiOutcomeEntry{{Bullet: "dom has .banner", OK: true}})
		// the harness died: only the metadata marker, no per-test artifact
		rd := filepath.Join(vendorDir, "test-results")
		_ = os.MkdirAll(rd, 0o755)
		_ = os.WriteFile(filepath.Join(rd, ".last-run.json"), []byte(`{}`), 0o644)
		return 1, hasTestArtifacts(rd), ""
	}
	res := runUIScenario(uiScenarioMD(t, "- dom has .banner"), "tr-ui-5", vendor)
	if res.Status != report.StatusError {
		t.Fatalf("status = %q, want %q — no per-test artifact means the harness died, not the SUT", res.Status, report.StatusError)
	}
	// and the outcomes file is NOT under test-results/
	if _, err := os.Stat(filepath.Join(vendor, "test-results", "assertions.json")); err == nil {
		t.Error("the outcomes file must never live under test-results/ — it would be read as a SUT artifact")
	}
}

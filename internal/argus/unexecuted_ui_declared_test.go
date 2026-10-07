package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// AC-D27 — a Web UI scenario's DECLARED checks are evaluated by testkit/ui/tests/live/argus-declared.spec.ts
// (V29-021), which writes one {bullet, ok, observed} per check to argus-out/assertions.json, and the row
// records each one in assertions_enforced. The report still listed every one of them as "not executed"
// (measured: one run, WEBUI-001-AKS passed, assertions_enforced_count 5, and
// `unexecuted` named all 5). A real pass reported as hollow — the AC-D23 / ARG-SYN-005 class.
//
// The rule now: a declared check is unexecuted only when the spec wrote no outcome for it. A bullet
// that is not a Web UI check at all is still listed, with the reason.
func TestAC_D27_DeclaredUIChecksAreNotReportedUnexecutedOnceEvaluated(t *testing.T) {
	orig := uiRun
	defer func() { uiRun = orig }()
	const bullets = "- dom has .cl-rootBox\n- no console errors\n- no backend 4xx/5xx"

	t.Run("every declared check has an outcome: nothing is unexecuted", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			var e []uiOutcomeEntry
			for _, a := range asserts {
				e = append(e, uiOutcomeEntry{Bullet: a.Bullet, OK: true, Observed: "ok"})
			}
			writeOutcomes(t, vendorDir, e)
			return 0, true, ""
		}
		s := uiScenarioMD(t, bullets)
		res := runUIScenario(s, "tr-acd27-1", vendor)
		if res.Status != "passed" || res.AssertionsEnforcedCount != 3 {
			t.Fatalf("fixture: want a pass with 3 enforced, got %q / %d", res.Status, res.AssertionsEnforcedCount)
		}
		if got := UnexecutedAfterRun(s, res); len(got) != 0 {
			t.Fatalf("all 3 declared checks were evaluated, yet reported unexecuted: %+v", got)
		}
	})

	t.Run("a declared check the spec wrote no outcome for is still listed", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			writeOutcomes(t, vendorDir, []uiOutcomeEntry{{Bullet: "dom has .cl-rootBox", OK: true}, {Bullet: "no console errors", OK: true}})
			return 0, true, ""
		}
		s := uiScenarioMD(t, bullets)
		res := runUIScenario(s, "tr-acd27-2", vendor)
		got := UnexecutedAfterRun(s, res)
		if len(got) != 1 || !strings.Contains(got[0].Bullet, "no backend 4xx/5xx") {
			t.Fatalf("want exactly the check with no outcome, got %+v", got)
		}
		if !strings.Contains(got[0].Reason, "no outcome") {
			t.Errorf("the reason must say the spec wrote no outcome for it: %q", got[0].Reason)
		}
	})

	t.Run("a failed check was evaluated, so it is not unexecuted either", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			var e []uiOutcomeEntry
			for _, a := range asserts {
				e = append(e, uiOutcomeEntry{Bullet: a.Bullet, OK: a.Kind != scenario.UIKindDOM, Observed: "0 element(s) match"})
			}
			writeOutcomes(t, vendorDir, e)
			return 1, true, ""
		}
		s := uiScenarioMD(t, bullets)
		res := runUIScenario(s, "tr-acd27-3", vendor)
		if res.Status != "failed" {
			t.Fatalf("fixture: want failed, got %q", res.Status)
		}
		if got := UnexecutedAfterRun(s, res); len(got) != 0 {
			t.Fatalf("a failed check was evaluated; it must not be listed as unexecuted: %+v", got)
		}
	})

	t.Run("the harness never ran: every declared check is listed", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			return 1, false, "browserType.launch: Executable doesn't exist"
		}
		s := uiScenarioMD(t, bullets)
		res := runUIScenario(s, "tr-acd27-4", vendor)
		if res.Status != report.StatusError {
			t.Fatalf("fixture: want error, got %q", res.Status)
		}
		if got := UnexecutedAfterRun(s, res); len(got) != 3 {
			t.Fatalf("nothing was evaluated, so all 3 must be listed: %+v", got)
		}
	})

	t.Run("a bullet that is not a Web UI check is still listed", func(t *testing.T) {
		vendor := t.TempDir()
		uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
			var e []uiOutcomeEntry
			for _, a := range asserts {
				e = append(e, uiOutcomeEntry{Bullet: a.Bullet, OK: true})
			}
			writeOutcomes(t, vendorDir, e)
			return 0, true, ""
		}
		s := uiScenarioMD(t, "- dom has .cl-rootBox\n- status=200")
		res := runUIScenario(s, "tr-acd27-5", vendor)
		got := UnexecutedAfterRun(s, res)
		if len(got) != 1 || !strings.Contains(got[0].Bullet, "status=200") {
			t.Fatalf("want only the non-ui bullet, got %+v", got)
		}
	})
}

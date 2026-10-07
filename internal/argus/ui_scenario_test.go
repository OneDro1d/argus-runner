package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// runUIScenario maps the OS-process outcome to a verdict (VR-L3 / UC-62/84): exit 0 ->
// passed; exit!=0 WITH a results artifact -> SUT "failed"; exit!=0 WITHOUT artifact ->
// a DISTINCT execution error (never silent-green). Drives the injectable uiRun.
func TestRunUIScenario_Verdicts(t *testing.T) {
	orig := uiRun
	t.Cleanup(func() { uiRun = orig })

	s := &scenario.Scenario{ID: "UI-X", Tags: []string{"ui"}, Expect: []string{"the dashboard renders", "no backend 4xx/5xx"}}
	s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://localhost:5190"}`

	cases := []struct {
		name       string
		exit       int
		hasResults bool
		stderr     string
		wantStatus string
		wantObs    string // substring the reality-only observed must contain
	}{
		{"green", 0, true, "", "passed", ""},
		{"sut-failed", 1, true, "", "failed", "DOM assertion or a backend"},
		{"exec-error-no-artifact", 1, false, "Error: browser launch failed\n at x", report.StatusError, "EXECUTION failure"},
		{"exec-error-negexit", -1, false, "panic", report.StatusError, "EXECUTION failure"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
				if len(specs) != 1 || specs[0] != "tests/live/x.spec.ts" {
					t.Fatalf("spec not threaded: %q", specs)
				}
				if appURL != "http://localhost:5190" {
					t.Fatalf("app_url not threaded: %q", appURL)
				}
				return c.exit, c.hasResults, c.stderr
			}
			res := runUIScenario(s, "tr-abc", "testkit/ui")
			if res.Status != c.wantStatus {
				t.Fatalf("status = %q; want %q", res.Status, c.wantStatus)
			}
			if c.wantStatus == "passed" {
				if res.Failure != nil {
					t.Fatalf("passed scenario must have no failure, got %+v", res.Failure)
				}
				return
			}
			if res.Failure == nil {
				t.Fatalf("non-passing scenario must carry a failure")
			}
			if !strings.Contains(res.Failure.Observed, c.wantObs) {
				t.Fatalf("observed %q must contain %q", res.Failure.Observed, c.wantObs)
			}
			// reality-only (VR-C8): observed must NOT restate the EXPECT text.
			if strings.Contains(res.Failure.Observed, "the dashboard renders") {
				t.Fatalf("observed leaked the EXPECT: %q", res.Failure.Observed)
			}
		})
	}
}

// Test-requests panel: one request per spec run — a passed spec is ReqSuccess=1, a
// SUT-failed OR execution-errored spec is ReqError=1 (argus_sut_requests_total).
func TestRunUIScenario_RequestCounts(t *testing.T) {
	orig := uiRun
	t.Cleanup(func() { uiRun = orig })
	s := &scenario.Scenario{ID: "UI-RC", Tags: []string{"ui"}}
	s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://localhost:5190"}`

	uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		return 0, true, ""
	} // passed
	if r := runUIScenario(s, "tr-1", "testkit/ui"); r.ReqSuccess != 1 || r.ReqFailed != 0 || r.ReqError != 0 || len(r.Requests) != 1 {
		t.Errorf("passed spec → success=1 (+1 sample), got success=%d failed=%d error=%d samples=%d", r.ReqSuccess, r.ReqFailed, r.ReqError, len(r.Requests))
	}
	uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		return 1, true, ""
	} // SUT-failed (artifact) → a negative response
	if r := runUIScenario(s, "tr-2", "testkit/ui"); r.ReqFailed != 1 || r.ReqSuccess != 0 || r.ReqError != 0 {
		t.Errorf("SUT-failed spec → ReqFailed=1 (negative response, NOT error), got success=%d failed=%d error=%d", r.ReqSuccess, r.ReqFailed, r.ReqError)
	}
	uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		return 1, false, "boom"
	} // exec-error (no artifact) → no response
	if r := runUIScenario(s, "tr-3", "testkit/ui"); r.ReqError != 1 || r.ReqSuccess != 0 || r.ReqFailed != 0 {
		t.Errorf("exec-errored spec → ReqError=1, got success=%d failed=%d error=%d", r.ReqSuccess, r.ReqFailed, r.ReqError)
	}
}

func TestRunUIScenario_PreflightMissingSpec(t *testing.T) {
	s := &scenario.Scenario{ID: "UI-Y", Tags: []string{"ui"}}
	res := runUIScenario(s, "tr-abc", "testkit/ui")
	if res.Status != "failed" || res.Failure == nil || !strings.Contains(res.Failure.Observed, "preflight") {
		t.Fatalf("missing spec must fail at preflight, got status=%q failure=%+v", res.Status, res.Failure)
	}
}

// hasTestArtifacts must treat a test-results dir holding ONLY .last-run.json as having
// NO real artifact (a "no tests found" / harness failure → execution error, UC-62), but
// a per-test subdir as a real artifact (a SUT failure).
func TestHasTestArtifacts(t *testing.T) {
	dir := t.TempDir()
	if hasTestArtifacts(dir) {
		t.Fatal("empty dir must have no artifacts")
	}
	if err := os.WriteFile(filepath.Join(dir, ".last-run.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasTestArtifacts(dir) {
		t.Fatal(".last-run.json alone must NOT count as a real artifact (harness failure)")
	}
	if err := os.MkdirAll(filepath.Join(dir, "UI-X-chromium"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasTestArtifacts(dir) {
		t.Fatal("a per-test subdir IS a real artifact (SUT failure)")
	}
}

// runUICaptured runs runUIScenario with a uiRun that records what Playwright would have been
// launched with. called=false means the run stopped at preflight.
func runUICaptured(t *testing.T, s *scenario.Scenario, corr string) (res report.ScenarioResult, specs []string, appURL string, called bool) {
	t.Helper()
	orig := uiRun
	t.Cleanup(func() { uiRun = orig })
	uiRun = func(vendorDir string, sp []string, u, c string, asserts []scenario.UIAssert, outPath string) (int, bool, string) {
		specs, appURL, called = sp, u, true
		return 0, true, ""
	}
	res = runUIScenario(s, corr, t.TempDir())
	return
}

// parseUISpec reads the payload AS WRITTEN (#160, #161): resolving is runUIScenario's job, once.
func TestParseUISpec_ReadsThePayloadAsWritten(t *testing.T) {
	t.Setenv("APP_URL", "http://localhost:5190")
	got, err := parseUISpec(`{"spec":"tests/live/${cid}.spec.ts","app_url":"${APP_URL}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec != "tests/live/${cid}.spec.ts" || got.AppURL != "${APP_URL}" {
		t.Fatalf("parseUISpec must not resolve: %+v", got)
	}
	for _, empty := range []string{"", "  \n"} {
		if got, err := parseUISpec(empty); err != nil || got != (uiSpec{}) {
			t.Fatalf("an empty payload is no payload: %+v, %v", got, err)
		}
	}
	if _, err := parseUISpec(`{"spec":"tests/live/x.spec.ts",`); err == nil {
		t.Fatal("invalid JSON must be an error, not an empty spec")
	}
}

// ${cid} and ${VAR} still resolve — in the payload fields and in the TRIGGER url fallback.
func TestRunUIScenario_ResolvesCidAndVars(t *testing.T) {
	t.Setenv("APP_URL", "http://ambient:1")
	t.Setenv("ARGUS_T160_APP", "http://localhost:5190")

	s := &scenario.Scenario{ID: "UI-VARS", Tags: []string{"ui"}}
	s.Trigger.Payload = `{"spec":"tests/live/${cid}.spec.ts","app_url":"${ARGUS_T160_APP}"}`
	_, specs, appURL, called := runUICaptured(t, s, "tr-99")
	if !called || len(specs) != 1 || specs[0] != "tests/live/tr-99.spec.ts" || appURL != "http://localhost:5190" {
		t.Fatalf("called=%v specs=%q appURL=%q", called, specs, appURL)
	}

	s = &scenario.Scenario{ID: "UI-URL", Tags: []string{"ui"}}
	s.Trigger.URL = "`tests/live/${cid}.spec.ts`"
	_, specs, appURL, called = runUICaptured(t, s, "tr-99")
	if !called || len(specs) != 1 || specs[0] != "tests/live/tr-99.spec.ts" || appURL != "http://ambient:1" {
		t.Fatalf("TRIGGER url fallback: called=%v specs=%q appURL=%q", called, specs, appURL)
	}
}

// #160: a `"` or `\` in a ${VAR} value reaches Playwright verbatim. It used to be put inside the
// JSON text: a quote made the payload invalid and the run silently used the ambient APP_URL (or
// stopped with "missing spec"), and a backslash was read as a JSON escape.
func TestRunUIScenario_QuoteOrBackslashInVarValue(t *testing.T) {
	t.Setenv("APP_URL", "http://ambient:1")
	for _, v := range []string{`a"b`, `a\b`, `p"w\"d\n`} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("ARGUS_T160_PW", v)
			s := &scenario.Scenario{ID: "UI-160", Tags: []string{"ui"}}
			s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://u:${ARGUS_T160_PW}@h:8090/app"}`
			res, specs, appURL, called := runUICaptured(t, s, "tr-160")
			if !called {
				t.Fatalf("the run stopped at preflight: %+v", res.Failure)
			}
			if len(specs) != 1 || specs[0] != "tests/live/x.spec.ts" {
				t.Errorf("spec = %q", specs)
			}
			if want := "http://u:" + v + "@h:8090/app"; appURL != want {
				t.Errorf("app_url = %q, want %q", appURL, want)
			}
		})
	}
}

// #161: each field is resolved once. A value that itself holds ${...} is passed on as that text,
// not expanded a second time.
func TestRunUIScenario_ResolvesEachFieldOnce(t *testing.T) {
	t.Setenv("ARGUS_T161_NEST", "${ARGUS_T161_INNER}")
	t.Setenv("ARGUS_T161_INNER", "expanded-twice")
	s := &scenario.Scenario{ID: "UI-161", Tags: []string{"ui"}}
	s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://h/${ARGUS_T161_NEST}"}`
	_, _, appURL, called := runUICaptured(t, s, "tr-161")
	if !called || appURL != "http://h/${ARGUS_T161_INNER}" {
		t.Fatalf("called=%v app_url=%q, want http://h/${ARGUS_T161_INNER}", called, appURL)
	}
	// In `spec` the text left after one pass still reads as unresolved, so the run stops at preflight
	// rather than launching a spec path the file does not state.
	s.Trigger.Payload = `{"spec":"tests/live/${ARGUS_T161_NEST}.spec.ts"}`
	res, _, _, called := runUICaptured(t, s, "tr-161")
	if called || res.Failure == nil || !strings.Contains(res.Failure.Observed, "unresolved spec path") {
		t.Fatalf("called=%v failure=%+v", called, res.Failure)
	}
}

// #160: a payload that is not JSON stops the run at preflight and says so. It used to be dropped,
// and the run went on with the TRIGGER url's spec and the ambient APP_URL.
func TestRunUIScenario_InvalidPayloadIsAPreflightFailure(t *testing.T) {
	t.Setenv("ARGUS_T160_PW", "s3cret-value")
	s := &scenario.Scenario{ID: "UI-BAD", Tags: []string{"ui"}}
	s.Trigger.URL = "`tests/live/other.spec.ts`"
	s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://u:${ARGUS_T160_PW}@h"`
	res, _, _, called := runUICaptured(t, s, "tr-bad")
	if called {
		t.Fatal("Playwright must not run on a payload that does not parse")
	}
	if res.Status != "failed" || res.Failure == nil {
		t.Fatalf("want a failed preflight, got %+v", res)
	}
	obs := res.Failure.Observed
	if !strings.Contains(obs, "not valid JSON") || !strings.HasSuffix(obs, "— preflight") {
		t.Errorf("Observed = %q", obs)
	}
	if strings.Contains(obs, "s3cret-value") {
		t.Errorf("the preflight message quotes an environment value: %q", obs)
	}
}

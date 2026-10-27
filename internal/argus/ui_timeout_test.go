package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// a ui scenario's `## TIMEOUT` reaches Playwright through two environment variables,
// and it only LENGTHENS the vendored limits (test 60s, navigation 30s). These tests stand in for the
// browser (the uiProcess seam) and read the environment the process would have been started with.

func envValue(env []string, name string) string {
	v := ""
	for _, kv := range env { // the LAST one wins, as it does for a started process
		if k, val, ok := strings.Cut(kv, "="); ok && k == name {
			v = val
		}
	}
	return v
}

func captureUIEnv(t *testing.T) *[]string {
	t.Helper()
	var got []string
	orig := uiProcess
	t.Cleanup(func() { uiProcess = orig })
	uiProcess = func(vendorDir string, parts, env []string) (int, string) {
		got = env
		return 0, ""
	}
	return &got
}

func TestUIRun_DeclaredTimeoutBecomesTheTwoLimits(t *testing.T) {
	// a stray value in the ambient environment must lose to the run's own
	t.Setenv(uiTestTimeoutEnv, "1")
	t.Setenv(uiNavTimeoutEnv, "1")
	cases := []struct {
		name     string
		timeout  string // the scenario's `## TIMEOUT`; "" = declares nothing
		wantTest string
		wantNav  string
	}{
		{"nothing declared", "", "60000", "30000"},
		{"10s", "10s", "60000", "30000"},
		{"500ms", "500ms", "60000", "30000"},
		{"30s (the shipped value)", "30s", "60000", "30000"},
		{"60s", "60s", "60000", "60000"},
		{"120s", "120s", "120000", "120000"},
		{"300s", "300s", "300000", "300000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := captureUIEnv(t)
			s := &scenario.Scenario{ID: "UI-TO", Tags: []string{"ui"}, Timeout: c.timeout}
			s.Trigger.Payload = `{"spec":"tests/live/x.spec.ts","app_url":"http://localhost:5190"}`
			runUIScenario(s, "tr-to", t.TempDir())
			if got := envValue(*env, uiTestTimeoutEnv); got != c.wantTest {
				t.Errorf("%s = %q; want %q", uiTestTimeoutEnv, got, c.wantTest)
			}
			if got := envValue(*env, uiNavTimeoutEnv); got != c.wantNav {
				t.Errorf("%s = %q; want %q", uiNavTimeoutEnv, got, c.wantNav)
			}
		})
	}
}

// The chain `ui` step passes no declared value: the chain's own TIMEOUT and backstop bound it, so the
// limits are exactly today's, whatever the chain declares.
func TestChainUIStep_KeepsTheVendoredLimits(t *testing.T) {
	env := captureUIEnv(t)
	s := &scenario.Scenario{ID: "CHAIN-UI-TO", Tags: []string{"chain"}, Timeout: "120s"}
	s.Trigger.Payload = `{"steps":[{"type":"ui","name":"only","spec":"tests/live/x.spec.ts"}]}`
	runChainScenario(&config.Config{}, s, "tr-to", t.TempDir())
	if got := envValue(*env, uiTestTimeoutEnv); got != "60000" {
		t.Errorf("chain ui step %s = %q; want 60000", uiTestTimeoutEnv, got)
	}
	if got := envValue(*env, uiNavTimeoutEnv); got != "30000" {
		t.Errorf("chain ui step %s = %q; want 30000", uiNavTimeoutEnv, got)
	}
}

func TestUITimeoutEnv_OnlyLengthens(t *testing.T) {
	for _, ms := range []int{-5, 0, 1, 500, 10_000, 29_999, 30_000, 59_999, 60_000} {
		e := uiTimeoutEnv(ms)
		if len(e) != 2 || e[0] != uiTestTimeoutEnv+"=60000" {
			t.Errorf("declared %dms: %v — the per-test limit must stay 60000", ms, e)
		}
		if ms <= 30_000 && e[1] != uiNavTimeoutEnv+"=30000" {
			t.Errorf("declared %dms: %v — the navigation limit must stay 30000", ms, e)
		}
	}
	if e := uiTimeoutEnv(61_000); e[0] != uiTestTimeoutEnv+"=61000" || e[1] != uiNavTimeoutEnv+"=61000" {
		t.Errorf("declared 61000ms: %v", e)
	}
}

// The vendored config must read the two names uiTimeoutEnv sets, with the vendored values as the
// floor. This repo pins vendored TypeScript by its text (see TestVendoredConfig_WritesTheReport…): node
// is not part of the Go gate, so the text is what can be asserted without a browser.
func TestVendoredConfig_ReadsTheTimeoutVariablesAndKeepsTheFloors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testkit", "ui", "playwright.config.ts"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(b)
	for _, want := range []string{
		"timeout: atLeast(TEST_TIMEOUT_MS, '" + uiTestTimeoutEnv + "')",
		"navigationTimeout: atLeast(NAVIGATION_TIMEOUT_MS, '" + uiNavTimeoutEnv + "')",
		"const TEST_TIMEOUT_MS = 60_000;",
		"const NAVIGATION_TIMEOUT_MS = 30_000;",
		"v > floor ? v : floor", // never below the constant
		"actionTimeout: 15_000,",
		"expect: { timeout: 15_000 },",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("playwright.config.ts no longer contains %q", want)
		}
	}
}

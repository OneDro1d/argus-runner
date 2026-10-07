package ui_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// V29-021 (VR13-UI) — THE DECLARED-ASSERTIONS SPEC'S OWN CONTRACT, DRIVEN FROM GO.
//
// ⛔ THE ONE PROPERTY THIS FILE EXISTS FOR: **every declared bullet always produces an outcome
// entry**, whatever the outcomes are. The runner reads a MISSING entry as "not evaluated" and
// reports an execution error — so a spec that threw on the first failure and left the remaining
// bullets unrecorded would turn a SUT failure into "we did not look". That is the exact
// misattribution this round exists to remove, and one bare `await expect(...)` reintroduces it, so
// it is pinned here rather than trusted.
//
// ⚠ IT DRIVES PLAYWRIGHT FROM GO, and that shape was arrived at the hard way. Written first as a
// Playwright test that ran Playwright, the inner run kept re-selecting the OUTER project and grep —
// the contract spec ran ITSELF, recursively, and reported on nothing. Nesting a test runner inside
// itself is a poor bargain; one process per case is simpler and says what it means.
//
// ⛔ IT SKIPS, WITH A REASON, WHEN NODE OR THE BROWSER IS ABSENT. The round's parity rule allows
// exactly this for the node-dependent harness tests. A skip that NAMES why is honest; a test that
// silently passes on a machine with no browser is not.

type uiOutcome struct {
	Bullet   string `json:"bullet"`
	OK       bool   `json:"ok"`
	Observed string `json:"observed"`
}

// runDeclared runs the declared-assertions spec once against the static fixture page and returns
// what it wrote. skip is true when this machine has no node or no browser. The dom wait is short
// (1s): every page here is complete at first paint, so a longer one would only slow the failing cases.
func runDeclared(t *testing.T, asserts string) (outcomes []uiOutcome, exitOK bool, skip bool) {
	t.Helper()
	return runDeclaredOn(t, "declared.html", 1000, asserts)
}

// runDeclaredOn is runDeclared against a named fixture page with an explicit dom wait.
func runDeclaredOn(t *testing.T, pageFile string, domWaitMs int, asserts string) (outcomes []uiOutcome, exitOK bool, skip bool) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	vendorDir, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", "testkit", "ui"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vendorDir, "node_modules")); err != nil {
		return nil, false, true
	}
	npx := "npx"
	if runtime.GOOS == "windows" {
		npx = "npx.cmd"
	}
	if _, err := exec.LookPath(npx); err != nil {
		return nil, false, true
	}

	page := "file:///" + strings.ReplaceAll(filepath.Join(vendorDir, "fixtures", "pages", pageFile), `\`, "/")
	out := filepath.Join(t.TempDir(), "assertions.json")

	cmd := exec.Command(npx, "playwright", "test", "tests/live/argus-declared.spec.ts", "--reporter=list")
	cmd.Dir = vendorDir
	cmd.Env = append(os.Environ(),
		"SKIP_WEBSERVER=1", // the guard: without it the config starts `npm run dev`, which does not exist
		"APP_URL="+page,
		"ARGUS_UI_ASSERTS="+asserts,
		"ARGUS_UI_OUT="+out,
		"ARGUS_UI_DOM_WAIT_MS="+strconv.Itoa(domWaitMs),
	)
	combined, runErr := cmd.CombinedOutput()
	if strings.Contains(string(combined), "Executable doesn't exist") ||
		strings.Contains(string(combined), "playwright install") {
		return nil, false, true
	}
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("the spec wrote no outcomes file:\n%s", combined)
	}
	if err := json.Unmarshal(b, &outcomes); err != nil {
		t.Fatalf("the outcomes file is not JSON: %v\n%s", err, b)
	}
	return outcomes, runErr == nil, false
}

func TestUIHarness_EveryDeclaredBulletProducesAnEntry(t *testing.T) {
	// ⛔ THE CASE THAT MATTERS. The FIRST bullet fails; the second and third must still be evaluated
	// and recorded. With a bare `await expect(...)` the spec would stop at the first, the runner
	// would see two missing entries, and a SUT failure would be reported as an execution error.
	outcomes, exitOK, skip := runDeclared(t, `[
		{"bullet":"dom has .total containing 99.99","kind":"dom","selector":".total","op":"contains","value":"99.99"},
		{"bullet":"dom has .banner","kind":"dom","selector":".banner"},
		{"bullet":"no console errors","kind":"no_console_errors"}
	]`)
	if skip {
		t.Skip("node/playwright not installed")
	}
	if len(outcomes) != 3 {
		t.Fatalf("want one entry per declared bullet (3), got %d: %+v", len(outcomes), outcomes)
	}
	if outcomes[0].OK {
		t.Errorf("the failing bullet must be recorded as failed: %+v", outcomes[0])
	}
	if !outcomes[1].OK {
		t.Errorf("the bullet AFTER a failure must still be evaluated: %+v", outcomes[1])
	}
	if !outcomes[2].OK {
		t.Errorf("and the one after that: %+v", outcomes[2])
	}
	if exitOK {
		t.Error("the run must still end red — the tally fails the test")
	}
}

func TestUIHarness_ObservedIsRealityOnly(t *testing.T) {
	outcomes, _, skip := runDeclared(t, `[
		{"bullet":"dom has .total containing 99.99","kind":"dom","selector":".total","op":"contains","value":"99.99"}
	]`)
	if skip {
		t.Skip("node/playwright not installed")
	}
	if len(outcomes) != 1 {
		t.Fatalf("got %+v", outcomes)
	}
	// VR-C8: observed reaches the product hat, so it names what was THERE, never what was asked for
	if !strings.Contains(outcomes[0].Observed, "42.00") {
		t.Errorf("observed must name the actual value: %q", outcomes[0].Observed)
	}
	if strings.Contains(outcomes[0].Observed, "99.99") {
		t.Errorf("observed leaked the asserted value: %q", outcomes[0].Observed)
	}
}

func TestUIHarness_ASelectorThatMatchesNothingIsAFailedCheck(t *testing.T) {
	outcomes, _, skip := runDeclared(t, `[{"bullet":"dom has .nope","kind":"dom","selector":".nope"}]`)
	if skip {
		t.Skip("node/playwright not installed")
	}
	if len(outcomes) != 1 || outcomes[0].OK {
		t.Fatalf("a selector matching nothing is a FAILED check, not a missing entry: %+v", outcomes)
	}
	if !strings.Contains(outcomes[0].Observed, "no element matches") {
		t.Errorf("observed = %q", outcomes[0].Observed)
	}
}

func TestUIHarness_AllGreen(t *testing.T) {
	outcomes, exitOK, skip := runDeclared(t, `[
		{"bullet":"dom has .banner containing Order","kind":"dom","selector":".banner","op":"contains","value":"Order"},
		{"bullet":"dom has .total matching ^\\d+\\.\\d{2}$","kind":"dom","selector":".total","op":"matches","value":"^\\d+\\.\\d{2}$"},
		{"bullet":"no backend 4xx/5xx","kind":"no_backend_errors"}
	]`)
	if skip {
		t.Skip("node/playwright not installed")
	}
	for _, o := range outcomes {
		if !o.OK {
			t.Errorf("expected all green, got %+v", o)
		}
	}
	if !exitOK {
		t.Error("an all-green run must exit 0")
	}
}

// ⛔ memstore-dev WEBUI-001, run 20260918T150128060: the SPA is blank for 1-3s until Clerk loads, and
// the spec counted the selector the instant navigation returned, so it judged an empty body.
// declared-late.html renders its card 1.5s after first paint. RED on the no-wait spec ("no element
// matches .cl-rootBox"), GREEN with the bounded wait.
func TestUIHarness_ADomCheckWaitsForALateRender(t *testing.T) {
	outcomes, exitOK, skip := runDeclaredOn(t, "declared-late.html", 8000, `[
		{"bullet":"dom has .cl-rootBox","kind":"dom","selector":".cl-rootBox"},
		{"bullet":"dom has body containing Sign in to OneDroid","kind":"dom","selector":"body","op":"contains","value":"Sign in to OneDroid"}
	]`)
	if skip {
		t.Skip("node/playwright not installed")
	}
	if len(outcomes) != 2 {
		t.Fatalf("want 2 entries, got %+v", outcomes)
	}
	for _, o := range outcomes {
		if !o.OK {
			t.Errorf("a late-rendering element must be waited for: %+v", o)
		}
	}
	if !exitOK {
		t.Error("an all-green run must exit 0")
	}
}

// A check that times out says so, and names the auth requests that failed: the likeliest reason an
// SPA stays blank. Host + path only, never a query string.
func TestUIHarness_ATimedOutDomCheckNamesTheFailedClerkRequests(t *testing.T) {
	outcomes, _, skip := runDeclaredOn(t, "declared-late.html", 2500, `[
		{"bullet":"dom has .never","kind":"dom","selector":".never"},
		{"bullet":"dom has .never-either","kind":"dom","selector":".never-either"}
	]`)
	if skip {
		t.Skip("node/playwright not installed")
	}
	if len(outcomes) != 2 {
		t.Fatalf("want 2 entries, got %+v", outcomes)
	}
	for _, o := range outcomes {
		if o.OK {
			t.Fatalf("a selector that never appears is a FAILED check: %+v", o)
		}
		if !strings.Contains(o.Observed, "after waiting up to 2500ms") {
			t.Errorf("a timed-out check must say it waited: %q", o.Observed)
		}
		if !strings.Contains(o.Observed, "clerk.argus-fixture.invalid/npm/clerk.browser.js") {
			t.Errorf("a timed-out check must name the failed Clerk request: %q", o.Observed)
		}
	}
}

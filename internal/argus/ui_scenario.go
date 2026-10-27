package argus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
	"github.com/OneDro1d/argus-runner/internal/ui"
)

// uiOutcome maps a UI spec verdict to a request-response outcome for the per-request
// distribution panel (r3): passed→success; error(StatusError, no artifact / harness died)
// →error (no usable response); SUT-failed→failed (a negative response).
func uiOutcome(status string) string {
	switch status {
	case "passed":
		return report.OutcomeSuccess
	case report.StatusError:
		return report.OutcomeError
	default:
		return report.OutcomeFailed
	}
}

// UITag marks a web-UI scenario (D4): the runner drives a VENDORED Playwright spec
// (testkit/ui — no private dev-kit) host-side as an OS process and maps the outcome to
// a verdict (rendered-DOM asserts + the "no backend 4xx/5xx during the flow" assertion
// inside the spec). A harness failure is a DISTINCT execution error, never silent-green
// (VR-L3 / UC-62). It rides the SAME RunAll pipeline as any scenario, tag-selectable by
// `ui` and layer-selectable by its layer (UC-84). DF-DEC-M25-15: in the local-compose
// runner the OS-Process-Sampler role is played by the runner's own process spawn (the
// containerized JMeter cannot launch a host browser); the verdict mapping is identical.
const UITag = scenario.UITag

type uiSpec struct {
	Spec   string `json:"spec"`
	AppURL string `json:"app_url"`
}

// parseUISpec reads the ui scenario's TRIGGER payload JSON ({spec, app_url?}) AS WRITTEN. `spec` is the
// vendored Playwright spec path (relative to testkit/ui); `app_url` optionally overrides the ambient
// APP_URL. An empty payload is not an error: the spec then comes from the TRIGGER url.
//
// ⛔ NOTHING IS RESOLVED HERE — runUIScenario resolves each field, once (#160, #161). Resolving the
// JSON TEXT first put the raw ${VAR} value inside a JSON string: a `"` in it made the payload invalid,
// the discarded error left spec and app_url empty, and the run fell back to the TRIGGER url and the
// ambient APP_URL — a different address, with nothing in the row saying why. And the field was then
// resolved a SECOND time, so a value holding ${...} expanded again. Parsing first also matches the
// write-time check, which reads the same raw payload (scenario.UINamesItsOwnSpec).
func parseUISpec(payload string) (uiSpec, error) {
	var s uiSpec
	if strings.TrimSpace(payload) == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(payload), &s); err != nil {
		return uiSpec{}, err
	}
	return s, nil
}

// uiRun is the injectable OS-process runner (swapped in unit tests). It runs the
// vendored Playwright command for spec in vendorDir with APP_URL + the correlation id
// in env and returns the exit code, whether the SUT was EXERCISED (uiExercised), and
// captured stderr. A harness error (no browser, missing dep, no tests) exits non-zero
// WITHOUT a test having run — that is the discriminator ui.Classify uses to separate a
// SUT failure from an execution failure (UC-62/84).
//
// ⛔ BOTH callers go through here (the standalone ui scenario and the chain `ui` step), so the
// clearing of THIS run's evidence lives here. Order: test-results/ and argus-out/results.json are
// removed FIRST, then the process starts, and only then is either read. runUIScenario clears the
// whole of argus-out/ (assertions.json included) BEFORE it calls this; the chain step clears
// nothing else, so without the results.json line a chain `ui` step would judge the last run's report.
//
// declaredMs is the scenario's own `## TIMEOUT` in milliseconds, 0 = none declared
// (the chain `ui` step passes 0: the chain's own TIMEOUT and backstop already bound it). It only
// LENGTHENS the vendored limits, see uiTimeoutEnv.
var uiRun = func(vendorDir string, specs []string, appURL, corr string, asserts []scenario.UIAssert, outPath string, declaredMs int) (exitCode int, hasResults bool, stderr string) {
	resultsDir := filepath.Join(vendorDir, "test-results")
	_ = os.RemoveAll(resultsDir) // Playwright APPENDS per-test dirs; start each run fresh
	// the JSON report is read AFTER the process, so one left by an earlier run would
	// read as THIS run's "a test ran". Removed here, in the same place and for the same reason.
	_ = os.Remove(uiResultsPath(vendorDir))
	parts := ui.Command(specs...)
	// the declared asserts reach the spec as JSON, and the spec writes its per-bullet outcomes to
	// ARGUS_UI_OUT. ⛔ Both are OUTSIDE test-results/: that directory is a VR-L3 discriminator
	// (hasTestArtifacts), so putting our own files there would make a harness failure look like a
	// SUT one.
	assertsJSON, _ := json.Marshal(asserts)
	env := append(os.Environ(),
		"APP_URL="+appURL,
		"ARGUS_CORRELATION_ID="+corr,
		"ARGUS_UI_ASSERTS="+string(assertsJSON),
		"ARGUS_UI_OUT="+outPath,
	)
	env = append(env, uiTimeoutEnv(declaredMs)...)
	exitCode, stderr = uiProcess(vendorDir, parts, env)
	return exitCode, uiExercised(vendorDir), stderr
}

// The vendored config's own limits (testkit/ui/playwright.config.ts: `timeout: 60_000`,
// `navigationTimeout: 30_000`). They are the FLOOR a declared `## TIMEOUT` can only raise.
const (
	uiTestTimeoutFloorMs = 60_000
	uiNavTimeoutFloorMs  = 30_000
	// The two names the config reads (and ui_timeout_test.go pins in its text).
	uiTestTimeoutEnv = "ARGUS_UI_TEST_TIMEOUT_MS"
	uiNavTimeoutEnv  = "ARGUS_UI_NAV_TIMEOUT_MS"
)

// uiTimeoutEnv is what a declared `## TIMEOUT` becomes for Playwright: the per-test
// timeout is max(60 s, declared) and the navigation timeout max(30 s, declared). It LENGTHENS and never
// shortens: a browser flow is many round trips, and a declared 10 s (the template's value, the parse
// default) must not turn a 60 s test into a 10 s one. Nothing declared gives the vendored values
// exactly. Action and expect timeouts are not touched. ⛔ No process-level kill is added: a hung
// browser process is still bounded only by Playwright.
func uiTimeoutEnv(declaredMs int) []string {
	test, nav := uiTestTimeoutFloorMs, uiNavTimeoutFloorMs
	if declaredMs > test {
		test = declaredMs
	}
	if declaredMs > nav {
		nav = declaredMs
	}
	return []string{
		uiTestTimeoutEnv + "=" + strconv.Itoa(test),
		uiNavTimeoutEnv + "=" + strconv.Itoa(nav),
	}
}

// uiProcess starts the command and waits for it. It is a seam of its own, BELOW uiRun, so a test can
// stand in for the browser (write the files Playwright would have written, return an exit code) and
// still run uiRun's real clearing and uiExercised's real reading.
var uiProcess = func(vendorDir string, parts, env []string) (exitCode int, stderr string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", append([]string{"/c"}, parts...)...) // npm is npm.cmd on Windows
	} else {
		cmd = exec.Command(parts[0], parts[1:]...)
	}
	cmd.Dir = vendorDir
	cmd.Env = env
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	_ = cmd.Run() // exit code read from ProcessState below; stdout discarded (artifacts are the evidence)
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else {
		exitCode = -1
	}
	return exitCode, errBuf.String()
}

// declaredUITimeoutMs is the scenario's `## TIMEOUT` in ms, 0 when it declares none (a Scenario built
// without one would otherwise read as the parser's 10 s default; uiTimeoutEnv ignores anything below
// its floors either way, so the two readings give the same limits).
func declaredUITimeoutMs(s *scenario.Scenario) int {
	if strings.TrimSpace(s.Timeout) == "" {
		return 0
	}
	return int(s.TimeoutDuration() / time.Millisecond)
}

// uiResultsPath is where the vendored config's json reporter writes (argus-out/results.json).
func uiResultsPath(vendorDir string) string {
	return filepath.Join(vendorDir, uiOutDirName, "results.json")
}

// uiExercised reports whether a test actually RAN in this run: Playwright's own account first, the
// per-test directory only when there is no readable account.
//
// "a per-test directory exists" was the whole rule, and it holds only while tracing
// is on (every test leaves a trace). A spec that sets `trace: 'off'` and passes leaves nothing, and
// the run read as an execution failure on a healthy system. The JSON reporter the vendored config
// already writes states what ran: stats.expected + unexpected + flaky >= 1. Zero tests, or all
// skipped, is still "not exercised" (VR-L3: absent results != pass).
//
// A missing, unreadable or stats-less file (a foreign config with no JSON reporter) falls back to
// hasTestArtifacts, exactly as before. The file is this run's only because uiRun removed it first.
func uiExercised(vendorDir string) bool {
	if b, err := os.ReadFile(uiResultsPath(vendorDir)); err == nil {
		var rep struct {
			Stats *struct {
				Expected   int `json:"expected"`
				Unexpected int `json:"unexpected"`
				Flaky      int `json:"flaky"`
			} `json:"stats"`
		}
		if json.Unmarshal(b, &rep) == nil && rep.Stats != nil {
			return rep.Stats.Expected+rep.Stats.Unexpected+rep.Stats.Flaky >= 1
		}
	}
	return hasTestArtifacts(filepath.Join(vendorDir, "test-results"))
}

// hasTestArtifacts reports whether Playwright produced a real per-test artifact (the
// fallback discriminator when there is no JSON report, UC-62). A test that actually RAN
// writes a per-test SUBDIR under test-results/ (trace/screenshot/video on failure; trace
// on pass, when tracing is on). A harness failure ("no tests found" / config / launch
// error) writes ONLY the `.last-run.json` metadata marker — which does NOT count.
func hasTestArtifacts(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != ".last-run.json" {
			return true
		}
	}
	return false
}

// runUIScenario executes one ui-typed scenario (vendored Playwright host-side) and
// builds a standard ScenarioResult: exit 0 -> passed; exit!=0 WITH a results artifact
// -> SUT "failed" (a DOM assert or a backend 4xx/5xx during the flow fired); exit!=0
// WITHOUT artifact -> report.StatusError (a DISTINCT execution/harness failure, UC-62).
// Observed is reality-only (VR-C8) — it never restates the EXPECT.
func runUIScenario(s *scenario.Scenario, corr, vendorDir string) report.ScenarioResult {
	res := report.ScenarioResult{ID: s.ID, CorrelationID: corr, Status: "failed"}
	spec, err := parseUISpec(s.Trigger.Payload)
	if err != nil {
		// Parsed before any ${VAR} is resolved, so this error is about the file as written and
		// quotes none of the environment's values.
		res.Failure = &report.Failure{Observed: "ui scenario: the TRIGGER payload is not valid JSON (" + err.Error() + ") — preflight"}
		return res
	}
	if spec.Spec == "" { // fall back to the TRIGGER url (POST `tests/live/x.spec.ts` — method is a placeholder)
		spec.Spec = strings.Trim(s.Trigger.URL, "`")
	}
	// Each field is resolved exactly ONCE, here (#161): spec below, app_url after it.
	spec.Spec = resolveVars(spec.Spec, corr)
	if spec.Spec == "" || strings.Contains(spec.Spec, "${") {
		res.Failure = &report.Failure{Observed: "ui scenario: missing/unresolved spec path (set a `spec` payload field or a TRIGGER url) — preflight"}
		return res
	}
	appURL := resolveVars(spec.AppURL, corr)
	if appURL == "" {
		appURL = os.Getenv("APP_URL")
	}
	// V29-021: the DECLARED assertions. A bullet the grammar cannot execute is refused when the
	// scenario is WRITTEN (uiExpectErrors), so a parse error here can only come from a catalogue
	// written before the rule — the run says so rather than judging the file's format.
	asserts, perrs := scenario.ParseUIExpect(s.RunnableExpect())
	if len(perrs) > 0 {
		res.Failure = &report.Failure{Observed: perrs[0].Error() + " — preflight"}
		return res
	}
	outPath := filepath.Join(vendorDir, uiOutDirName, "assertions.json")
	// ⛔ ABSOLUTE, because the spec runs with cmd.Dir = vendorDir: a relative path (ui.VendorDir is
	// "testkit/ui") is resolved again from inside the kit, the spec writes testkit/ui/testkit/ui/…, and
	// the runner reads a file nobody wrote — every declared bullet "not evaluated" (memstore-dev
	// WEBUI-001, run 20260918T150128060).
	if abs, err := filepath.Abs(outPath); err == nil {
		outPath = abs
	}
	// ⛔ V29-021 U-B — CLEARED HERE, OUTSIDE THE SEAM, AND THAT PLACEMENT IS THE POINT.
	//
	// A previous run's all-green assertions.json read as THIS run's evidence is the worst thing this
	// row could ship: a green verdict on a run that wrote nothing. uiRun clears `test-results/` for
	// the same reason — but uiRun is the INJECTABLE seam, so a clearing that lives inside it is a
	// guarantee any substitute runner can drop. (Measured: with the line inside the seam, the stale
	// test passed as `passed`.) The evidence directory is cleared by the caller, where nothing can
	// swap it out.
	_ = os.RemoveAll(filepath.Join(vendorDir, uiOutDirName))

	startMs := time.Now().UnixMilli() // the spec run's real fire-time
	exitCode, hasResults, stderr := uiRun(vendorDir, []string{spec.Spec}, appURL, corr, asserts, outPath, declaredUITimeoutMs(s))
	status, observed := ui.Classify(exitCode, hasResults, stderr)

	// ⛔ THE DECLARED ASSERTIONS DECIDE, ONCE THE HARNESS IS KNOWN TO HAVE RUN. Classify still owns
	// the execution/SUT split (VR-L3: absent results is not a pass), and only then are the author's
	// own claims read back. A bullet with no entry was NOT EVALUATED — that is an execution error,
	// never a pass and never a SUT failure, because turning "we did not look" into "the SUT is
	// wrong" is the misattribution this product exists to prevent.
	if status != report.StatusError {
		outcomes := readUIOutcomes(outPath)
		byBullet := map[string]uiOutcomeEntry{}
		for _, o := range outcomes {
			byBullet[o.Bullet] = o
		}
		var missing []string
		var failedObs []string
		for _, a := range asserts {
			o, ok := byBullet[a.Bullet]
			if !ok {
				missing = append(missing, a.Bullet)
				continue
			}
			res.AssertionsEnforced = append(res.AssertionsEnforced, a.Bullet)
			if !o.OK {
				failedObs = append(failedObs, o.Observed)
			}
		}
		res.AssertionsEnforcedCount = len(res.AssertionsEnforced)
		switch {
		case len(missing) > 0:
			// #139: the verdict stays "not measured", and the cause is kept. A failed spec that wrote no
			// outcome usually never reached the checks (page.goto threw), and Playwright recorded why.
			cause, fromPlaywright := "", false
			if status == "failed" {
				cause, fromPlaywright = uiFailureCause(filepath.Join(filepath.Dir(outPath), "results.json"), stderr, observed)
			}
			status = report.StatusError
			observed = fmt.Sprintf("%d of %d declared ui assertions were not evaluated — the SUT was not measured (V29-021)",
				len(missing), len(asserts))
			// ⛔ ONLY A LINE PLAYWRIGHT ACTUALLY WROTE MAY BE QUOTED AS PLAYWRIGHT'S. The fallback is
			// Classify's own text, and labelling it "Playwright:" sent readers hunting for a log line
			// that was never written — and, worse, made the row assert a DOM/backend failure one clause
			// after saying nothing was measured. Keep the observation (it is the only context there is);
			// attribute it honestly.
			switch {
			case fromPlaywright:
				observed += ". Playwright: " + cause
			case cause != "":
				observed += ". " + cause
			}
		case len(failedObs) > 0:
			status = "failed"
			observed = strings.Join(failedObs, "; ")
		}
	}
	res.Status = status
	// Test-requests panel (r3): one request per spec run, recorded as a timestamped sample by
	// its outcome (3-way): passed → success; SUT-failed (DOM/backend assert, an artifact exists)
	// → failed (a negative response); execution error (no artifact, harness died) → error (no
	// usable response). (The preflight return above fired no spec → no sample.)
	res.AddRequest(startMs, uiOutcome(status))
	if status != "passed" {
		exp := strings.Join(s.Expect, "; ")
		// ⛔ REDACTED AT THE SINK. Every branch above can quote a URL Playwright or the spec saw AFTER
		// ${VAR} expansion — the #139 cause, Classify's first stderr line (no artifact), a failed
		// "backend errors: GET <url> → 5xx" bullet — and Failure.Observed leaves the executor (report
		// files, runner_get_report, the Memstore build record). The expanded values go back to their
		// ${NAME} first (a secret in a path or outside any URL), then redactURLs is the backstop.
		observed = redactURLs(scrubVarValues(observed, s.Trigger.Payload))
		res.Failure = &report.Failure{Observed: observed, Expected: &exp}
	}
	return res
}

// ── V29-021 (VR13-UI): THE DECLARED ASSERTIONS' OUTCOMES ──────────────────────────────────────

// uiOutDirName is where the spec writes its evidence. ⛔ NOT `test-results/`: that directory is the
// VR-L3 discriminator (hasTestArtifacts reads it to tell a SUT failure from a harness one), so a
// file of ours in there would make an execution failure look like a SUT failure.
// uiFailureCause is the first error Playwright recorded for a failed spec run (#139): from its JSON
// report (the `json` reporter in testkit/ui/playwright.config.ts writes argus-out/results.json, cleared
// before every run with the rest of argus-out/), else the first line of stderr, else fallback. One
// line, without colour codes: Playwright's messages carry ANSI escapes and a "Call log" tail.
// The bool reports whether the string really came FROM Playwright (its JSON report or its stderr).
// False means the caller is holding the fallback — Argus's own words — which must never be quoted
// under Playwright's name.
func uiFailureCause(resultsPath, stderr, fallback string) (string, bool) {
	if b, err := os.ReadFile(resultsPath); err == nil {
		var rep pwReport
		if json.Unmarshal(b, &rep) == nil {
			if m := rep.firstError(); m != "" {
				return m, true
			}
		}
	}
	// strip colour codes BEFORE taking the line: a stderr that opens with codes on a line of their
	// own otherwise yields an empty "first line", and the real error is dropped for the fallback.
	if line := firstLineOf(ansiRe.ReplaceAllString(stderr, "")); line != "" {
		return line, true
	}
	return fallback, false
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// urlRe is a URL-shaped substring: a scheme, "://", and everything up to whitespace, a double quote
// or an angle bracket. NOT an apostrophe: it is legal unencoded in userinfo and path, and stopping
// there handed back everything after it — the rest of the password and the whole query.
var urlRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"<>]+`)

// hostAfterAtRe is what must follow an '@' for it to end a userinfo: a host (a name or an IPv6
// literal), an optional port, then the end or one of / ? #.
var hostAfterAtRe = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._~%-]+)(:[0-9]*)?([/?#]|$)`)

// redactURLs keeps where each URL in s points (scheme, host, port, path — what #139 needs to show
// a wrong address) and drops what may be a credential: the userinfo becomes "REDACTED@", and the
// query and fragment become "?REDACTED". amqpengine.RedactURL and javaSamplerURI remove the userinfo
// only, and a ?token= survives both.
//
// Which '@' ends the userinfo (k = the first '?' or '#'): the later of the last '@' a host[:port]
// follows and the last '@' before k. So an unencoded '/', '?', '#' or '@' in a password cannot split
// it, and a userinfo is still found when no readable host follows ("amqp://svc:S@rabbitmq:5672:" at
// the end of a sentence, or "@${SUT_HOST}" after the ${VAR} scrub). With neither, but an '@' in the
// query and no host right after "://", the last '@' anywhere. An '@' chosen AFTER k cannot be told
// apart from query text (?next=/@alice/x&token=S), so then neither survives: scheme://REDACTED?REDACTED.
// Everything before the chosen '@' is dropped, so a wrong choice loses the host, never leaks:
// http://h:1/@scope/x gives http://REDACTED@scope/x.
func redactURLs(s string) string {
	return urlRe.ReplaceAllStringFunc(s, func(m string) string {
		scheme, rest, _ := strings.Cut(m, "://")
		k := strings.IndexAny(rest, "?#")
		if k < 0 {
			k = len(rest)
		}
		at := strings.LastIndexByte(rest[:k], '@')
		for i := len(rest) - 1; i > at; i-- {
			if rest[i] == '@' && hostAfterAtRe.MatchString(rest[i+1:]) {
				at = i
				break
			}
		}
		if at < 0 && strings.IndexByte(rest[k:], '@') >= 0 && !hostAfterAtRe.MatchString(rest) {
			at = strings.LastIndexByte(rest, '@')
		}
		if at > k {
			return scheme + "://REDACTED?REDACTED"
		}
		user := ""
		if at >= 0 {
			rest, user = rest[at+1:], "REDACTED@"
		}
		q := ""
		if i := strings.IndexAny(rest, "?#"); i >= 0 {
			rest, q = rest[:i], "?REDACTED"
		}
		return scheme + "://" + user + rest + q
	})
}

// minScrubLen is the shortest ${VAR} value scrubVarValues replaces: a shorter one ("1", "dev") would
// also blank ordinary words in the row, and is no secret worth the noise.
const minScrubLen = 8

// scrubVarValues puts ${NAME} back wherever the expanded value of a ${NAME} the TRIGGER payload
// references appears in s (also URL-escaped). No URL pattern can close a secret in a path, one
// quoted outside a URL, or one in a URL the pattern does not see; the values themselves are known
// here. Skipped: ${cid}/${correlation_id}, values shorter than minScrubLen, and whole URLs (an
// ${APP_URL} is the address #139 has to show — redactURLs covers its credentials).
func scrubVarValues(s, payload string) string {
	type nv struct{ name, v string }
	var vars []nv
	for _, m := range varRe.FindAllStringSubmatch(payload, -1) {
		name := m[1]
		if name == "cid" || name == "correlation_id" {
			continue
		}
		v := os.Getenv(name)
		if len(v) < minScrubLen || urlRe.FindString(v) == v {
			continue
		}
		vars = append(vars, nv{name, v})
	}
	// Longest first: a value that is a prefix of a longer one (TENANT=acme-prod-01,
	// API_KEY=acme-prod-01-9f8e…) would otherwise split it and leave the tail.
	sort.SliceStable(vars, func(i, j int) bool { return len(vars[i].v) > len(vars[j].v) })
	for _, x := range vars {
		for _, form := range []string{x.v, url.QueryEscape(x.v), url.PathEscape(x.v)} {
			s = strings.ReplaceAll(s, form, "${"+x.name+"}")
		}
	}
	return s
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// pwReport is the part of Playwright's JSON report that carries errors: top-level errors (a failure
// outside any test), and suites → specs → tests → results → errors.
type pwReport struct {
	Errors []pwError `json:"errors"`
	Suites []pwSuite `json:"suites"`
}
type pwError struct {
	Message string `json:"message"`
}
type pwSuite struct {
	Suites []pwSuite `json:"suites"`
	Specs  []struct {
		Tests []struct {
			Results []struct {
				Errors []pwError `json:"errors"`
			} `json:"results"`
		} `json:"tests"`
	} `json:"specs"`
}

func (r pwReport) firstError() string {
	for _, e := range r.Errors {
		if m := cleanPWMessage(e.Message); m != "" {
			return m
		}
	}
	var walk func([]pwSuite) string
	walk = func(ss []pwSuite) string {
		for _, s := range ss {
			for _, sp := range s.Specs {
				for _, t := range sp.Tests {
					for _, r := range t.Results {
						for _, e := range r.Errors {
							if m := cleanPWMessage(e.Message); m != "" {
								return m
							}
						}
					}
				}
			}
			if m := walk(s.Suites); m != "" {
				return m
			}
		}
		return ""
	}
	return walk(r.Suites)
}

func cleanPWMessage(m string) string {
	return strings.TrimPrefix(firstLineOf(ansiRe.ReplaceAllString(m, "")), "Error: ")
}

const uiOutDirName = "argus-out"

// uiOutcomeEntry is one declared bullet's result, as the spec writes it.
//
// `Observed` is REALITY-ONLY (VR-C8): it says what was actually in the DOM, never what the scenario
// asked for. It reaches the product hat, and the asserted value is holdout material.
type uiOutcomeEntry struct {
	Bullet   string `json:"bullet"`
	OK       bool   `json:"ok"`
	Observed string `json:"observed"`
}

// readUIOutcomes reads the spec's per-bullet evidence. A missing file is not an error here — the
// caller decides what an absent entry means, and it is the caller that must never read it as a pass.
func readUIOutcomes(path string) []uiOutcomeEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []uiOutcomeEntry
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

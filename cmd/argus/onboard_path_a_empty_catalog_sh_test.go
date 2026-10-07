package main

// onboard_path_a_empty_catalog_sh_test.go — AC-D48 (GitHub #251): onboarding printed ALL SET, and
// recorded the instance `complete`, over an EMPTY scenario catalog — including on Path A, where the
// scenarios ship with Argus.
//
// THE RULE (owner, 2026-09-25). Path A's scenarios ship inside the image, so importing them is
// MANDATORY: zero imported means the onboarding FAILED. Path B's scenarios are the user's: supplying
// none is ALL SET, with a warning.
//
// ⛔ "COULD NOT MEASURE" IS NOT "MEASURED EMPTY". The seed branch that fired in the incident also fires
// when the import's report cannot be read, and a Path A failure with a control plane tears the instance
// down and de-registers it. So the unreadable report is pinned as NOT failed, beside the measured zero
// that must fail.
//
// RUN, NOT READ: onboard.sh is RUN under the harness (stubbed docker/kubectl/curl) on the compose tier,
// with the control-plane fixture set of onboard_failure_block_sh_test.go. What the control plane was
// told is read off the stamp's own call (`-e ARGUS_ONBOARDING_STATE=<state>`); a teardown off the calls
// only teardown.sh makes (cloud-deregister, `down -v`). Every test proves its path was TAKEN and
// isolates the cause before it judges the outcome (see mustHaveCalled).

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const d48AllSet = "ALL SET — ARGUS MCP IS UP AND RUNNING."

// d48Unconfirmed is the headline for every catalog nobody measured (01-design.md decision 19: it was
// "…THE SCENARIO IMPORT COULD NOT BE CONFIRMED.", false when no import ran — a refresh with an empty
// folder, or no seed credential).
const d48Unconfirmed = "PARTIALLY SET — REGISTERED AND WIRED, BUT ITS SCENARIO CATALOG IS UNCONFIRMED."

// d48FolderUnchecked is the same judgement WITHOUT a control plane (decision 26): nothing registered and
// no catalog, only a scenario folder the probe could not look into — so the headline names that.
const d48FolderUnchecked = "PARTIALLY SET — WIRED, BUT ITS SCENARIO FOLDER COULD NOT BE CHECKED."

type d48Case struct {
	// --path: "a" (the OrderService demo) | "b" (BYO-SUT) | "" passes NO --path, so onboard.sh's own
	// default decides (decision 21: an exported PATH_MODE must not).
	path      string
	scenario  bool             // the test folder holds a scenario file, so HAS_SCEN is non-empty
	seedToken bool             // deploy/compose/cp-author.token exists, so the import is attempted
	cp        bool             // --control-plane, with the cloud fixtures
	inst      string           // --instance-id; "" passes none, as test-orderservice.sh's no-CP arm does
	fx        []harnessFixture // tried before the base set
	// kitSeed: extra files written INTO THE KIT before the run (path relative to the kit root) — e.g.
	// deploy/compose/env.<id> holding a machine identity, which is what makes a run a REFRESH.
	kitSeed map[string]string
	// testFiles: extra files written into the TEST folder (<sand>/test, relative) — e.g. the .mcp.json
	// the step-9/9 routability probe reads its router token from.
	testFiles map[string]string
	// env: extra NAME=value entries for the script's environment (withHarnessEnv) — e.g. an exported
	// PATH_MODE or IS_REFRESH the script must ignore.
	env []string
}

func d48Run(t *testing.T, c d48Case) harnessRun {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	sand := t.TempDir()
	prod, scen := filepath.Join(sand, "prod"), filepath.Join(sand, "test", "scenarios")
	for _, d := range []string{prod, scen} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	cfg, err := os.ReadFile(filepath.Join("..", "..", "examples", "memstore", "argus-config.yaml"))
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	mustWrite(t, filepath.Join(prod, "argus-config.yaml"), string(cfg))
	if c.scenario {
		mustWrite(t, filepath.Join(scen, "S-001.md"), "# a scenario\n")
	}
	for rel, body := range c.testFiles {
		p := filepath.Join(sand, "test", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		mustWrite(t, p, body)
	}
	// A terminal that DECLINES: after a failure the trap offers a retry, and nobody is here to take it.
	answers := filepath.Join(t.TempDir(), "answers")
	mustWrite(t, answers, "n\n")
	// ARGUS_ONBOARD_ATTEMPT=1 explicitly, not inherited: the trap's attempt >= 2 arm also withholds the
	// retry prompt, so a prompt's absence means `retryable no` only while this run is attempt 1.
	withHarnessEnv(t, append([]string{"ASK_TTY=" + filepath.ToSlash(answers), "ARGUS_ONBOARD_ATTEMPT=1"}, c.env...)...)

	seed := harnessSeed{}
	for rel, body := range c.kitSeed {
		seed[rel] = body
	}
	fx := append([]harnessFixture{}, c.fx...)
	fx = append(fx,
		harnessFixture{Match: "docker info", Out: "Server Version: 99.0\n"},
		harnessFixture{Match: "keygen", Out: "MC4CAQAwBQYDK2VwBCIEIHarnessOnlyNotARealKeyAAAAAAAAAAAAAAAA=\n"},
		harnessFixture{Match: "render-obs", Out: `{"sut_network":"probe_default","sut_project":"probe"}` + "\n"},
		harnessFixture{Match: "preflight-auth", Out: `{"verdict": "ok"}` + "\n"},
		harnessFixture{Match: "router wire", Out: `{"port": 9765, "mcp_json": "/x/.mcp.json"}` + "\n"},
		harnessFixture{Match: "docker inspect -f {{.State.Status}}", Out: "running\n"},
		// The router ANSWERS, so a downgraded banner is never the router's (asserted per test).
		harnessFixture{Match: "127.0.0.1:9765/ready", Out: "200"},
	)
	args := []string{"--product-dir", prod, "--scenarios-dir", filepath.Join(sand, "test"),
		"--tier", "compose", "--image", "img:test"}
	if c.path != "" {
		args = append(args, "--path", c.path)
	}
	if c.inst != "" {
		args = append(args, "--instance-id", c.inst)
	}
	if c.cp {
		// Written by cloud-login and render-obs respectively; a stub cannot create files.
		seed["deploy/compose/cp-session.token"] = "odts_harnessSessionNotRealAAAAAAAAAAAAAAAAAAAA\n"
		seed["deploy/compose/promtail-config.yaml"] = "# harness placeholder\n"
		fx = append(fx,
			harnessFixture{Match: "cloud-login", Out: `{"workspace":"ws-probe"}` + "\n"},
			harnessFixture{Match: "cloud-check-instance", Out: `{"valid": true,"available": true,"owned_by_you": false,"stale": false}` + "\n"},
			harnessFixture{Match: "cloud-executor-status", Out: `{"registered": true,"poll_accepted": true}` + "\n"},
			harnessFixture{Match: "cloud-deregister", Out: `{"deregistered": true}` + "\n"},
			harnessFixture{Match: "api/health", Out: "200"},
			harnessFixture{Match: "cp.example/mcp", Out: "200"},
			// The per-instance project HAS containers, so a teardown resolves it and issues `down -v`
			// (teardown.sh reads the project from `ps -a`; an empty answer skips the down entirely).
			harnessFixture{Match: "ps -a --format {{.Name}}", Out: "argus-inst-" + c.inst + "-executor-1\n"},
		)
		args = append(args, "--control-plane", "https://cp.example")
	}
	if c.seedToken {
		// cloud-mint-token writes this and the stub cannot; the seed reads it as SEED_TOKEN.
		seed["deploy/compose/cp-author.token"] = "odat_harnessAuthorNotRealAAAAAAAAAAAAAAAAAAAAAA\n"
	}
	return runKitScriptSeeded(t, 170*time.Second, "onboard.sh", seed, fx, args...)
}

// d48Stamps returns every state the run sent to the control plane, in order.
func d48Stamps(r harnessRun) []string {
	var states []string
	for _, c := range r.Calls {
		if i := strings.Index(c, "ARGUS_ONBOARDING_STATE="); i >= 0 {
			v := c[i+len("ARGUS_ONBOARDING_STATE="):]
			if j := strings.IndexAny(v, " \t"); j >= 0 {
				v = v[:j]
			}
			states = append(states, v)
		}
	}
	return states
}

// d48TeardownCalls returns the calls only teardown.sh makes: the cloud de-register and the `down -v`.
func d48TeardownCalls(r harnessRun) []string {
	var out []string
	for _, c := range r.Calls {
		if strings.Contains(c, "cloud-deregister") || strings.Contains(c, "down -v") {
			out = append(out, c)
		}
	}
	return out
}

// d48Headline returns the banner's headline: the line above "… IS ONBOARDED AND READY TO BE TESTED.".
func d48Headline(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for i := len(lines) - 1; i > 0; i-- {
		if strings.Contains(lines[i], "IS ONBOARDED AND READY TO BE TESTED") {
			return strings.TrimSpace(lines[i-1])
		}
	}
	t.Fatalf("the run never printed its banner, so there is no headline to judge.\n  output:\n%s", out)
	return ""
}

// d48LineIndex returns the byte offset of the first line containing every one of <parts>, or -1.
func d48LineIndex(out string, parts ...string) int {
	off := 0
	for _, l := range strings.SplitAfter(out, "\n") {
		all := true
		for _, p := range parts {
			if !strings.Contains(l, p) {
				all = false
				break
			}
		}
		if all {
			return off
		}
		off += len(l)
	}
	return -1
}

func d48MustNotHaveCalled(t *testing.T, r harnessRun, needle, why string) {
	t.Helper()
	for _, c := range r.Calls {
		if strings.Contains(c, needle) {
			t.Fatalf("the run invoked %q (%s), so it is not on the path this test describes.\n  call: %s", needle, why, c)
		}
	}
}

// d48ExitCode is the script's exit status. ⚠ NOT `Err != nil`: a FINISHED onboard exits 3 when the
// runtime is not ready yet (RUNTIME_NOT_READY), so only die()'s 1 says the run was failed.
func d48ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func d48HasState(states []string, want string) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

func d48ContainsAll(l string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(l, p) {
			return false
		}
	}
	return true
}

// d48WarnBlock returns the first WARN whose FIRST line carries every one of <parts>, joined with its
// continuation lines (onboard.sh wraps a WARN at a 10-space indent), or "" when no first line does.
// Keyed on the first line on purpose: a WARN that says UNCONFIRMED only on its third line is not one
// whose first line says it.
func d48WarnBlock(out string, parts ...string) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for i, l := range lines {
		if !strings.Contains(l, "WARN:") || !d48ContainsAll(l, parts...) {
			continue
		}
		block := []string{l}
		for _, c := range lines[i+1:] {
			if !strings.HasPrefix(c, "          ") || strings.Contains(c, "WARN:") {
				break
			}
			block = append(block, c)
		}
		return strings.Join(block, "\n")
	}
	return ""
}

// d48ScenariosLine returns the banner's `Scenarios:` value (the SCEN_LINE the seed block set).
func d48ScenariosLine(t *testing.T, out string) string {
	t.Helper()
	got := ""
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if s := strings.TrimSpace(l); strings.HasPrefix(s, "Scenarios:") {
			got = strings.TrimSpace(strings.TrimPrefix(s, "Scenarios:"))
		}
	}
	if got == "" {
		t.Fatalf("the banner printed no `Scenarios:` line, so SCEN_LINE cannot be judged.\n  output:\n%s", out)
	}
	return got
}

// d48MustBeTheGatesOwnFailureBlock: the trap printed the failure block — so the run reached the retry
// decision, which comes right after it — WHAT WENT WRONG and WHAT THAT MEANS carry the Path A gate's own
// text, no retry is offered, and the SUT is not blamed.
//
// The retry prompt's ABSENCE is meaningful under this harness: with the same ASK_TTY answers file,
// TestOnboardSh_AnUnrecognisedErrorStillPrintsTheWholeBlock sees `Try onboarding again` on stdout. The
// trap has three arms — retryable=no (silent), attempt >= 2 (`This was attempt …`), else the prompt —
// and d48Run pins ARGUS_ONBOARD_ATTEMPT=1, so the prompt's absence means retryable=no suppressed it.
//
// TQ-8: `Try onboarding again` is the only needle that can appear here. `Starting over` follows a `y`
// and `no answer in 120s` a timeout — the answers file says `n`, instantly — and `This was attempt`
// needs attempt >= 2, which the pinned ARGUS_ONBOARD_ATTEMPT=1 rules out. They were dropped as
// unreachable, not as wrong.
func d48MustBeTheGatesOwnFailureBlock(t *testing.T, r harnessRun, what, meaning string) {
	t.Helper()
	if w := d48BlockSection(t, r, "WHAT WENT WRONG", "Exact error"); !strings.Contains(w, what) {
		t.Errorf("⛔ WHAT WENT WRONG does not carry the gate's own sentence %q:\n%s", what, w)
	}
	m := d48BlockSection(t, r, "WHAT THAT MEANS", "IS ANYTHING LEFT BEHIND?")
	if meaning != "" && !strings.Contains(m, meaning) {
		t.Errorf("⛔ WHAT THAT MEANS is not the gate's signature row (want %q):\n%s", meaning, m)
	}
	// Decision 15: a meaning does not say what the cleanup did — IS ANYTHING LEFT BEHIND? reports that,
	// and a teardown can fail.
	if strings.Contains(m, "cleaned up") {
		t.Errorf("⛔ WHAT THAT MEANS claims the onboard was `cleaned up`; IS ANYTHING LEFT BEHIND? is the one "+
			"place that reports the cleanup:\n%s", m)
	}
	if strings.Contains(r.Stdout, "Try onboarding again") {
		t.Errorf("⛔ the gate's failure offered a retry (`Try onboarding again` found) — its rows are retryable `no`: " +
			"the trap's retry re-runs onboard.sh alone against a demo SUT its teardown just stopped")
	}
	if strings.Contains(r.Stdout, "Your SUT could not be reached") {
		t.Errorf("⛔ the failure block blames the SUT (`Your SUT could not be reached`) for Path A's empty catalog")
	}
	if t.Failed() {
		_, block := fbSplit(t, r.Stdout, "8b")
		t.Logf("failure block:\n%s", block)
	}
}

// d48BlockSection returns one section of the failure block the trap printed at step 8b, whitespace-
// normalised (the block wraps at 80 columns, so a sentence can span lines).
func d48BlockSection(t *testing.T, r harnessRun, from, to string) string {
	t.Helper()
	_, block := fbSplit(t, r.Stdout, "8b")
	return strings.Join(strings.Fields(fbSection(t, block, from, to)), " ")
}

// d48MustBeTheEmptyFolderRow: the failure block is the empty-folder row's (decisions 11, 15). WHAT WENT
// WRONG no longer says "The executor registered" (false on a no-control-plane run with an id); WHAT
// THAT MEANS STARTS with the folder; WHAT YOU CAN DO NOW's step 1 is the bundle check and names
// `argus init` (TQ-4), because re-running the demo alone copies the same empty bundle again.
func d48MustBeTheEmptyFolderRow(t *testing.T, r harnessRun) {
	t.Helper()
	d48MustBeTheGatesOwnFailureBlock(t, r,
		"The demo's scenario folder held no scenario files, and the demo cannot run without them.", "")
	if w := d48BlockSection(t, r, "WHAT WENT WRONG", "Exact error"); strings.Contains(w, "The executor registered") {
		t.Errorf("⛔ WHAT WENT WRONG still says `The executor registered` for an empty folder:\n%s", w)
	}
	if m := d48BlockSection(t, r, "WHAT THAT MEANS", "IS ANYTHING LEFT BEHIND?"); !strings.HasPrefix(m, "The demo's scenario folder held no") {
		t.Errorf("⛔ WHAT THAT MEANS does not START with `The demo's scenario folder held no`:\n%s", m)
	}
	action := d48BlockSection(t, r, "WHAT YOU CAN DO NOW", "LOG")
	for _, want := range []string{"order-service-demo/scenarios-baked", "argus init"} {
		if !strings.Contains(action, want) {
			t.Errorf("⛔ WHAT YOU CAN DO NOW does not name %q:\n%s", want, action)
		}
	}
	step2 := strings.Index(action, " 2. ")
	if !strings.HasPrefix(action, "1. Check the kit's order-service-demo/scenarios-baked holds .md scenarios.") ||
		step2 < 0 || strings.Index(action, "argus init") > step2 {
		t.Errorf("⛔ step 1 of WHAT YOU CAN DO NOW is not the bundle check (check scenarios-baked, re-extract "+
			"with `argus init`) — re-running first copies the same empty bundle:\n%s", action)
	}
}

// d48MustPrintTheNoCPGuidance: with no control plane and no instance id the trap prints nothing, so the
// gate's own guidance is all the operator gets. It must come BEFORE the ERROR, say the demo is still
// running, point at the kit's bundle and at `argus init`, and give the restart COMMAND (T7: the needle
// is the command, not the word `test-orderservice.sh`, which the box's prose also contains).
func d48MustPrintTheNoCPGuidance(t *testing.T, r harnessRun) {
	t.Helper()
	errAt := d48LineIndex(r.Stdout, "ERROR:", "Path A: the scenario folder holds no scenario files")
	if errAt < 0 {
		t.Errorf("⛔ no `ERROR:` line naming `Path A: the scenario folder holds no scenario files`.\n  output:\n%s", r.Stdout)
	}
	for _, needle := range []string{"still running", "order-service-demo/scenarios-baked", "argus init",
		"bash onboarding/test-orderservice.sh"} {
		at := d48LineIndex(r.Stdout, needle)
		switch {
		case at < 0:
			t.Errorf("⛔ the gate's guidance has no line carrying %q. Without a control plane no failure "+
				"block prints, so this is all the operator gets.\n  output:\n%s", needle, r.Stdout)
		case errAt >= 0 && at > errAt:
			t.Errorf("the guidance line carrying %q prints after the ERROR line; it must come first", needle)
		}
	}
}

// seedMeasuredZero is what cloud-seed-scenarios writes when it READ the folder, offered its one file
// and the control plane took none: both counts are present, so the empty catalog is MEASURED.
const seedMeasuredZero = `{
  "failed": [
    {
      "path": "S-001.md",
      "error": "the control plane refused the write (harness)"
    }
  ],
  "instance_id": "probe-d48-zero",
  "seeded": 0,
  "total": 1
}`

// seedMeasuredZeroTransport is the same measured zero, but the reject carries a Go TRANSPORT error that
// ends in "connection refused" — the failure-signature table's SUT-unreachable key. Before the gate had
// rows of its own (01-design.md decision 10, F3) the failure block blamed the SUT for it and offered
// the retry the card calls broken on Path A.
const seedMeasuredZeroTransport = `{
  "failed": [
    {
      "path": "S-001.md",
      "error": "Post \"https://cp.example/mcp\": dial tcp 10.0.0.5:443: connect: connection refused"
    }
  ],
  "instance_id": "probe-d48-zero-dial",
  "seeded": 0,
  "total": 1
}`

// ── STRUCTURE: the final stamp runs after the LAST headline assignment ────────────────────────────
//
// The stamp used to run before the router-down, router-stale and Grafana downgrades, so of the five
// conditions that downgrade the banner the stored record followed exactly one. Any stamp placed above a
// later ALLSET_HEAD assignment records a verdict the banner may still overturn.
//
// T6: an assignment is found ANYWHERE on a non-comment line, not only at its start — a one-liner
// `[ … ] && ALLSET_HEAD="…"` below the stamp must turn this red, and a line-start anchor never saw it.
// TQ-9: `NAME=` is not the only way to assign — `printf -v ALLSET_HEAD …` and `read … ALLSET_HEAD`
// assign it too. The final stamp is found the same way. The detector is proven on mutants of the real
// file; a function body that assigns the headline (which line order cannot judge — it runs where it is
// CALLED) is TestOnboardSh_EmptyCatalog_NoFunctionBodyAssignsTheHeadline's.
// R2TQ-8: a quote is a separator too — `eval "ALLSET_HEAD=…"` / `eval 'ALLSET_HEAD=…'` assign it, and the
// quote before the name hid them. (A quoted `echo "ALLSET_HEAD=…"` now reads as one as well: the detector
// errs toward red, and onboard.sh carries no such line — the clean-file runs below prove it.)
var (
	d48AssignRE  = regexp.MustCompile(`(^|[;&|(){}\s"'])ALLSET_HEAD\+?=`)
	d48PrintfVRE = regexp.MustCompile(`(^|[;&|(){}\s"'])printf\s+-v\s*["']?ALLSET_HEAD\b`)
	// `read` with ALLSET_HEAD among its NAMES: no separator or redirection between the two, so
	// `read -r x <<<"$ALLSET_HEAD"` (a read FROM the headline) does not match.
	d48ReadRE  = regexp.MustCompile(`(^|[;&|(){}\s"'])read\s+([^;&|<>]*\s)?ALLSET_HEAD($|[\s;&|<>)"'])`)
	d48StampRE = regexp.MustCompile(`(^|[;&|(){}\s])stamp_onboarding_state\s+(\S+)`)
)

// d48AssignsHeadline reports whether a line of shell assigns ALLSET_HEAD, in any of the three forms.
func d48AssignsHeadline(l string) bool {
	return d48AssignRE.MatchString(l) || d48PrintfVRE.MatchString(l) || d48ReadRE.MatchString(l)
}

// d48StampOrder scans non-comment lines and returns the index of the last ALLSET_HEAD assignment, the
// indexes of every FINAL stamp (any state but `incomplete`), and the stamps that run before that
// assignment (the violations).
func d48StampOrder(src string) (lastAssign int, finals, violations []int) {
	lastAssign = -1
	for i, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if d48AssignsHeadline(l) {
			lastAssign = i
		}
		if m := d48StampRE.FindStringSubmatch(l); m != nil && m[2] != "incomplete" {
			finals = append(finals, i)
		}
	}
	for _, f := range finals {
		if f < lastAssign {
			violations = append(violations, f)
		}
	}
	return lastAssign, finals, violations
}

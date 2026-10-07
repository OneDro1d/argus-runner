package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ── THE HARNESS (V26 gate remediation, Phase A) ──────────────────────────────────────────────────
//
// Round 8's gate failed because nine of eighteen acceptance scenarios were verified by
// strings.Contains over the SOURCE of onboard.sh / teardown.sh. My own PO document set the rule
// "every acceptance asserts on THE WORLD, never on the run's own summary" — and a file's text is
// neither. Two of the worst defects each passed a test that searched the WHOLE FILE for a string
// without checking which BRANCH it was in.
//
// So these scripts get RUN here, against stubbed docker/kubectl (tests/harness/bin), and the
// assertions are about what they DID.
//
// ⛔ The stubs never fall through to a real binary — see tests/harness/bin/_stub.sh.

type harnessRun struct {
	Stdout string
	Calls  []string // every stubbed invocation, in order: "docker\trun --rm …"
	Kit    string   // the throwaway kit the script ran from
	// RouterState is the throwaway ARGUS_ROUTER_STATE this run was given. Exposed because
	// teardown.sh runs a REAL `rm -rf` on it, so a test must be able to assert where it pointed.
	RouterState string
	// FD3 is everything the script wrote to its JSON-step side channel, captured only when a test
	// opts in via withFD3Capture. nil (not empty-slice) when capture was never enabled for this
	// run — a test that wants to assert "nothing was written" must opt in and check len(FD3)==0,
	// not rely on FD3 being absent by default.
	//
	// ⚠ PHYSICALLY FD 5, NOT FD 3 — see the capture site in runKitScriptSeeded for why. The name
	// stays FD3 because that is the channel's PURPOSE (the third, non-stdout/stderr stream a test
	// can read), not its OS-level number, which onboard.sh's own funnel.sh already claimed.
	FD3 []byte
	Err error
}

// runKitScript copies the onboarding kit into a throwaway directory and runs one of its scripts
// there, so a teardown under test can never touch the real tree's deploy/compose or logs.
// harnessFixture answers one kind of stubbed call.
//
// RC MATTERS AS MUCH AS OUT, and the first version of this harness got it wrong: the stub
// defaulted to exit 0, so teardown's `kubectl get ns <deleted-ns>` poll read "still terminating"
// forever and the script span until the bound killed it. A stub that always succeeds is not
// neutral - it is a lie with a different shape.
type harnessFixture struct {
	Match string // a substring of the argv
	Out   string
	RC    int
	Times int // 0 = unlimited; otherwise this fixture answers only this many calls
}

// runKitScript runs a kit script under a 25s bound. onboard.sh legitimately needs longer than
// teardown.sh, so use runKitScriptFor when driving it — but ALWAYS with a bound: a script that
// hangs must fail a test, never hang the suite.
func runKitScript(t *testing.T, script string, fixtures []harnessFixture, args ...string) harnessRun {
	return runKitScriptFor(t, 25*time.Second, script, fixtures, args...)
}

// harnessSeed holds files written INTO the kit before the run.
//
// Some real commands write files as a side effect that a stub cannot: cloud-login drops
// deploy/compose/cp-session.token, render-obs writes deploy/compose/promtail-config.yaml. A test
// that needs the script to get past those must supply them, and doing it EXPLICITLY per test —
// rather than always creating them in the helper — keeps a missing one an honest failure instead
// of something the harness quietly papers over.
type harnessSeed map[string]string

func runKitScriptFor(t *testing.T, bound time.Duration, script string, fixtures []harnessFixture, args ...string) harnessRun {
	return runKitScriptSeeded(t, bound, script, nil, fixtures, args...)
}

// Two hooks, so a test can describe a machine the default sandbox does not.
//
// The default sandbox is an idealised one: every file carries 0755 and no terminal exists. Real
// operators run on neither. A kit extracted from a tarball without permissions, or copied through a
// zip, or living on a Windows filesystem, has NO EXECUTE BIT -- and a failed onboard is answered by
// somebody sitting at a prompt. Both are ordinary, and neither was reachable from a test.
var (
	harnessExtraEnv   []string
	harnessPostCopy   func(kit string)
	harnessBareName   bool
	harnessRouterSeed map[string]string
	harnessCaptureFD3 bool
)

func withHarnessEnv(t *testing.T, kv ...string) {
	t.Helper()
	prev := harnessExtraEnv
	harnessExtraEnv = append(append([]string{}, prev...), kv...)
	t.Cleanup(func() { harnessExtraEnv = prev })
}

// withBareScriptName invokes the script the way `cd onboarding && bash onboard.sh` does: cwd is the
// kit directory and $0 is a BARE FILENAME with no slash in it.
//
// That distinction is the whole of F7. `exec "$0"` on a name containing no slash does not run the
// file -- it SEARCHES $PATH, which does not contain `.`, so the retry died with 127. The default
// harness hands the script an absolute path, where the bug cannot appear at all.
// withRouterStateSeed puts files into this run's THROWAWAY router state dir before the script runs.
//
// Opt-in, because merely creating that directory flips teardown's RT_STATE_PRESENT and would move
// every other test onto a different branch. ⛔ The path is always the sandbox one — see
// TestHarness_NeverPointsAtRealRouterState for why that matters.
func withRouterStateSeed(t *testing.T, files map[string]string) {
	t.Helper()
	prev := harnessRouterSeed
	harnessRouterSeed = files
	t.Cleanup(func() { harnessRouterSeed = prev })
}

func withBareScriptName(t *testing.T) {
	t.Helper()
	prev := harnessBareName
	harnessBareName = true
	t.Cleanup(func() { harnessBareName = prev })
}

// withFD3Capture hands the script an extra open file descriptor beyond stdin/stdout/stderr and
// captures whatever lands on it, alongside the usual stdout/stderr capture. It is fd 5 at the OS
// level (see the capture site for why fd 3 itself is unusable) — the name describes what the
// channel is FOR: the third stream a test can read, distinct from stdout and stderr.
//
// Opt-in, for the same reason withRouterStateSeed is: a run that never asked for it must look
// EXACTLY like a run under the harness today — no extra descriptor open at all — so a test proving
// "inert unless opted in" is comparing a real absence, not a capture that happened to stay empty.
func withFD3Capture(t *testing.T) {
	t.Helper()
	prev := harnessCaptureFD3
	harnessCaptureFD3 = true
	t.Cleanup(func() { harnessCaptureFD3 = prev })
}

func withHarnessPostCopy(t *testing.T, fn func(kit string)) {
	t.Helper()
	prev := harnessPostCopy
	harnessPostCopy = fn
	t.Cleanup(func() { harnessPostCopy = prev })
}

func runKitScriptSeeded(t *testing.T, bound time.Duration, script string, seed harnessSeed, fixtures []harnessFixture, args ...string) harnessRun {
	t.Helper()

	kit := t.TempDir()
	src := filepath.Join("..", "..", "onboarding")
	dst := filepath.Join(kit, "onboarding")
	if err := copyTreeForTest(src, dst); err != nil {
		t.Fatalf("copy kit: %v", err)
	}
	// onboard.sh installs from REPO/skills; without it step 7 dies for the wrong reason.
	if err := copyTreeForTest(filepath.Join("..", "..", "skills"), filepath.Join(kit, "skills")); err != nil {
		t.Fatalf("copy skills: %v", err)
	}
	// teardown.sh resolves REPO as HERE/.., and looks for credentials under REPO/deploy/compose.
	if err := os.MkdirAll(filepath.Join(kit, "deploy", "compose"), 0o755); err != nil {
		t.Fatalf("mkdir compose: %v", err)
	}

	for rel, body := range seed {
		p := filepath.Join(kit, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("seed mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("seed write: %v", err)
		}
	}

	fxDir := filepath.Join(t.TempDir(), "fixtures")
	if err := os.MkdirAll(fxDir, 0o755); err != nil {
		t.Fatalf("mkdir fixtures: %v", err)
	}
	for i, fx := range fixtures {
		name := fmt.Sprintf("%02d", i) // sorted order == the order written here
		write := func(ext, body string) {
			if err := os.WriteFile(filepath.Join(fxDir, name+ext), []byte(body), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
		}
		write(".match", fx.Match)
		write(".out", fx.Out)
		write(".rc", strconv.Itoa(fx.RC))
		if fx.Times > 0 {
			write(".times", strconv.Itoa(fx.Times))
		}
	}

	if harnessPostCopy != nil {
		harnessPostCopy(kit)
	}
	// 🚨 THE SCRIPTS DELETE THINGS THE STUBS DO NOT COVER. This is the containment for that.
	//
	// The stub PATH intercepts docker, kubectl and curl. It does NOT intercept `rm`. teardown.sh runs
	// a REAL `rm -rf "$ROUTER_STATE"` (teardown.sh:1316), and router_state_dir() resolves to the
	// OPERATOR'S OWN per-user path — $LOCALAPPDATA/argus/router on Windows.
	//
	// ⛔ THIS ALREADY HAPPENED. A harness run on 2026-08-21 deleted this machine's real router state:
	// identity.key, state.json and tunnels/ for seven live instances. The fail-closed stub guard above
	// covers the three binaries I thought of; it never occurred to me that the script's own `rm` was
	// outside it. A sandbox that only sandboxes what you remembered is not a sandbox.
	// ⚠ NAMED `argus/router` UNDER A TEMP ROOT, not something arbitrary. update.sh's own test
	// asserts the path it passes LOOKS like a host state dir (…argus/router) rather than docker's
	// VM-side /mnt/… report — feeding that back would bind an empty dir and strand every folder. The
	// sandbox has to keep that shape or it breaks a real assertion.
	routerState := filepath.Join(t.TempDir(), "argus", "router")
	for rel, body := range harnessRouterSeed {
		p := filepath.Join(routerState, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("router-state seed mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("router-state seed write: %v", err)
		}
	}
	logPath := filepath.Join(t.TempDir(), "calls.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}

	stubs, err := filepath.Abs(filepath.Join("..", "..", "tests", "harness", "bin"))
	if err != nil {
		t.Fatalf("abs stubs: %v", err)
	}
	// FAIL CLOSED. filepath.Abs succeeds for a path that does not exist, and prepending a
	// nonexistent directory to PATH is a NO-OP - docker and kubectl then resolve to the REAL
	// binaries and these tests run teardown.sh against the operator's actual daemon, actual router
	// state and current kube context.
	//
	// That was not hypothetical. `.gitignore` had a bare `bin/`, which matches tests/harness/bin at
	// any depth, so `git add tests/harness` added NOTHING and the suite was green only because four
	// untracked files happened to exist on one laptop. Any clean clone - CI, a colleague, a release
	// build - would have gone live.
	//
	// A missing stub must STOP the test, never let it proceed against something real.
	// 🚨 AND THE CHECK HAS TO BE THE ONE THAT MATTERS. This was os.Stat, which answers "does the
	// file exist". The question is "will bash resolve THIS file from PATH", and the two came apart:
	// the stubs were committed 100644, and bash's PATH search SKIPS a non-executable file and falls
	// through to the REAL binary. core.fileMode=false on the author's Windows host, so nothing local
	// ever showed it, and os.Stat passed the entire time.
	//
	// Running the stub the way the scripts under test run it covers every variant at once: missing,
	// not executable, PATH not applied, or shadowed by a real binary. A real docker given this
	// argument does not print the sentinel.
	for _, tool := range []string{"docker", "kubectl", "curl"} {
		probe := exec.Command("bash", "-c", tool+" __harness_selfcheck__")
		probe.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
		probeOut, perr := probe.CombinedOutput()
		if perr != nil || !strings.Contains(string(probeOut), "__ARGUS_HARNESS_STUB_OK__") {
			t.Fatalf("the harness stub %q did not answer from %s (err %v).\n"+
				"  REFUSING TO RUN: bash did not resolve the stub, so PATH falls through and this test "+
				"would drive the REAL docker/kubectl against your actual daemon, router state and current "+
				"kube context.\n"+
				"  Causes seen: the file is missing; it is committed NON-EXECUTABLE (git ls-files -s should "+
				"show 100755, not 100644); or .gitignore is eating tests/harness/bin/.\n"+
				"  it said: %s", tool, stubs, perr, strings.TrimSpace(string(probeOut)))
		}
	}

	// ⛔ BOUNDED. A script that hangs must FAIL a test, never hang the suite — the same rule the
	// gate applied to VR8-R2's auto-cleanup, now applied to the tooling that checks it. Without this
	// the first run of this harness sat past the 2-minute tool timeout with no output at all.
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(dst, script)}, args...)...)
	if harnessBareName {
		cmd = exec.CommandContext(ctx, "bash", append([]string{script}, args...)...)
		cmd.Dir = dst
	}
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HARNESS_LOG="+logPath,
		"HARNESS_FIXTURES="+fxDir,
	)
	cmd.Env = append(cmd.Env, "ARGUS_ROUTER_STATE="+filepath.ToSlash(routerState))
	cmd.Env = append(cmd.Env, harnessExtraEnv...)
	// ⛔ AND PROVE THE OVERRIDE IS THE ONE THAT WINS. A later entry in the slice takes precedence, so
	// a test that set its own via withHarnessEnv would silently re-point the delete at something real.
	for _, kv := range harnessExtraEnv {
		if strings.HasPrefix(kv, "ARGUS_ROUTER_STATE=") {
			t.Fatalf("a test overrode ARGUS_ROUTER_STATE (%q). teardown.sh runs a REAL rm -rf on it; "+
				"it must stay inside the test's temp dir.", kv)
		}
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	var runErr error
	var fd3Bytes []byte
	if harnessCaptureFD3 {
		// exec.Cmd.ExtraFiles maps its slice 1:1 onto the child's fd 3, 4, 5, … — index 0 is fd 3,
		// index 1 is fd 4, index 2 is fd 5.
		//
		// ⛔ THE CAPTURE LANDS ON FD 5, NOT FD 3, AND THAT IS DELIBERATE. onboarding/lib/funnel.sh
		// already owns fd 3 and fd 4 before ANY of json_step's call sites can run: funnel_start
		// (invoked at onboard.sh:202, long before the earliest json_step call site) does
		// `exec 3>&1 4>&2` to save the real stdout/stderr, then repoints 1/2 at the scrub|tee
		// transcript pipeline. A write to `>&3` from inside json_step therefore lands on the
		// PRE-FUNNEL real stdout — exactly the interleaved-with-the-human-transcript channel this
		// mechanism exists to avoid, and unscrubbed besides. fd 8/9 are likewise taken (the TTY
		// prompt sink and its read side). fd 5 is the first free one, so indices 0 and 1 here are
		// throwaway padding for fd 3/4 — bash's own `exec 3>&1 4>&2` overwrites them immediately
		// regardless of what arrives there, so what they point at never matters.
		fd3r, fd3w, perr := os.Pipe()
		if perr != nil {
			t.Fatalf("open fd3 pipe: %v", perr)
		}
		pad, perr := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if perr != nil {
			t.Fatalf("open %s: %v", os.DevNull, perr)
		}
		cmd.ExtraFiles = []*os.File{pad, pad, fd3w} // fd 3, fd 4 (padding), fd 5 (the real capture)
		if serr := cmd.Start(); serr != nil {
			fd3r.Close()
			fd3w.Close()
			pad.Close()
			t.Fatalf("start %s: %v", script, serr)
		}
		fd3w.Close() // our copy — the child (and only the child) now holds the writable end
		pad.Close()
		readDone := make(chan struct{})
		go func() {
			fd3Bytes, _ = io.ReadAll(fd3r)
			close(readDone)
		}()
		runErr = cmd.Wait()
		<-readDone
		fd3r.Close()
	} else {
		runErr = cmd.Run()
	}
	if ctx.Err() != nil {
		t.Fatalf("%s did not finish within %s under the harness — the stubs answer instantly, so a "+
			"script that still blocks is waiting on something real.\n  partial output:\n%s",
			script, bound, out.String())
	}

	blob, _ := os.ReadFile(logPath)
	var calls []string
	for _, l := range strings.Split(string(blob), "\n") {
		if strings.TrimSpace(l) != "" {
			calls = append(calls, l)
		}
	}
	return harnessRun{Stdout: out.String(), Calls: calls, Kit: kit, RouterState: routerState, FD3: fd3Bytes, Err: runErr}
}

// mustHaveCalled fails the test when the run never reached the code path under test.
//
// ⛔ THIS EXISTS BECAUSE THIS HARNESS LIED TO ME ON ITS FIRST RUN. A `get ns` fixture returning
// NotFound sent teardown down the "namespace already gone" branch, so cloud-deregister was never
// invoked at all — and the test asserting "it must not claim the name is free" PASSED, while the
// defect it was written for was still present and reachable. A green test that never executed its
// subject is worse than a red one.
//
// Every harness test asserts the path was TAKEN before it asserts anything about the outcome.
func (r harnessRun) mustHaveCalled(t *testing.T, needle string) {
	t.Helper()
	for _, c := range r.Calls {
		if strings.Contains(c, needle) {
			return
		}
	}
	t.Fatalf("the run never invoked %q, so it did not reach the path under test — this result proves"+
		" NOTHING.\n  calls made (%d):\n    %s\n  output:\n%s",
		needle, len(r.Calls), strings.Join(r.Calls, "\n    "), r.Stdout)
}

func copyTreeForTest(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, b, 0o755)
	})
}

// ── PHASE A ACCEPTANCE: the harness must REPRODUCE the gate's findings before any fix ────────────
//
// If it cannot, it is not a harness — it is another instrument that agrees with me.

// blankVerification matches the completeness box's verification line with NOTHING after it.
var blankVerification = regexp.MustCompile(`(?m)^.*verification:[ \t]*$`)

// assertTranscriptIsComplete checks that the transcript file did not lose its TAIL.
//
// ⚠ READ THIS BEFORE TRUSTING IT: THIS DOES **NOT** COVER THE FUNNEL DRAIN.
//
// It was added to, and MEASURED NOT TO. Removing the `wait` from _drain_funnel and running this
// three times caught nothing (0/3), because the harness reads stdout through a Go pipe and
// cmd.Wait() does not return until every writer closes -- including the orphaned tee. The harness
// STRUCTURALLY CANNOT reproduce the race; the race needs stdout to be a FILE or a terminal.
//
// What it DOES assert, which is still worth having: the transcript ends where stdout ends.
// _drain_funnel is the last statement before `exit`, so the closing `transcript:` line goes THROUGH
// the funnel and must land in both. That catches a truncating or crashing scrub, and a funnel wired
// to the wrong file.
//
// ✅ THE DRAIN IS NOW TESTED ELSEWHERE — this comment used to say it was not, and was left stale by
// the very next commit. tests/onboarding/funnel-keeps-its-tail.test.sh drives
// onboarding/lib/funnel.sh directly with a large producer and a REDIRECTED FILE, which is where the
// race lives; its second case re-measures the failure every run, so a green first case is only
// evidence when the failure has just been demonstrated.
//
// This assertion stays because it covers something that one does not: that the transcript ends where
// stdout ends, which catches a truncating scrub or a funnel wired to the wrong file.
func assertTranscriptIsComplete(t *testing.T, r harnessRun) {
	t.Helper()
	logs, gerr := filepath.Glob(filepath.Join(r.Kit, "logs", "onboard-*.log"))
	if gerr != nil || len(logs) == 0 {
		t.Fatalf("no transcript was written, so its completeness cannot be judged (glob err %v)", gerr)
	}
	blob, rerr := os.ReadFile(logs[len(logs)-1])
	if rerr != nil {
		t.Fatalf("read transcript: %v", rerr)
	}
	lastLine := func(s string) string {
		lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.TrimSpace(lines[i]) != "" {
				return strings.TrimSpace(lines[i])
			}
		}
		return ""
	}
	wantTail, gotTail := lastLine(r.Stdout), lastLine(string(blob))
	if wantTail != gotTail {
		t.Errorf("⛔ THE TRANSCRIPT LOST ITS TAIL. The run printed %q last; the transcript ends at "+
			"%q.\n"+
			"  The funnel is a process substitution the shell does not wait for, so at exit the "+
			"scrub|tee pipeline is still draining and dies with the script. The tail is where a failed "+
			"run says WHY it failed — which is the whole of VR8-R1, and the reason auto-cleanup was "+
			"acceptable at all.", wantTail, gotTail)
	}
}

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// V29-008 / VR11-F1 — A FAILED ONBOARD ENDS WITH A BLOCK THAT NAMES THE FAILURE, ITS FAULT CLASS AND
// THE ACTION, AND NOTHING ELSE.
//
// ── THE DEFECT (measured 2026-09-07 on the owner's own orderservice-compose run) ──────────────────
//
// Docker refused the executor container at step 8b. The operator saw the raw error twice, then
// `ONBOARDING FAILED at step 8b.`, then ~80 lines of teardown output carrying four message groups
// that are right for a DELIBERATE teardown and wrong here ("had NO registration", "could not find
// out WHERE … was onboarded from", "the registration is still on the control plane", the
// completeness box), then a retry prompt that held the terminal for 120 s, then generic advice
// ("re-read the onboarding instructions, check your system under test, or contact support"). The
// cause was ~100 lines up, in Docker's words, never restated. The owner: "it is hard for user to
// understand what happened."
//
// ── RUN, NOT READ ────────────────────────────────────────────────────────────────────────────────
//
// onboard.sh is RUN under the harness (stubbed docker/kubectl/curl) to a forced failure at step 8b
// (pattern: TestFailedOnboardCleansUpWithoutEverNeedingABrowser, scrub_sh_test.go:316, and the
// compose + control-plane fixture set of TestHarness_TheBannerNeverSaysAllSetOverAnEmptyCatalogue).
// The assertions are on the run's stdout and on the transcript file it wrote. Two cases that need a
// state the harness cannot produce (a cleanup that did not finish; no transcript at all) lift the
// block's own function out of onboard.sh by its landmarks and run it — the way
// onboard_cleanup_branch_test.go runs the enrollment arm.

const fbInst = "orderservice-compose"

// fbDockerErr is the measured error, verbatim, as the docker CLI printed it (V29-008 "The trigger").
const fbDockerErr = `Error response from daemon: invalid mount config for type "bind": invalid mount path: 'C:/Users/api/argus-kits/orderservice-compose/deploy/compose/identity.orderservice-compose.key' mount path must be absolute`

const fbBindPath = `C:/Users/api/argus-kits/orderservice-compose/deploy/compose/identity.orderservice-compose.key`

// fbUnknownErr matches the CAPTURE pattern (`Error response from daemon`) and none of the seeded
// signatures — the whole block must still print, with no cause invented.
const fbUnknownErr = `Error response from daemon: the daemon said something nobody has catalogued yet (harness)`

func fbSandbox(t *testing.T) (prod, tests string) {
	t.Helper()
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
	mustWrite(t, filepath.Join(scen, "S-001.md"), "# a scenario\n")
	return prod, filepath.Join(sand, "test")
}

// fbFixtures carries a compose + control-plane onboard to step 8b. `first` answers the call this
// test wants to fail and is tried before everything else.
func fbFixtures(first ...harnessFixture) []harnessFixture {
	fx := append([]harnessFixture{}, first...)
	return append(fx,
		harnessFixture{Match: "docker info", Out: "Server Version: 99.0\n"},
		harnessFixture{Match: "keygen", Out: "MC4CAQAwBQYDK2VwBCIEIHarnessOnlyNotARealKeyAAAAAAAAAAAAAAAA=\n"},
		harnessFixture{Match: "render-obs", Out: `{"sut_network":"probe_default","sut_project":"probe"}` + "\n"},
		harnessFixture{Match: "preflight-auth", Out: `{"verdict": "ok"}` + "\n"},
		harnessFixture{Match: "router wire", Out: `{"port": 9765, "mcp_json": "/x/.mcp.json"}` + "\n"},
		harnessFixture{Match: "cloud-login", Out: `{"workspace":"ws-probe"}` + "\n"},
		harnessFixture{Match: "cloud-check-instance", Out: `{"valid": true,"available": true,"owned_by_you": false,"stale": false}` + "\n"},
		harnessFixture{Match: "cloud-executor-status", Out: `{"registered": true,"poll_accepted": true}` + "\n"},
		// The auto-cleanup's de-register (compose: machine identity, no sign-in). At 8b the executor never
		// registered, so the control plane answers not_present — the verdict the block's `No.` needs.
		harnessFixture{Match: "cloud-deregister", Out: `{"not_present": true}` + "\n"},
		harnessFixture{Match: "docker inspect -f {{.State.Status}}", Out: "running\n"},
		harnessFixture{Match: "api/health", Out: "200"},
		harnessFixture{Match: "cp.example/mcp", Out: "200"},
		harnessFixture{Match: "127.0.0.1:9765/ready", Out: "200"},
	)
}

// fbRun runs one cloud onboard of <inst> on the compose tier, with a terminal that answers <answer>
// to any prompt (a file, read the way a tty is — see lib/ask.sh).
func fbRun(t *testing.T, inst, answer string, seed harnessSeed, fx []harnessFixture) harnessRun {
	t.Helper()
	prod, tests := fbSandbox(t)
	answers := filepath.Join(t.TempDir(), "answers")
	mustWrite(t, answers, answer)
	withHarnessEnv(t, "ASK_TTY="+filepath.ToSlash(answers))
	if seed == nil {
		seed = harnessSeed{}
	}
	// Written by cloud-login and render-obs respectively; a stub cannot create files.
	seed["deploy/compose/cp-session.token"] = "odts_harnessSessionNotRealAAAAAAAAAAAAAAAAAAAA\n"
	seed["deploy/compose/promtail-config.yaml"] = "# harness placeholder\n"
	return runKitScriptSeeded(t, 170*time.Second, "onboard.sh", seed, fx,
		"--product-dir", prod, "--scenarios-dir", tests,
		"--tier", "compose", "--instance-id", inst, "--image", "img:test",
		"--control-plane", "https://cp.example")
}

// fbSplit returns the text between the banner and the block, and the block itself (from the LAST
// box top-left corner to the end of <out>).
func fbSplit(t *testing.T, out, step string) (between, block string) {
	t.Helper()
	banner := "ONBOARDING FAILED at step " + step + "."
	iB := strings.Index(out, banner)
	if iB < 0 {
		t.Fatalf("the run did not fail at step %s, so it proves nothing about the block.\n  output:\n%s", step, out)
	}
	iBox := strings.LastIndex(out, "╔")
	if iBox < 0 || iBox < iB {
		t.Fatalf("no failure block (╔ …) after the banner — the run still closes the way V29-008 measured.\n  output:\n%s", out)
	}
	return out[iB+len(banner) : iBox], out[iBox:]
}

func fbNonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// fbSection returns the block text between the heading <from> and the next heading <to>.
func fbSection(t *testing.T, block, from, to string) string {
	t.Helper()
	i := strings.Index(block, from)
	if i < 0 {
		t.Fatalf("the block has no %q section:\n%s", from, block)
	}
	rest := block[i+len(from):]
	if to == "" {
		return rest
	}
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("the block has no %q section after %q:\n%s", to, from, block)
	}
	return rest[:j]
}

func fbTranscript(t *testing.T, r harnessRun) (path, body string) {
	t.Helper()
	logs, gerr := filepath.Glob(filepath.Join(r.Kit, "logs", "onboard-*.log"))
	if gerr != nil || len(logs) == 0 {
		t.Fatalf("no transcript was written (glob err %v)", gerr)
	}
	blob, rerr := os.ReadFile(logs[len(logs)-1])
	if rerr != nil {
		t.Fatalf("read transcript: %v", rerr)
	}
	return logs[len(logs)-1], string(blob)
}

// ── S13 / S14 — two states the harness cannot produce, driven by lifting print_failure_block out
// of onboard.sh by its landmarks and RUNNING it ─────────────────────────────────────────────────────
func fbLiftedBlock(t *testing.T) string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "..", "onboarding", "onboard.sh"))
	if err != nil {
		t.Fatalf("read onboard.sh: %v", err)
	}
	src := string(blob)
	const open, close = "# ── V29-008 / VR11-F1 — THE FAILURE BLOCK", "# ── end of the failure block ──"
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatal("onboard.sh has no failure block (no `" + open + "` landmark); re-point this test deliberately")
	}
	j := strings.Index(src[i:], close)
	if j < 0 {
		t.Fatal("the failure block has no closing landmark")
	}
	return src[i : i+j]
}

func fbRunLifted(t *testing.T, env string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	lib, err := filepath.Abs(filepath.Join("..", "..", "onboarding", "lib", "failure-signatures.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "block.sh")
	// ⛔ THE SAME SHELL OPTIONS onboard.sh RUNS UNDER, and the block runs inside an EXIT trap where
	// errexit is still live: a grep that matches nothing must not abort the block half-printed.
	body := "set -euo pipefail\n. '" + filepath.ToSlash(lib) + "'\n" + env + "\n" + fbLiftedBlock(t) + "\nprint_failure_block\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("print_failure_block exited non-zero: %v\n%s", err, out)
	}
	return string(out)
}

const fbLiftedEnv = `INSTANCE_ID=lift-inst CURRENT_STEP=8b CONTROL_PLANE=https://cp.example USE_ROUTER=1
PRODUCT_DIR=/p COMPOSE_DIR=/nowhere ARGUS_TRANSCRIPT_DIR=/c/kit/logs
_FB_FAILED_CMD='compose up -d executor'
_FB_ERR_LINES='Error response from daemon: invalid mount config: mount path must be absolute'
_FB_RETRYABLE=yes _FB_MEANING='a meaning' _FB_ACTION='1. an action'
_FB_DEMO_REC='' _FB_DEMO_ACTED=0
`

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ── THE HARNESS FOR `argus up` ITSELF ────────────────────────────────────────────────────────────
//
// cmd/argus's existing harness (harness_test.go) runs onboard.sh/teardown.sh as the SCRIPT under
// test, one process away. Here the CLI verb is the thing under test, and it in turn execs onboard.sh
// as ITS OWN child — so these helpers run cmdUp IN-PROCESS (mirroring how main() would call it) while
// still routing onboard.sh's docker/kubectl/curl calls through the SAME stubbed PATH
// (tests/harness/bin) that harness_test.go uses. Never asserted against onboard.sh's source — only
// against what a REAL run of it, under `up`, actually did.

// upFixtureKit copies the canonical fixture (cmd/argus/testdata/up-json-transcript) into a
// fresh sandbox and returns the ABSOLUTE product/scenarios paths.
//
// ⛔ COPIED, NOT USED IN PLACE. onboard.sh's own install_skills/register/wire_folder
// (onboard.sh:2760-2850) WRITE into --product-dir (.claude/, .mcp.json, .argus-skill-backups/) — a
// checked-in fixture given directly to a real run would be dirtied by the first green test.
func upFixtureKit(t *testing.T) (productDir, scenariosDir string) {
	t.Helper()
	sand := t.TempDir()
	src := filepath.Join("testdata", "up-json-transcript")
	if err := copyTreeForTest(filepath.Join(src, "product"), filepath.Join(sand, "product")); err != nil {
		t.Fatalf("copy fixture product dir: %v", err)
	}
	if err := copyTreeForTest(filepath.Join(src, "scenarios"), filepath.Join(sand, "scenarios")); err != nil {
		t.Fatalf("copy fixture scenarios dir: %v", err)
	}
	pd, err := filepath.Abs(filepath.Join(sand, "product"))
	if err != nil {
		t.Fatalf("abs product dir: %v", err)
	}
	sd, err := filepath.Abs(filepath.Join(sand, "scenarios"))
	if err != nil {
		t.Fatalf("abs scenarios dir: %v", err)
	}
	return pd, sd
}

// upWriteFixtures writes harnessFixture entries the same way runKitScriptSeeded does (harness_test.go),
// factored out here because that logic lives inside a function this file has no reason to call (it
// runs cmdUp in-process, not a bare script) — same file format, same stub reads it.
func upWriteFixtures(t *testing.T, fixtures []harnessFixture) string {
	t.Helper()
	fxDir := filepath.Join(t.TempDir(), "fixtures")
	if err := os.MkdirAll(fxDir, 0o755); err != nil {
		t.Fatalf("mkdir fixtures: %v", err)
	}
	for i, fx := range fixtures {
		name := fmt.Sprintf("%02d", i)
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
	return fxDir
}

// upPrepareHarness builds a throwaway kit (onboarding/ + skills/ + deploy/compose, exactly like
// runKitScriptSeeded), points PATH at the SAME stubbed docker/kubectl/curl harness_test.go uses (with
// the identical fail-closed self-check — a missing/non-executable stub must stop the test, never let
// it fall through to a real binary), and chdirs the test process into the kit so `up`'s own relative
// "onboarding/onboard.sh" resolves. Returns the kit's absolute path.
func upPrepareHarness(t *testing.T, fixtures []harnessFixture) (kit string) {
	t.Helper()

	stubs, err := filepath.Abs(filepath.Join("..", "..", "tests", "harness", "bin"))
	if err != nil {
		t.Fatalf("abs stubs: %v", err)
	}
	for _, tool := range []string{"docker", "kubectl", "curl"} {
		probe := exec.Command("bash", "-c", tool+" __harness_selfcheck__")
		probe.Env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, perr := probe.CombinedOutput()
		if perr != nil || !strings.Contains(string(out), "__ARGUS_HARNESS_STUB_OK__") {
			t.Fatalf("harness stub %q did not answer from %s (err %v): REFUSING to run `up` against a "+
				"real docker/kubectl/curl. it said: %s", tool, stubs, perr, strings.TrimSpace(string(out)))
		}
	}

	kit = t.TempDir()
	if err := copyTreeForTest(filepath.Join("..", "..", "onboarding"), filepath.Join(kit, "onboarding")); err != nil {
		t.Fatalf("copy onboarding: %v", err)
	}
	if err := copyTreeForTest(filepath.Join("..", "..", "skills"), filepath.Join(kit, "skills")); err != nil {
		t.Fatalf("copy skills: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(kit, "deploy", "compose"), 0o755); err != nil {
		t.Fatalf("mkdir compose: %v", err)
	}

	fxDir := upWriteFixtures(t, fixtures)
	logPath := filepath.Join(t.TempDir(), "calls.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create log: %v", err)
	}
	routerState := filepath.Join(t.TempDir(), "argus", "router")

	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HARNESS_LOG", logPath)
	t.Setenv("HARNESS_FIXTURES", fxDir)
	t.Setenv("ARGUS_ROUTER_STATE", filepath.ToSlash(routerState))

	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(kit); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(prevWD); err != nil {
			t.Fatalf("chdir back: %v", err)
		}
	})
	return kit
}

// upRun calls cmdUp IN-PROCESS, capturing everything this process's own os.Stdout receives (the
// --json protocol in JSON mode, onboard.sh's pass-through transcript otherwise) and feeding stdin
// from the given string. Sequential only — it swaps process-wide os.Stdout/os.Stdin, exactly the
// same trade harness_test.go's own env/cwd hooks already make for this package's tests.
func upRun(t *testing.T, stdin string, args ...string) (stdout string, code int) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	prevStdout := os.Stdout
	os.Stdout = outW
	outCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(outR)
		outCh <- string(b)
	}()

	prevStdin := os.Stdin
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	os.Stdin = inR
	go func() {
		io.WriteString(inW, stdin)
		inW.Close()
	}()

	code = cmdUp(args)

	outW.Close()
	os.Stdout = prevStdout
	os.Stdin = prevStdin
	inR.Close()
	return <-outCh, code
}

// upJSONFixtures builds on the fixture combination TestOnboardJSONStep_StepStart_EmitsStartOnFD3 and
// TestOnboardJSONStep_Disabled_NoFD3Output already run onboard.sh with (onboard_json_step_hook_test.go).
// Those tests pin --image explicitly (IMAGE_PINNED=1), which skips onboard.sh's own image-variant
// auto-selection (onboard.sh:1474-1513) entirely; `up` does not add an --image flag in this pass, so
// that selection DOES run here and — with no "select-image" fixture answering it — dies at step 2b/8
// refusing to guess a variant.
//
// ⛔ THAT IS DELIBERATE, NOT AN OVERSIGHT: this set is kept exactly as far as steps 1/8, 2/8 and
// 2b/8 (two full "ok" inferences plus a real terminal "fail"), which is everything the tests below
// need and finishes in well under a second. A fixture answering "select-image" too reaches full
// completion, but onboard.sh's own router-readiness poll past step 8/8 costs it real wall-clock
// seconds (~35s, measured) — wasted here since none of these tests assert on anything past 2b/8.
var upJSONFixtures = []harnessFixture{
	{Match: "docker info", Out: "Server Version: 99.0\n"},
	{Match: "keygen", Out: "MC4CAQAwBQYDK2VwBCIEIHarnessOnlyNotARealKeyAAAAAAAAAAAAAAAA=\n"},
	{Match: "render-obs", Out: `{"sut_network":"probe_default","sut_project":"probe"}` + "\n"},
	{Match: "preflight-auth", Out: `{"verdict": "ok"}` + "\n"},
	{Match: "router wire", Out: `{"port": 9765, "mcp_json": "/x/.mcp.json"}` + "\n"},
}

func upBaseArgs(productDir, scenariosDir string) []string {
	return []string{
		"--config", filepath.Join(productDir, "argus-config.yaml"),
		"--scenarios-dir", scenariosDir,
		"--tier", "compose",
		"--runner-id", "probe-up-json",
	}
}

func nonBlankLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

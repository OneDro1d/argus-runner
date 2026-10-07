package main

// onboard_path_a_empty_catalog_review_sh_test.go — AC-D48 (GitHub #251), the cases the post-review
// decisions added (01-design.md decisions 5-13; pinned per 11-test-spec-after-review.md).
//
//   - the VR10 cross-check marks the import UNCONFIRMED, end to end (decision 8 / F9);
//   - a REFRESH of a live instance never tears it down, whether the import measured zero or never ran
//     (decision 7 / D48-R1 / F2 — the one that mattered);
//   - (b4 on Path A — removed by AC-D50/#308: with a CP the author token is always minted, and a run
//     without one stops at step 8b; FINDINGS);
//   - Path B without a control plane and with an empty folder is ALL SET, with a WARN (decision 12, R5);
//   - the stamp follows the router-stale and the Grafana-missing downgrades too (decision 1, T4);
//   - the HAS_SCEN probe answers as the loader does (decision 13, `-iname`).
//
// Re-pointed for round 2 (decisions 14-24, 14-test-spec-round-2.md): the UNCONFIRMED headline is exactly
// d48Unconfirmed; the refresh test adds the two empty-folder refreshes (b2, b4) and the gate's new WARN
// wording; the b4-on-Path-A absence needle is b4's current advice text (anchored by the Path B b4
// subtest); both router boxes say only what landed; the probe is lifted as a BLOCK and judged against
// onboard.LoadScenarioDir itself, including the two could-not-look cases (decision 20).
//
// Re-pointed for round 3 (decisions 25-30, 17-test-spec-round-3.md): the probe PRUNES dot-directories
// (an unreadable `.cache/` is never entered, so it neither hides a scenario nor turns an empty folder
// into "could not look"), and its marker reads `(not probed: the probe exited N)`; the empty-folder
// refreshes pin Path A's own Scenarios line; the no-control-plane run takes the sleep shim.
//
// RUN, NOT READ, exactly as onboard_path_a_empty_catalog_sh_test.go: onboard.sh is RUN under the
// harness via d48Run, and every test proves its path was TAKEN before it judges the outcome. The one
// exception is the HAS_SCEN probe, which is lifted out of onboard.sh by its own text and RUN under bash.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// d48rCheckRefresh is what cloud-check-instance answers for YOUR OWN live instance: valid, not
// available, owned by you, not stale. With the kit's env file holding a machine identity,
// resolve_instance_name takes the refresh arm and sets IS_REFRESH=1.
const d48rCheckRefresh = `{"valid": true,"available": false,"owned_by_you": true,"stale": false}`

// d48rIdentityB64 is the harness's placeholder machine identity (the same bytes the keygen fixture
// returns), written into deploy/compose/env.<id> the way an earlier onboard of this kit left it.
const d48rIdentityB64 = "MC4CAQAwBQYDK2VwBCIEIHarnessOnlyNotARealKeyAAAAAAAAAAAAAAAA="

// d48rRefresh turns a Path A + CP case into a REFRESH: the control plane says the name is yours and
// alive, and the kit's per-instance env file (onboard.sh bind_instance_paths: ENV_FILE is
// $COMPOSE_DIR/env.<instance-id> whenever --control-plane is given) already holds its identity.
func d48rRefresh(c d48Case) d48Case {
	c.kitSeed = map[string]string{
		"deploy/compose/env." + c.inst: "ARGUS_IDENTITY_KEY_B64=" + d48rIdentityB64 + "\n",
	}
	c.fx = append([]harnessFixture{{Match: "cloud-check-instance", Out: d48rCheckRefresh + "\n"}}, c.fx...)
	return c
}

// d48rMustHaveRefreshed: the run took resolve_instance_name's refresh arm (its own output line), and
// consequently used the kit's identity rather than minting one and skipped the enrollment mint.
func d48rMustHaveRefreshed(t *testing.T, r harnessRun) {
	t.Helper()
	r.mustHaveCalled(t, "cloud-check-instance")
	if !strings.Contains(r.Stdout, "re-onboarding REFRESHES it in place") {
		t.Fatalf("resolve_instance_name did not take its refresh arm (no `re-onboarding REFRESHES it in place`), "+
			"so IS_REFRESH is not set and this is not the case under test.\n  output:\n%s", r.Stdout)
	}
	if !strings.Contains(r.Stdout, "no enrollment credential needed — this is a refresh") {
		t.Fatalf("the enrollment step did not see IS_REFRESH=1.\n  output:\n%s", r.Stdout)
	}
	d48MustNotHaveCalled(t, r, "keygen", "a refresh reuses the kit's machine identity")
}

// ── THE HAS_SCEN PROBE MATCHES THE LOADER (decision 13) ───────────────────────────────────────────
//
// The loader (internal/onboard/cloud.go LoadScenarioDir) decides what the import offers; the probe
// decides whether Path A fails, so they must answer the same, layout for layout (decisions 13, 20):
// case-insensitive `.md` and README.md, a directory never a scenario whatever its name, dot-DIRECTORIES
// skipped but dot-FILES imported. Each layout is judged against the loader ITSELF (imported here), with
// a literal want beside it so a change to either side shows which one moved.
//
// The probe is a BLOCK now (decision 20): the find, its exit status, and the could-not-look arm that
// turns "the probe failed and found nothing" into a non-empty "(not probed: …)" marker. It is lifted out
// of onboard.sh whole — from `HAS_SCEN_UNPROBED=0 _hs_rc=0` to the `fi` that closes that arm — and RUN
// under the shell options onboard.sh runs under.
//
// d48ProbeFind is the round-3 find expression (decision 25): dot-directories are PRUNED — never entered,
// as the loader's SkipDir never reads them — where round 2's `-not -path '*/.*/*'` only filtered what a
// walk INTO them printed. The literal is pinned because the find shim (d48FindShim) recognises the probe
// by two of its words (`-iname *.md`, `-quit`); every behaviour is pinned by the runs below.
const d48ProbeFind = `find . -name '.?*' -type d -prune -o ! -type d -iname '*.md' ! -iname 'README.md' -print -quit`

func d48ProbeBlock(t *testing.T) string {
	t.Helper()
	lines := strings.Split(readOnboardSh(t), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "HAS_SCEN_UNPROBED=0 _hs_rc=0" {
			if start >= 0 {
				t.Fatalf("two `HAS_SCEN_UNPROBED=0 _hs_rc=0` lines in onboard.sh (:%d, :%d) — re-point this test "+
					"deliberately", start+1, i+1)
			}
			start = i
		}
	}
	if start < 0 {
		t.Fatalf("onboard.sh has no `HAS_SCEN_UNPROBED=0 _hs_rc=0` line — the probe block moved; re-point this " +
			"test deliberately")
	}
	end := -1
	for j := start + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == "fi" {
			end = j
			break
		}
	}
	if end < 0 {
		t.Fatalf("the probe block at onboard.sh:%d has no closing `fi`", start+1)
	}
	block := strings.Join(lines[start:end+1], "\n")
	if strings.Count(block, `HAS_SCEN="$(cd`) != 1 || !strings.Contains(block, "HAS_SCEN_UNPROBED=1") {
		t.Fatalf("the lifted block (onboard.sh:%d-%d) is not the probe — want ONE `HAS_SCEN=\"$(cd` and the "+
			"`HAS_SCEN_UNPROBED=1` arm:\n%s", start+1, end+1, block)
	}
	if !strings.Contains(block, d48ProbeFind) {
		t.Errorf("⛔ the probe (onboard.sh:%d-%d) is not the round-3 find (decision 25) — want\n  %s\nin:\n%s",
			start+1, end+1, d48ProbeFind, block)
	}
	return block
}

// d48ProbeResult is what the lifted probe block left behind.
type d48ProbeResult struct{ hasScen, unprobed, rc, out string }

// d48RunProbe runs the lifted probe block against <src> under `set -euo pipefail`. <as> is an optional
// command prefix (e.g. setpriv, to run it as an unprivileged user); the script is written into <dir>.
func d48RunProbe(t *testing.T, block, dir, src string, as ...string) d48ProbeResult {
	t.Helper()
	script := filepath.Join(dir, "probe.sh")
	body := "set -euo pipefail\n" +
		"SCEN_SRC=" + shq(filepath.ToSlash(src)) + "\n" +
		// Never consulted while SCEN_SRC is set; pointed at nothing so a fallback could not hide.
		"SCENARIOS_DIR=" + shq(filepath.ToSlash(filepath.Join(dir, "no-such-agent-folder"))) + "\n" +
		block + "\n" +
		"printf 'HAS_SCEN=[%s]\\n' \"$HAS_SCEN\"\n" +
		"printf 'HAS_SCEN_UNPROBED=[%s]\\n' \"$HAS_SCEN_UNPROBED\"\n" +
		"printf '_hs_rc=[%s]\\n' \"$_hs_rc\"\n"
	mustWrite(t, script, body)
	argv := append(append([]string{}, as...), "bash", script)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("⛔ the probe block exited non-zero under set -euo pipefail (%v) — in onboard.sh that aborts the "+
			"onboard:\n%s", err, out)
	}
	r := d48ProbeResult{out: string(out)}
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		val := func(p string) string { return strings.TrimSuffix(strings.TrimPrefix(l, p), "]") }
		switch {
		case strings.HasPrefix(l, "HAS_SCEN=["):
			r.hasScen = val("HAS_SCEN=[")
		case strings.HasPrefix(l, "HAS_SCEN_UNPROBED=["):
			r.unprobed = val("HAS_SCEN_UNPROBED=[")
		case strings.HasPrefix(l, "_hs_rc=["):
			r.rc = val("_hs_rc=[")
		}
	}
	return r
}

// d48MustBeUnprobed: find (or the cd before it) failed and found nothing, so the probe says it could not
// look — a NON-EMPTY marker, so the import still runs and Path A is not failed over a folder nobody read.
// Decision 25: the marker names THE PROBE, not find — the cd before it can be what failed.
func d48MustBeUnprobed(t *testing.T, r d48ProbeResult) {
	t.Helper()
	if r.unprobed != "1" || r.hasScen != "(not probed: the probe exited "+r.rc+")" || r.rc == "0" || r.rc == "" {
		t.Errorf("⛔ HAS_SCEN_UNPROBED=[%s] HAS_SCEN=[%s] _hs_rc=[%s]; want UNPROBED=[1], HAS_SCEN exactly `(not "+
			"probed: the probe exited <rc>)` and a non-zero rc — could not look is not empty\n  output: %s",
			r.unprobed, r.hasScen, r.rc, r.out)
	}
}

// d48ProbeAsNobody builds <files> (relative to a fresh SCEN_SRC; parents created 0755), locks every
// folder in <locked> (chmod 000), and runs the lifted probe as a user those folders really deny. root
// reads a chmod-000 folder anyway (CAP_DAC_OVERRIDE) — the Go tests run as root in the build container —
// so there the probe runs as uid 65534 through setpriv. PREMISES, each fatal: that user can ENTER
// SCEN_SRC (so a failure is never the cd's) and can NOT list any locked folder (so the layout is what it
// claims). It returns the probe's result, SCEN_SRC, and whether this process is that same user (so a
// caller knows whether an in-process loader call was denied like the probe or ran as root).
func d48ProbeAsNobody(t *testing.T, block string, files, locked []string) (r d48ProbeResult, src string, sameUser bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("this platform carries no POSIX modes, so a chmod-000 folder is not unreadable")
	}
	var as []string
	if os.Geteuid() == 0 {
		p, err := exec.LookPath("setpriv")
		if err != nil {
			t.Skip("running as root, which reads a chmod-000 folder anyway, and no setpriv to drop to an " +
				"unprivileged user: this layout cannot be produced here (the missing-folder case still pins UNPROBED)")
		}
		as = []string{p, "--reuid=65534", "--regid=65534", "--clear-groups"}
	}
	// Not t.TempDir(): its parent is 0700, which the unprivileged user could not even enter.
	dir, err := os.MkdirTemp("", "d48-unreadable-")
	if err != nil {
		t.Fatal(err)
	}
	src = filepath.Join(dir, "scenarios")
	t.Cleanup(func() {
		for _, l := range locked {
			_ = os.Chmod(filepath.Join(src, filepath.FromSlash(l)), 0o755)
		}
		_ = os.RemoveAll(dir)
	})
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		mustWrite(t, filepath.Join(src, filepath.FromSlash(f)), "# a scenario\n")
	}
	for _, l := range locked {
		if err := os.Chmod(filepath.Join(src, filepath.FromSlash(l)), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range locked {
		ls := append(append([]string{}, as...), "ls", filepath.Join(src, filepath.FromSlash(l)))
		if exec.Command(ls[0], ls[1:]...).Run() == nil {
			t.Skipf("the probe's user can still read the chmod-000 folder %s, so this layout cannot be produced here", l)
		}
	}
	enter := append(append([]string{}, as...), "bash", "-c", `cd "$1"`, "_", src)
	if out, err := exec.Command(enter[0], enter[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("premise: the probe's user cannot even enter %s (%v: %s), so a failure would be the cd's", src, err, out)
	}
	return d48RunProbe(t, block, dir, src, as...), src, len(as) == 0
}

// d48WalkFailsAsNobody reports whether a PLAIN walk of src (`find .`, entering every folder) fails for
// the probe's user — the premise that a locked dot-folder would break any probe that walks into it, so
// a clean answer from the real probe is its PRUNE at work, not a layout that happened to be readable.
func d48WalkFailsAsNobody(t *testing.T, src string) bool {
	t.Helper()
	var as []string
	if os.Geteuid() == 0 {
		p, err := exec.LookPath("setpriv")
		if err != nil {
			t.Fatalf("setpriv vanished between the layout and the walk: %v", err)
		}
		as = []string{p, "--reuid=65534", "--regid=65534", "--clear-groups"}
	}
	walk := append(append([]string{}, as...), "bash", "-c", `cd "$1" && find . >/dev/null 2>&1`, "_", src)
	return exec.Command(walk[0], walk[1:]...).Run() != nil
}

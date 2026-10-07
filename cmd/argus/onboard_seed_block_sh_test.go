package main

// onboard_seed_block_sh_test.go — VR10-S4-6/7/8 (V28-006), THE LAST HOP.
//
// The Go already returns every rejected path and reason. THE SHELL THROWS THEM AWAY: the seed block
// sed's `seeded` and `total` out of the JSON, subtracts them, and prints a bare count. Its own WARN
// then tells the operator to "open the web Scenarios page" to find out what failed — sending them to
// look for information the script had in hand and discarded.
//
// That is the hop the measured defect lives on. Bartek's onboard printed
//
//	successfully imported scenarios: 46 · failed to import: 4
//
// and no names, no reasons, and no way to tell that 50 had been offered.
//
// ⚠ EVIDENCE LEVEL: this EXTRACTS the seed block from onboard.sh and RUNS it under bash with a
// `docker` stub standing in for the CLI. It is not a live onboard — no container, no control plane —
// but the rendering under test is entirely the shell's, and the stub emits exactly the bytes
// cloud-seed-scenarios now writes (asserted for real in cloud_seed_reporting_test.go).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// extractSeedBlock pulls the (e) SEED block out of onboard.sh by its own landmarks, so the test does
// not rot against line numbers: from the SEED_TOKEN capture down to the `fi` that closes the chain,
// which is the line before CLOUD_ONBOARDED=1.
func extractSeedBlock(t *testing.T) string {
	t.Helper()
	lines := strings.Split(readOnboardSh(t), "\n")
	start, end := -1, -1
	for i, l := range lines {
		if start < 0 && strings.HasPrefix(strings.TrimSpace(l), `SEED_TOKEN="$(cat`) {
			start = i
			continue
		}
		if start >= 0 && strings.TrimSpace(l) == "CLOUD_ONBOARDED=1" {
			end = i // exclusive: keeps the closing `fi` on the line above
			break
		}
	}
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("could not extract the seed block (start=%d end=%d) — re-point this test deliberately", start, end)
	}
	block := strings.Join(lines[start:end], "\n")
	if !strings.Contains(block, "cloud-seed-scenarios") {
		t.Fatalf("the extracted block does not run the seed; extraction is wrong:\n%s", block)
	}
	return block
}

// runSeedBlock runs the extracted block with a `docker` stub that answers as cloud-seed-scenarios
// does: the JSON on stdout, the operator summary on stderr, and `rc` as the exit status. The block
// merges the two streams itself (2>&1), exactly as it does in production.
func runSeedBlock(t *testing.T, stdout, stderr string, rc int) string {
	t.Helper()
	return runSeedBlockWith(t, stdout, stderr, rc, "")
}

// runSeedBlockWith is runSeedBlock with extra shell declarations placed before the block — e.g.
// `IS_REFRESH=1`, which resolve_instance_name sets on a re-onboard of a live instance.
func runSeedBlockWith(t *testing.T, stdout, stderr string, rc int, decl string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	dir := t.TempDir()

	// A scenarios folder with one .md, so the block takes its import branch.
	scen := filepath.Join(dir, "scenarios")
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scen, "A.md"), []byte("# A"), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(dir, "compose")
	if err := os.MkdirAll(compose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(compose, "cp-author.token"), []byte("author-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	harness := "" +
		// onboard.sh runs under set -euo pipefail; a block that only survives a laxer shell is
		// not the block that ships.
		"set -euo pipefail\n" +
		"COMPOSE_DIR=" + shq(filepath.ToSlash(compose)) + "\n" +
		"SCENARIOS_DIR=" + shq(filepath.ToSlash(scen)) + "\n" +
		"SCEN_SRC=" + shq(filepath.ToSlash(scen)) + "\n" +
		"IMAGE=argus:test\n" +
		"CONTROL_PLANE=https://cp.example\n" +
		"INSTANCE_ID=inst-1\n" +
		"SCEN_EMPTY=0\n" +
		// AC-D48: HAS_SCEN is an INPUT to the block now — probed at the SCEN_SRC derivation, outside
		// the extracted lines, and the folder above holds A.md, which is what that probe answers. It is
		// the one of these two that the three original tests need: undeclared, `set -u` exits at the
		// block's first read (`[ -n "$HAS_SCEN" ]`) and all three fail.
		"HAS_SCEN=./A.md\n" +
		// AC-D48 round 3 (decision 27): the probe's could-not-look flag is an input too, set beside
		// HAS_SCEN at the SCEN_SRC derivation (0 when the probe looked, as it did above). The offered0 arm
		// reads it unguarded, so undeclared, `set -u` exits there (measured: `HAS_SCEN_UNPROBED: unbound
		// variable` failed AnImportThatOfferedNothingIsUnconfirmed on the round-3 code).
		"HAS_SCEN_UNPROBED=0\n" +
		// SCEN_UNCONFIRMED is declared as onboard.sh declares it at the top. The block reads it only in
		// the VR10 cross-check's third term, which none of the three original tests reaches, so without
		// it they still pass — measured 2026-09-25 on 095e0c7's copy of this file: HAS_SCEN alone, all
		// three PASS; SCEN_UNCONFIRMED alone, all three FAIL on `HAS_SCEN: unbound variable`. The echo
		// below reads it on every run, so it is declared here for the AC-D48 cases.
		"SCEN_UNCONFIRMED=0\n" +
		// AC-D48 round 2 (decision 14): WHY SCEN_EMPTY is 1 — declared as onboard.sh declares it, and
		// echoed below, so the measured kind is seen to name itself.
		"SCEN_EMPTY_WHY=\n" +
		decl + "\n" +
		"hostpath() { printf '%s' \"$1\"; }\n" +
		// the stub CLI: same streams, same exit status as the real one
		"docker() { printf '%s\\n' " + shq(stdout) + "; printf '%s\\n' " + shq(stderr) + " >&2; return " +
		strconv.Itoa(rc) + "; }\n" +
		extractSeedBlock(t) + "\n" +
		"echo \"SCEN_LINE=[$SCEN_LINE]\"\n" +
		"echo \"SCEN_EMPTY=[$SCEN_EMPTY]\"\n" +
		"echo \"SCEN_UNCONFIRMED=[$SCEN_UNCONFIRMED]\"\n" +
		"echo \"SCEN_EMPTY_WHY=[$SCEN_EMPTY_WHY]\"\n"

	script := filepath.Join(dir, "block.sh")
	if err := os.WriteFile(script, []byte(harness), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("running the extracted seed block failed: %v\n%s", err, b)
	}
	return string(b)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

// The JSON + summary a lossy seed now produces: four written, one refused by name.
const seedJSONFourOfFive = `{
  "failed": [
    {
      "path": "database/E.md",
      "error": "line 4: ID \"my scenario\" is not allowed: an id may contain only letters, digits"
    }
  ],
  "instance_id": "inst-1",
  "seeded": 4,
  "total": 5
}`

const seedStderrFourOfFive = `  database/E.md  —  line 4: ID "my scenario" is not allowed: an id may contain only letters, digits
imported 4 of 5 — 1 rejected`

// ── AC-D48: the three kinds of "none imported" that are NOT a measured-empty catalog ──────────────
//
// Each must set SCEN_UNCONFIRMED=1 and leave SCEN_EMPTY=0 (the Path A gate keys on SCEN_EMPTY, and on
// a control plane that gate tears the instance down), print ONE WARN whose first line carries both
// `WARN:` and `UNCONFIRMED`, and never say "the cloud catalog is EMPTY".
func seedMustBeUnconfirmedNotEmpty(t *testing.T, out string) (warn string) {
	t.Helper()
	if got := seedVar(t, out, "SCEN_UNCONFIRMED"); got != "1" {
		t.Errorf("⛔ SCEN_UNCONFIRMED=[%s]; want [1] — nothing measured what reached the catalog\n\noutput:\n%s", got, out)
	}
	if got := seedVar(t, out, "SCEN_EMPTY"); got != "0" {
		t.Errorf("⛔ SCEN_EMPTY=[%s]; want [0] — an unmeasured import is not a measured-empty catalog, and "+
			"on Path A SCEN_EMPTY fails the onboard and tears the instance down\n\noutput:\n%s", got, out)
	}
	n := 0
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for i, l := range lines {
		if strings.Contains(l, "WARN:") && strings.Contains(l, "UNCONFIRMED") {
			n++
			if warn == "" {
				block := []string{l}
				for _, c := range lines[i+1:] {
					if !strings.HasPrefix(c, "          ") || strings.Contains(c, "WARN:") {
						break
					}
					block = append(block, c)
				}
				warn = strings.Join(block, "\n")
			}
		}
	}
	if n != 1 {
		t.Errorf("⛔ %d lines carry both `WARN:` and `UNCONFIRMED`; want exactly ONE (the first line of the "+
			"one WARN)\n\noutput:\n%s", n, out)
	}
	if strings.Contains(out, "the cloud catalog is EMPTY") {
		t.Errorf("⛔ `the cloud catalog is EMPTY` printed on an unconfirmed import\n\noutput:\n%s", out)
	}
	return warn
}

// seedVar returns the value the harness echoed as `NAME=[value]`.
func seedVar(t *testing.T, out, name string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, name+"=[") {
			return strings.TrimSuffix(strings.TrimPrefix(strings.TrimRight(l, "\r"), name+"=["), "]")
		}
	}
	t.Fatalf("the harness never echoed %s:\n%s", name, out)
	return ""
}

func scenLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "SCEN_LINE=[") {
			return strings.TrimSuffix(strings.TrimPrefix(strings.TrimRight(l, "\r"), "SCEN_LINE=["), "]")
		}
	}
	t.Fatalf("the block never set SCEN_LINE:\n%s", out)
	return ""
}

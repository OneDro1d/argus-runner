package updatecmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// update_outcome_truth_test.go — AC-D56 (#390) and AC-D58 (#392): the update's outcome word and its record
// say what really happened. Every test here RUNS the generated script (or the rendered block) in real bash
// against the stubs of apply_exec_test.go.

func touchFlags(t *testing.T, root string, names ...string) func(PlanInput) {
	return func(PlanInput) {
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(root, n), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// ── AC-D56: AN UNDO IS JUDGED BY ITS RESTORE, NOT BY A PROBE THE HOST MAY NOT BE ABLE TO MAKE ───────────

// ── AC-D56: THE REFUSAL NAMES WHAT undo/ HOLDS, AND OFFERS ONE WAY ON THAT KEEPS THE EVIDENCE ─────────

// renderedBlockRun renders the block for a POSIX kit under root and runs it with a docker stub that logs.
func renderedBlockRun(t *testing.T, root string) (string, int, string) {
	t.Helper()
	kit := filepath.Join(root, "kits", "i1")
	if err := os.MkdirAll(kit, 0o700); err != nil {
		t.Fatal(err)
	}
	block, _ := RenderBlock(Instance{Tier: "compose", InstanceID: "i1", Image: "ghcr.io/x/exec:0.3.50", KitDir: kit})
	if block == "" {
		t.Fatal("no block rendered")
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := "#!/usr/bin/env bash\nprintf 'docker %s\\n' \"$*\" >> \"$CALLS\"\ncase \"$*\" in *'image inspect'*) echo ghcr.io/x/exec@sha256:aa ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(root, "calls.log")
	cmd := exec.Command("bash", "-c", block)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CALLS="+calls, "ARGUS_ROUTER_STATE="+filepath.Join(root, "rs"))
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code, calls
}

func TestBlock_RefusalNamesWhatUndoHoldsAndAnAcknowledgeThatKeepsTheEvidence(t *testing.T) {
	requireBash(t)
	root := t.TempDir()
	stage := filepath.Join(root, "kits", StagePrefix+"i1")
	for p, s := range map[string]string{
		filepath.Join(stage, "outcome"):                 "rollback-failed\n",
		filepath.Join(stage, "unrestored"):              "A-3\n",
		filepath.Join(stage, "undo", "dashboard.json"):  `{"d":1}`,
		filepath.Join(stage, "undo", "datasource.json"): `{"d":2}`,
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, code, calls := renderedBlockRun(t, root)
	if code == 0 || !strings.Contains(out, "REFUSED") {
		t.Fatalf("the block did not refuse (exit %d):\n%s", code, out)
	}
	for _, want := range []string{"dashboard.json", "datasource.json", "A-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, out)
		}
	}
	// never tell the operator to delete the evidence, nor to put back what the folder does not hold
	for _, bad := range []string{"Put that back by hand", "then remove", "holds what was live before it"} {
		if strings.Contains(out, bad) {
			t.Errorf("the refusal still says %q:\n%s", bad, out)
		}
	}
	mv := regexp.MustCompile(`(?m)^\s*(mv '[^']+' '[^']+')\s*$`).FindStringSubmatch(out)
	if mv == nil {
		t.Fatalf("the refusal gives no literal mv command to acknowledge it:\n%s", out)
	}
	if strings.Contains(readFile(t, calls), "docker pull") {
		t.Error("the block went on to pull after refusing")
	}
	// follow it literally: the evidence moves aside, intact, and the next paste runs
	if b, err := exec.Command("bash", "-c", mv[1]).CombinedOutput(); err != nil {
		t.Fatalf("the literal acknowledge command failed: %v\n%s", err, b)
	}
	moved, _ := filepath.Glob(stage + ".*")
	if len(moved) != 1 || readFile(t, filepath.Join(moved[0], "undo", "dashboard.json")) != `{"d":1}` {
		t.Fatalf("the evidence is not kept beside the stage: %v", moved)
	}
	out2, _, _ := renderedBlockRun(t, root)
	if strings.Contains(out2, "REFUSED") {
		t.Errorf("the block still refuses after the acknowledge:\n%s", out2)
	}
	if !strings.Contains(readFile(t, calls), "docker pull") {
		t.Error("the block did not proceed after the acknowledge")
	}
}

// ── AC-D58: THE OUTCOME IS WRITTEN AFTER THE RECORD, AND IT SAYS WHEN THERE IS NO RECORD ──────────────

// ── AC-D58: THE HELPER IMAGE IS NEVER A LOCAL IMPORT ALIAS ─────────────────────────────────────────

// digestSection is the part of the rendered block that resolves $DIGEST, run against a docker stub whose
// RepoDigests answer is given.
func runDigestSection(t *testing.T, img, repoDigests string) (string, string, int) {
	t.Helper()
	block, _ := RenderBlock(Instance{Tier: "compose", InstanceID: "i1", Image: img, KitDir: "/tmp/kits/i1"})
	if block == "" {
		t.Fatal("no block rendered")
	}
	lines := strings.Split(block, "\n")
	var imgLine string
	start, end := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "IMG="):
			imgLine = l
		case l == `docker pull "$IMG"`:
			start = i + 1
		case strings.HasPrefix(l, "dr --mount") && start >= 0 && end < 0:
			end = i
		}
	}
	if imgLine == "" || start < 0 || end < start {
		t.Fatalf("could not find the DIGEST section in the block:\n%s", block)
	}
	script := "set -euo pipefail\n" + imgLine + "\n" + strings.Join(lines[start:end], "\n") + "\nprintf 'DIGEST=[%s]\\n' \"$DIGEST\"\n"
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o700)
	os.WriteFile(filepath.Join(root, "repodigests"), []byte(repoDigests), 0o600)
	stub := "#!/usr/bin/env bash\ncase \"$*\" in *'image inspect'*) cat \"" + filepath.Join(root, "repodigests") + "\" ;; esac\nexit 0\n"
	os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o700)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(b), script, code
}

func TestBlock_DigestIsTheRepoDigestOfTheImagesOwnRepository(t *testing.T) {
	requireBash(t)
	alias := "argus-k3d-import@sha256:1111\n"
	for _, c := range []struct{ name, img, digests, want string }{
		{"a tag, alias first", "ghcr.io/onedro1d/x:0.3.49", alias + "ghcr.io/onedro1d/x@sha256:2222\n", "ghcr.io/onedro1d/x@sha256:2222"},
		{"a registry port", "localhost:5000/x:1", alias + "localhost:5000/x@sha256:3333\n", "localhost:5000/x@sha256:3333"},
		{"a digest-pinned image", "ghcr.io/onedro1d/x@sha256:aa", alias + "ghcr.io/onedro1d/x@sha256:aa\n", "ghcr.io/onedro1d/x@sha256:aa"},
		{"a repository that only ends the same way", "ghcr.io/a/x:1", "ghcr.io/b/ghcr.io/a/x@sha256:9999\nghcr.io/a/x@sha256:4444\n", "ghcr.io/a/x@sha256:4444"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, _, code := runDigestSection(t, c.img, c.digests)
			if code != 0 || !strings.Contains(out, "DIGEST=["+c.want+"]") {
				t.Fatalf("exit %d, want DIGEST=[%s]:\n%s", code, c.want, out)
			}
		})
	}
}

func TestBlock_NoRepoDigestOfTheImagesRepositoryFailsLoudlyNamingBoth(t *testing.T) {
	requireBash(t)
	out, _, code := runDigestSection(t, "ghcr.io/onedro1d/x:0.3.49", "argus-k3d-import@sha256:1111\n")
	if code == 0 {
		t.Fatalf("the local alias was accepted as the digest:\n%s", out)
	}
	for _, want := range []string{"ghcr.io/onedro1d/x", "argus-k3d-import@sha256:1111"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not name %q:\n%s", want, out)
		}
	}
}

package main

// V30-01H — the QA HOLDOUT for Argus 0.3.30 (the design notes, §holdout).
//
// These cases were WITHHELD from every builder and from the blind gate (C:/tmp/v30/po/V30-01H-holdout.md):
// each is a state the rows do not spell out. A build that satisfies its rows and fails one of these was built
// to the cases, not to the spec. They reuse the per-unit harnesses (stubs on PATH, the extracted shell
// functions, the kit run under a bound) and never a builder's own assertion. H2 lives in internal/control
// (v30_holdout_test.go there); H9 is a fetch of the DEPLOYED first-run page and is recorded in the QA record.
//
// Committed after the 0.3.30 images were built from ee45329 as a tests-only regression guard.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// v30K3dStub is the QA's own docker stub for lib/k3d-image.sh — one node, every answer from the env.
const v30K3dStub = `#!/usr/bin/env bash
[ -n "${CALLS:-}" ] && printf '%s\n' "$*" >> "$CALLS"
case "$1" in
  ps)   echo "k3d-h-server-0" ;;
  image) case "$2" in inspect) echo "${LOCAL_ID:-}" ;; *) exit 0 ;; esac ;;
  tag|rmi) exit 0 ;;
  save) echo "TARBYTES" ;;
  exec)
    case "$*" in
      *"images import"*) cat >/dev/null; exit 0 ;;
      *"images tag"*)    exit 0 ;;
      *"name==docker.io/library/argus-k3d-import:"*)
        a="$*"; echo "${a##*name==}	x	${IMPORTED_ID:-}" ;;
      *"images ls"*)
        [ -n "${BARE_ROW:-}" ] && echo "$IMG	x	$BARE_ROW"
        [ -n "${QUAL_ROW:-}" ] && echo "docker.io/library/$IMG	x	$QUAL_ROW"
        exit 0 ;;
      *"images rm"*)     exit 0 ;;
      *"crictl inspecti"*)
        # AC-D47: answers as the node's CRI does — an id when the node holds the ref (or NODE_CFG names
        # one), else "no such image" + exit 1 (shape measured 2026-09-25 on k3d-argus).
        if [ -n "${NODE_CFG:-}" ]; then echo "$NODE_CFG"; exit 0; fi
        if [ -n "${BARE_ROW:-}${QUAL_ROW:-}" ]; then echo "sha256:cfgcfg"; exit 0; fi
        a="$*"; printf 'level=fatal msg="no such image \\"%s\\" present"\n' "${a##* }" >&2; exit 1 ;;
      *) exit 0 ;;
    esac ;;
  *) exit 0 ;;
esac
`

func v30K3dRun(t *testing.T, stubSrc, img string, env map[string]string) (out, calls string) {
	t.Helper()
	lib, err := filepath.Abs(filepath.Join("..", "..", "onboarding", "lib", "k3d-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "docker"), []byte(stubSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	callsPath := filepath.Join(t.TempDir(), "calls.log")
	script := filepath.Join(t.TempDir(), "r.sh")
	body := ". '" + filepath.ToSlash(lib) + "'\n" +
		"k3d_import_image k3d-h '" + img + "'\n" +
		"echo \"rc=$?\"\n" +
		"printf 'FAILURES[%s]\\n' \"$K3D_IMPORT_FAILURES\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", script)
	c.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"IMG="+img, "CALLS="+filepath.ToSlash(callsPath), "K3D_CRI_RETRY_SLEEP=0")
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	o, _ := c.CombinedOutput()
	b, _ := os.ReadFile(callsPath)
	return string(o), string(b)
}

// ── H11 · VR11-T1 — a docs/ file promising a future build is OUT of the guard's scope ────────────
func TestV30Holdout_H11_TheGuardNeverReadsDocs(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"lib", "docs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range kitFuturePromiseScripts {
		if err := os.WriteFile(filepath.Join(dir, s), []byte("#!/usr/bin/env bash\necho ok\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "row.md"), []byte("the fix ships in the next build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, scanned, err := kitFuturePromisesIn(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("the guard flagged a docs/ file: %+v", found)
	}
	for _, s := range scanned {
		if strings.HasPrefix(s, "docs/") {
			t.Errorf("the guard READ %s — scope is the four scripts + lib/ only", s)
		}
	}
}

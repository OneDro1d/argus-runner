// Package updatecmd renders the update BLOCK an operator pastes to update an instance's executor.
//
// ── WHY IT IS ITS OWN PACKAGE ─────────────────────────────────────────────────────────────────────
//
// VR9-C1 rule 5: there are TWO places that tell an operator how to update — the control plane's
// instance view, and the executor itself when it lands on a tier that cannot self-update. They are
// pasted into the same shell by the same person, so two shapes is two things to get wrong; and the
// second one was worse than the first, rendering a RELATIVE path (the defect VR5-U1 fixed at the
// first site) and an `<id>` PLACEHOLDER the operator had to repair (which VR5-U3 forbids outright).
//
// ⛔ ONE FUNCTION, CALLED FROM BOTH, RATHER THAN TWO IMPLEMENTATIONS AND A DRIFT TEST. A test that
// asserts two copies agree is a guard someone can delete; a single renderer cannot drift at all.
//
// It lives here rather than in internal/control because the EXECUTOR needs it too, and the executor
// must not import the control plane's store (and its database dependencies) to render a string.
package updatecmd

import (
	"strings"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// SafeHostPath reports whether an executor-supplied string may be rendered into a command a HUMAN
// pastes into a shell (round 7 gate, SEC-3).
//
// ⚠ THE RULE IS "CAN SINGLE-QUOTING EXPRESS IT?", nothing more. Inside single quotes bash treats
// every byte literally except `'` itself — so a lone quote is the only character that can end the
// quoting and start being code, and it is therefore the only one that has to be refused outright.
// Control characters go too: a newline is a SECOND COMMAND on the operator's clipboard, and a
// carriage return hides the tail of the line behind whatever redraws over it.
//
// ⛔ IT DELIBERATELY ALLOWS `;`, `$`, `&` AND SPACES. They are harmless once quoted, and a rule that
// banned them would reject `C:\Program Files\…` — an ordinary Windows kit location. A validator that
// refuses legitimate input gets switched off, and then it protects nothing.
//
// ⚠ MOVED HERE from internal/control/store so both render sites can apply it. store re-exports it, so
// its own callers are unchanged and there is still exactly one implementation.
func SafeHostPath(s string) bool {
	for _, r := range s {
		if r == '\'' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ShellQuote wraps a value for the bash line the operator pastes. Safe only because SafeHostPath has
// already refused the one byte single-quoting cannot express.
func ShellQuote(s string) string { return "'" + s + "'" }

// MSYSPath renders a stored HOST path in the form the shell the command is for can actually open.
//
// ── WHY THIS IS NOT SIMPLY "IS IT WINDOWS?" ──────────────────────────────────────────────────────
//
// There are three spellings of the same Windows directory, and this codebase already produces two of
// them on purpose (onboard.sh:30-36):
//
//	native   C:\Users\api\kit    cygpath -w   the spelling a PERSON reads; how the CP stores it
//	mixed    C:/Users/api/kit    cygpath -m   what Windows BINARIES take (docker, kubectl)
//	MSYS     /c/Users/api/kit    (the input)  what BASH takes — and the command is bash
//
// So a single is-Windows branch emitting the stored form yields a path that LOOKS right and does not
// run. Bash cannot open `C:\...` at all, and reads `C:/...` as a relative path into a directory
// literally named `C:`.
//
// The shell is IMPLIED, not guessed: onboard.sh is a bash script, so an instance that exists was
// onboarded from bash, and on Windows that is Git Bash / MSYS. There is no cmd.exe or PowerShell
// case to handle, because neither could have produced the row. (SA §0 C2.)
//
// A POSIX path is returned untouched — that tier never had this problem, and converting it would
// create one.
func MSYSPath(p string) string {
	if !hasDriveLetter(p) {
		return p // POSIX, or too short to carry a drive letter
	}
	rest := strings.ReplaceAll(p[2:], `\`, "/")
	// MSYS mounts drives LOWERCASE: /c/… and /C/… are different paths to it.
	return "/" + strings.ToLower(string(p[0])) + rest
}

// MixedPath renders a stored HOST path in the form a WINDOWS BINARY takes — `C:/Users/api/kit`, the
// `cygpath -m` spelling. That is what `docker run -v` wants on the block's init line: docker is a
// Windows binary there, and the line is prefixed MSYS_NO_PATHCONV=1 so bash does not rewrite `/out`.
// A POSIX path is returned untouched, for the same reason as MSYSPath.
func MixedPath(p string) string {
	if !hasDriveLetter(p) {
		return p
	}
	return strings.ReplaceAll(p, `\`, "/")
}

// hasDriveLetter is the ONE drive-letter rule the block's Windows/POSIX choice rests on (VR10-U1:
// "the form is chosen by the drive-letter rule updatecmd.MSYSPath already handles").
func hasDriveLetter(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	d := p[0]
	return ('a' <= d && d <= 'z') || ('A' <= d && d <= 'Z')
}

// Instance is everything the block is rendered FROM — the stored instance row, as both sites see it.
//
// The control plane fills it from `instances`; the executor fills it from the environment onboarding
// set (ARGUS_KIT_DIR_HOST, ARGUS_KUBE_CONTEXT_HOST, ARGUS_KUBECONFIG_HOST). Every string is
// EXECUTOR-WRITTEN at some point of its life and crosses into a shell on another machine — SEC-3.
//
// ⚠ There is deliberately NO `Legacy` input field. "Legacy" is a FACT DERIVED from the row (a k8s tier
// with no recorded context — see Legacy()), and a derived fact that callers must also set is the exact
// two-sites-drift this package exists to prevent: one site would compute it, the other would forget.
type Instance struct {
	Tier        string // compose | k3d | managed (federation.ValidTier) — never quoted, never free text
	InstanceID  string
	Image       string // the digest-pinned reference the block pulls, inits from, and updates to
	KitDir      string // NATIVE spelling as stored (C:\… on Windows); "" = never recorded → no block
	KubeContext string // "" on compose; "" on a k8s tier = onboarded before 0.3.29 → the placeholder
	Kubeconfig  string // "" unless onboarding was given --kubeconfig; NATIVE spelling as stored

	// ── V31-001 §G-1 (VR13-UP): THE ROLLBACK BLOCK ────────────────────────────────────────────
	//
	// The owner's design change: after a successful update the page offers a copy-able block that
	// puts the instance back, with every variable filled in. It is the SAME renderer — two would
	// drift, and the difference is three fields, not a second program.
	Rollback bool
	// RollbackToVersion is the version the instance goes BACK to; it is what `--rollback-to` names
	// and what the page shows the operator.
	RollbackToVersion string
	// TargetImage is the digest-pinned image the rollback plans TOWARDS — `previous.image`.
	//
	// ⛔ AND `Image` STAYS THE INSTALLED ONE. This is the rule that is easy to get backwards, and
	// backwards it produces a block that cannot run: 0.3.32 is the release that ADDS the `update`
	// verb, so a pre-0.3.32 image answers `unknown command "update"` and could never perform its
	// own rollback. The NEWEST image — the one that just updated the machine — runs, and the
	// previous image is what it is pointed at (SA-D19).
	TargetImage string
}

// Legacy reports whether this instance was onboarded before the cluster name was recorded: a k8s tier
// with no kube context. It is the ONLY case in which the block carries a placeholder.
func (in Instance) Legacy() bool {
	return (in.Tier == "k3d" || in.Tier == "managed") && in.KubeContext == ""
}

// Placeholder is the literal token a legacy block carries in place of the context. Angle brackets on
// purpose: an unedited paste fails LOUDLY — kubectl knows no context by that name — instead of acting
// on whatever context happens to be current, which is the VR6-S1 hazard.
const Placeholder = "<KUBE_CONTEXT>"

// RenderBlock returns THE BLOCK for this instance — the commands the operator pastes into Git Bash on
// the machine the instance was onboarded from — or "" when it cannot be rendered completely.
// placeholder is true only when the block carries `<KUBE_CONTEXT>` (Legacy()).
//
// ── THE BLOCK RUNS THE NEW PATH (V31-001 1-SHIM, V32 Release QA 2026-09-14) ─────────────────────────
//
// It used to end in `bash …/onboarding/update.sh`, which moves the executor and the router and nothing
// else — so the all-or-nothing update this release exists for could not be reached from the page, and
// Release QA had to run it by hand. It is now, inside ONE subshell (so `set -e` stops the block, never
// the operator's terminal), exactly the sequence that QA proved on both local tiers:
//
//	IMG / KIT / STAGE / [KCTX / KCFG]    every value a single-quoted literal, assigned once
//	RS = router_state_dir()              onboarding's four branches (C-5), in both spellings
//	docker pull "$IMG"; DIGEST=…         the image lands on the PC; the HOST pins its digest (P-2)
//	update discover → bash discover.sh   the machine as it is, written to $STAGE/observed.json
//	update plan                          plan.json, preflight.sh, apply.sh (apply written LAST, C-6)
//	bash preflight.sh → bash apply.sh    `untouched`, or everything moved, or everything put back
//
// <kube-args> is empty on compose; on k3d/managed they name the recorded context (and the kubeconfig,
// only when onboarding was given one). The block carries NO --control-plane and NO token (SA §0.7): the
// planner reads the control plane from the instance's own env file, and the report signs with the
// instance's identity key (C-14).
//
// ── THE ONE EXCEPTION TO "NEVER A TEMPLATE" ──────────────────────────────────────────────────────
//
// An instance onboarded before the context was recorded gets `--kube-context '<KUBE_CONTEXT>'` and the
// page adds a hint — by owner decision, for those instances only, until they are re-onboarded. The
// token is emitted as a literal, never through the value path, so SEC-3 does not refuse it.
//
// ── "" IS AN ANSWER, NOT A FAILURE ───────────────────────────────────────────────────────────────
//
// KitDir is EMPTY for every instance onboarded before it was recorded, and the executor does not
// always know it either (foldersEnvBlock omits ARGUS_KIT_DIR_HOST entirely when empty, so an
// unknown value is ABSENT rather than blank). A half-built block is worse than none: the operator
// pastes it, it fails in a way that does not mention the missing part, and the surface that printed it
// has spent its credibility. And the tempting fill-in is forbidden: product_dir is the kit's parent
// only on the Path-A demo, so deriving from it passes the demo and misdirects every BYO operator.
//
// --tier is DECLARED rather than left to update.sh's discovery: a declared tier can CONTRADICT the
// target the script resolves, and a discovered one cannot contradict anything.
//
// 🚩 SEC-3 (round 7 gate) — every value is checked by SafeHostPath and single-quoted by ShellQuote,
// because every one of them is executor-written and this string is pasted into a shell by a human, on
// the machine that holds every other instance's kit. The script path on the Windows form is
// single-quoted too (the register's sample shows double quotes there; inside double quotes `$(…)` and
// backticks are live, and SafeHostPath deliberately admits both). ⚠ REFUSED, NOT REPAIRED: stripping
// the dangerous half would leave a path that points somewhere else and still looks plausible.
func RenderBlock(in Instance) (block string, placeholder bool) {
	if !federation.ValidTier(in.Tier) {
		return "", false
	}
	if in.Image == "" || in.KitDir == "" {
		return "", false
	}
	// ⛔ THE INSTANCE ID BUILDS A PATH THE BLOCK DELETES. The stage is `<kit's parent>/.argus-update-<id>`
	// and the block's first effect is `rm -rf` on it. SafeHostPath lets `/` and `..` through (quoting is its
	// whole concern) and the control plane stores whatever a registration sends, so an id such as
	// `x/../../Documents` would point that deletion at the operator's own folders (V32 adversary gate).
	// Only a name onboarding could have produced is rendered.
	if !federation.ValidInstanceID(in.InstanceID) {
		return "", false
	}
	// ⛔ A COMMA SPLITS EVERY `--mount`. docker reads a --mount value as comma-separated key=value pairs,
	// so a kit under `C:\Users\Doe, J\…` produces a spec docker refuses with exit 125 — which dr() then
	// retries as though it were Rancher's transient fault. No block rather than one that cannot start.
	if strings.ContainsRune(in.KitDir, ',') {
		return "", false
	}
	// A HALF-BUILT ROLLBACK IS NOT RENDERED. The renderer's standing rule: a block missing a value
	// is no block at all, because an operator pastes a half-built one and something unintended
	// happens. A rollback needs both the target image and the version it names — and a target PINNED by
	// digest: `update plan` refuses any other, so a tag here is a block that fails every time it is pasted.
	if in.Rollback && (in.TargetImage == "" || in.RollbackToVersion == "" || !strings.Contains(in.TargetImage, "@sha256:")) {
		return "", false
	}
	for _, v := range []string{in.InstanceID, in.Image, in.KitDir, in.KubeContext, in.Kubeconfig,
		in.TargetImage, in.RollbackToVersion} {
		if !SafeHostPath(v) {
			return "", false
		}
	}

	// ── THE PATHS, IN BOTH SPELLINGS THE BLOCK NEEDS (see MSYSPath's table) ─────────────────────────
	// docker is a Windows binary and takes the MIXED form as a bind source; bash, and every path a verb
	// writes into a script bash will run, need the MSYS form — and each helper container mounts a path
	// AT its MSYS spelling, so the one literal is right on both sides (scripts.go renderLiterals).
	windows := hasDriveLetter(in.KitDir)
	kit := MixedPath(in.KitDir)
	slash := strings.LastIndex(kit, "/")
	if slash < 0 {
		// 1-STAGE: the staging directory sits BESIDE the kit, so the kit swap is a rename on one
		// filesystem. A kit dir with no parent has nowhere to put it — no block, not a guessed place.
		return "", false
	}
	stage := kit[:slash] + "/" + StagePrefix + in.InstanceID

	kubeLines, kubeArgs := "", ""
	if in.Tier != "compose" {
		ctx := in.KubeContext
		if in.Legacy() {
			ctx = Placeholder
			placeholder = true
		}
		// ASSIGNED ONCE, read by both verbs — so the one placeholder stays ONE token to replace.
		kubeLines = "KCTX=" + ShellQuote(ctx) + "\n"
		kubeArgs = ` --kube-context "$KCTX"`
		if in.Kubeconfig != "" {
			kubeLines += "KCFG=" + ShellQuote(MSYSPath(in.Kubeconfig)) + "\n"
			kubeArgs += ` --kubeconfig "$KCFG"`
		}
	}
	pathconv := ""
	if windows {
		pathconv = "MSYS_NO_PATHCONV=1 "
	}

	// The target: a forward update goes to the image it just pulled, pinned by the digest the HOST
	// resolved; a rollback goes to the recorded previous image while still RUNNING the installed one's
	// verbs — see Instance.TargetImage and SA-D19.
	//
	// ⛔ AC-D58 (#392): THE REPOSITORY MUST MATCH $IMG's. `{{index .RepoDigests 0}}` took the FIRST entry, and on
	// a k3d host that can be the local `argus-k3d-import@sha256:…` alias, which then became RUNNER_IMAGE and failed
	// `docker run` with "pull access denied" on every attempt. The digest is the RepoDigest whose repository is
	// $IMG's (tag and digest stripped; a registry port is not a tag), and there being none is a refusal that
	// names both.
	digestLine := `RD="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$IMG")"` + "\n" +
		`IMG_REPO="${IMG%%@*}"; case "${IMG_REPO##*/}" in *:*) IMG_REPO="${IMG_REPO%:*}" ;; esac` + "\n" +
		`DIGEST="$(echo "$RD" | awk -v r="$IMG_REPO@" '{ sub(/[[:cntrl:]]+$/, "") } index($0, r) == 1 { print; exit }')"` + "\n" +
		`[ -n "$DIGEST" ] || { echo "REFUSED: the image $IMG has no RepoDigest in the repository $IMG_REPO, so there is no pullable digest to run the helpers from. Its RepoDigests are: $(echo "$RD" | awk '{ printf "%s ", $0 }')" >&2; exit 1; }` + "\n"
	target := ` --image "$IMG" --image-digest "$DIGEST"`
	if in.Rollback {
		// ⛔ A ROLLBACK LANDS THE PREVIOUS RELEASE'S KIT, TAKEN FROM THE PREVIOUS IMAGE (V32 adversary gate).
		// `update plan` runs in the INSTALLED image (SA-D19), whose own bundle is the NEWER kit — staged
		// from there, A-1 swapped the newer kit back in, A-5 reinstalled the newer skills, A-4 re-posted the
		// newer dashboard, and the manifest recorded all of them at the older version. The previous image's
		// own `init` (present in every release) extracts its bundle, and the plan stages that instead.
		digestLine = `docker pull ` + ShellQuote(in.TargetImage) + "\n" +
			`dr --mount "type=bind,src=$STAGE,dst=$STAGE_U" ` + ShellQuote(in.TargetImage) + ` init "$STAGE_U/bundle"` + "\n"
		target = " --image " + ShellQuote(in.TargetImage) + " --image-digest " + ShellQuote(in.TargetImage) +
			" --rollback-to " + ShellQuote(in.RollbackToVersion) + ` --runner-image "$IMG" --bundle "$STAGE_U/bundle"`
	}
	who := " --instance-id " + ShellQuote(in.InstanceID) + " --tier " + in.Tier

	var b strings.Builder
	b.WriteString("(\nset -euo pipefail\n")
	b.WriteString("IMG=" + ShellQuote(in.Image) + "\n")
	b.WriteString("KIT=" + ShellQuote(kit) + "\nKIT_U=" + ShellQuote(MSYSPath(kit)) + "\n")
	b.WriteString("STAGE=" + ShellQuote(stage) + "\nSTAGE_U=" + ShellQuote(MSYSPath(stage)) + "\n")
	b.WriteString(kubeLines)
	b.WriteString(blockRouterState + "\n" + blockRouterSpellings + "\n")
	// ⛔ AC-D60 (#394): THE HELPERS RUN AS THE OPERATOR ON LINUX (see helperUserShell). The image runs USER root, and
	// on a root Docker daemon discover.sh, the staged kit and undo/ would be root-owned 0700 — unreadable to the
	// very next line, `bash "$STAGE_U/discover.sh"`, which exits 126. Only ownership changes; the modes stay 0700.
	b.WriteString(helperUserShell + "\n")
	b.WriteString(`dr() { local rc=0 i; for i in 1 2 3; do ` + pathconv + `docker run --rm ${HELPER_USER:+--user "$HELPER_USER" -e HOME=/tmp} "$@" && return 0; rc=$?; [ "$rc" = 125 ] || return "$rc"; sleep 3; done; return "$rc"; }` + "\n")
	// ⛔ A STAGE THAT ENDED `rollback-failed` IS EVIDENCE, NOT SCRATCH. Its undo/ holds the only copy of the
	// Grafana documents and target file that were live before that update, and the page still shows this
	// block for an instance that is not current — so the obvious retry would delete them first.
	//
	// ⛔ AC-D56 (#390): THE REFUSAL SAYS WHAT undo/ REALLY HOLDS. It holds the Grafana captures, targets.json,
	// skills.tsv and scenarios.path — never a kit or the env file — so it does not tell the operator to "put it
	// back". It names the files present and the steps the stage recorded as not restored (`unrestored`), and offers
	// ONE way on that keeps the evidence: move the stage aside to a timestamped sibling, then paste again. It
	// never says to delete anything.
	b.WriteString(`if [ -f "$STAGE_U/outcome" ] && grep -qx rollback-failed "$STAGE_U/outcome"; then` + "\n")
	b.WriteString(`ACK="$STAGE_U.acknowledged-$(date -u +%Y%m%dT%H%M%SZ)"` + "\n")
	b.WriteString(`UNDO_HOLDS="$(ls -1 "$STAGE_U/undo" 2>/dev/null | awk '{ printf "%s ", $0 }' || true)"` + "\n")
	b.WriteString(`NOT_RESTORED="$(cat "$STAGE_U/unrestored" 2>/dev/null | awk '{ printf "%s ", $0 }' || true)"` + "\n")
	b.WriteString(`{ echo "REFUSED: the last update of this instance ended rollback-failed, so its staging folder is kept as evidence: $STAGE_U"; ` +
		`echo "  steps recorded as NOT restored: ${NOT_RESTORED:-none recorded — read $STAGE_U/progress.json}"; ` +
		`echo "  $STAGE_U/undo holds only what was captured before that update: ${UNDO_HOLDS:-nothing}"; ` +
		`echo "  It does NOT hold the kit or the env file. Check what the instance runs now before anything else."; ` +
		`echo "  To acknowledge this and continue, keeping the evidence, run:"; ` +
		`echo "    mv '$STAGE_U' '$ACK'"; ` +
		`echo "  then paste this block again."; } >&2` + "\n")
	b.WriteString("exit 1\nfi\n")
	// ⛔ AC-D53: rollback-unreachable is a NOTE, deliberately NOT a refusal. The undo of A-3 could not complete
	// because the runtime (or the cluster API) stopped answering; every other undo ran, and the executor may be
	// serving correctly. ⛔ The NOTE carries what THAT run recorded ($STAGE/outcome-note, scripts_apply.go
	// rollback()) — which image, whether it was put back, who stopped answering — never a fixed text: one sentence
	// cannot be true for every path, and on k3d/managed it is the Kubernetes API server, not a container runtime.
	// ⛔ ONLY ITS FIRST LINE — what that run SAW. The second is that run's advice, some of it for "before re-running";
	// this block continues the moment the NOTE is printed, so the NOTE says what THIS block does instead — from the
	// block's own target, never "re-runs that update" (it may be the rollback block, or another image). "Block", not
	// "paste": `argus up` runs this same block for a command the operator typed (up_second_run.go).
	now := `echo "This block now runs the update to $IMG." >&2`
	if in.Rollback {
		now = `echo ` + ShellQuote("This block now runs the rollback to "+in.RollbackToVersion+" ("+in.TargetImage+").") + ` >&2`
	}
	b.WriteString(`if [ -f "$STAGE_U/outcome" ] && grep -qx rollback-unreachable "$STAGE_U/outcome"; then echo "NOTE: the last update of this instance ended rollback-unreachable, and the recorded version of its executor reads unknown until an update completes. What that update saw:" >&2; if [ -f "$STAGE_U/outcome-note" ]; then sed -n '1s/^/  /p' "$STAGE_U/outcome-note" >&2 || true; else echo "  (not recorded)" >&2; fi; ` + now + `; fi` + "\n")
	b.WriteString("rm -rf \"$STAGE_U\"\nmkdir -p \"$STAGE_U\"\n")
	b.WriteString(`docker pull "$IMG"` + "\n")
	b.WriteString(digestLine)
	b.WriteString(`dr --mount "type=bind,src=$STAGE,dst=$STAGE_U" --mount "type=bind,src=$RS_M,dst=$RS_U,readonly" "$IMG" update discover --stage "$STAGE_U" --kit "$KIT_U"` +
		who + ` --router-state "$RS_U"` + kubeArgs + "\n")
	b.WriteString(`bash "$STAGE_U/discover.sh"` + "\n")
	b.WriteString(`dr --mount "type=bind,src=$STAGE,dst=$STAGE_U" --mount "type=bind,src=$KIT,dst=$KIT_U,readonly" --mount "type=bind,src=$RS_M,dst=$RS_U,readonly" "$IMG" update plan --stage "$STAGE_U" --kit "$KIT_U"` +
		who + ` --observed "$STAGE_U/observed.json" --router-state "$RS_U"` + kubeArgs + target + "\n")
	b.WriteString(`bash "$STAGE_U/preflight.sh"` + "\n")
	b.WriteString(`bash "$STAGE_U/apply.sh"` + "\n)")
	return b.String(), placeholder
}

// StagePrefix names the staging directory beside the kit: <kit's parent>/.argus-update-<instance>.
const StagePrefix = ".argus-update-"

// blockRouterState is onboarding/lib/router-state.sh's router_state_dir(), inlined (C-5): the block runs
// before any kit library is on the machine in its new version, and the two MUST agree — a block that
// computed a different directory would bind an EMPTY one, and the router would come back with no folder
// table. TestRenderBlock_RouterStateIsTheLibrarysFourBranches derives the branches from the library.
const blockRouterState = `if [ -n "${ARGUS_ROUTER_STATE:-}" ]; then RS="$ARGUS_ROUTER_STATE"; elif [ -n "${LOCALAPPDATA:-}" ]; then RS="$LOCALAPPDATA/argus/router"; elif [ -n "${XDG_STATE_HOME:-}" ]; then RS="$XDG_STATE_HOME/argus/router"; else RS="$HOME/.local/state/argus/router"; fi`

// blockRouterSpellings gives the router state in the two spellings the block needs. %LOCALAPPDATA% is
// C:\… in Git Bash's environment, so it is converted at run time; on POSIX there is nothing to convert.
const blockRouterSpellings = `if command -v cygpath >/dev/null 2>&1; then RS_M="$(cygpath -m "$RS")"; RS_U="$(cygpath -u "$RS")"; else RS_M="$RS"; RS_U="$RS"; fi`

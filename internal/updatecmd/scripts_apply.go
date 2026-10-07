package updatecmd

import (
	"fmt"
	"strings"
)

// scripts_apply.go — V31-001 (VR13-UP): apply.sh, the script that does the work and can put it back.

// RenderApply writes apply.sh.
func RenderApply(p Plan, in PlanInput) string {
	var b strings.Builder
	b.WriteString(scriptHeader)
	b.WriteString(renderLiterals(p, in))
	b.WriteString(applyGuard)
	b.WriteString("OUTCOME=" + shq(successWord(in)) + "\n")
	b.WriteString(applyState)
	if in.RouterOnly {
		b.WriteString(renderRouterOnlyApply())
		return b.String()
	}
	// AC-D63 (#407): "STARTED" IS REPORTED HERE — after the preflight guard has passed (this IS an update now) and
	// before renderSteps replaces anything. ⛔ BEST-EFFORT: `|| true`, so an unreachable control plane, or an old one
	// that does not know the route, never stops the operator's update. The end is A-8b's report (renderFinish).
	b.WriteString(applyStarted)
	b.WriteString(renderStepHelpers(p, in))
	b.WriteString(renderUndoFuncs(p, in))
	// finish() is defined BEFORE the trap that calls it — the trap fires the moment the first step
	// fails, and a handler naming an undefined function is a rollback that cannot report.
	b.WriteString(renderFinish(in))
	b.WriteString(applyTrap)
	b.WriteString(renderSteps(p, in))
	b.WriteString("\n# ── the success path ──────────────────────────────────────────────────────────────\ntrap - ERR\nfinish\n")
	return b.String()
}

// renderRouterOnlyApply is apply.sh for `update plan --router-only` (C-24): A-7 under the same trap, from
// the STAGED kit's router definition and version library, with nothing recorded per instance.
//
// ⛔ COMPOSE_DIR IS THE STAGED KIT'S. helpersRouter reads `$COMPOSE_DIR/docker-compose.router.yml`; the
// override comes first so the router is brought up from the definition the new image ships, and the
// operator's kit — every instance onboarded from it — is not touched.
func renderRouterOnlyApply() string {
	return `
COMPOSE_DIR="$STAGE/kit/deploy/compose"
` + strings.Replace(helpersRouter, "@ROLLBACK@", "0", 1) + `
undo_A_7() { # move the router back to the image it ran before
` + undoBody("A-7", PlanInput{}) + `}

finish() {
  trap - ERR
  # router-only records no instance: the router is machine-wide (see router_only_test.go)
  printf '\n%s\n' "$OUTCOME"
  case "$OUTCOME" in updated) exit 0 ;; *) exit 1 ;; esac
}
` + applyTrap + `
CURRENT='A-7'
step 'A-7 — move the machine router to the new image (router-only)'
APPLIED+=('A-7')
STEP_SKIPPED=""
. "$STAGE/kit/onboarding/lib/router-version.sh"
printf '   %s\n' "$ROUTER_UPGRADE_ORDER"
move_router || false
# ⛔ A MOVE THAT DID NOT HAPPEN IS NOT "updated" (V32 adversary gate). This plan is A-7 and nothing else,
# so when move_router declines — no router running, one already newer, a port it cannot read — nothing
# moved, and the word says so. update.sh --router ended on a read-back line for the same reason.
if [ -n "$STEP_SKIPPED" ]; then printf '   skipped: %s\n' "$STEP_SKIPPED"; progress 'A-7' skipped; OUTCOME=untouched; else progress 'A-7' applied; fi

# ── the success path ──────────────────────────────────────────────────────────────
trap - ERR
finish
`
}

// successWord is the outcome when every step lands: `rolled-back-to <version>` for a deliberate
// rollback (G-1.4), so the page can tell it from a failed update's automatic one.
func successWord(in PlanInput) string {
	if in.RollbackTo != "" {
		return "rolled-back-to " + in.RollbackTo
	}
	return "updated"
}

// ⛔ THIS SCRIPT REFUSES A PREFLIGHT THAT IS NOT THIS PLAN'S. The plan's identity is
// {instance_id, image_digest, generated_at}; a preflight.json from an earlier plan checked different
// preconditions against a different machine.
const applyGuard = `
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
KEPT="$KIT.prev-$STAMP"

[ -f "$STAGE/preflight.json" ] || { printf 'no preflight.json — run preflight.sh first\n' >&2; exit 2; }
grep -q '"ok":true' "$STAGE/preflight.json" || { printf 'the preflight did not pass\n' >&2; exit 2; }
grep -q "\"image\":\"$IMAGE_DIGEST\"" "$STAGE/preflight.json" || { printf 'preflight.json is for another image\n' >&2; exit 2; }
grep -q "\"generated_at\":\"$GENERATED_AT\"" "$STAGE/preflight.json" || { printf 'preflight.json is for another plan\n' >&2; exit 2; }
`

const applyState = `
APPLIED=()
FAILED_STEP=""
STEP_SKIPPED=""

# progress <id> <state> [health] [detail] — health and detail are AC-D53's: how a health check that did not
# pass ended (unhealthy | unreachable) and the literal output that decided it. ` + "`update rehash`" + ` reads id and
# state only; the rest is the record an operator reads.
progress() {
  local extra=""
  if [ -n "${3:-}" ]; then extra=",\"health\":\"$3\",\"detail\":\"$(json_text "${4:-}")\""; fi
  printf '{"id":"%s","state":"%s"%s,"at":"%s"}\n' "$1" "$2" "$extra" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$STAGE/progress.json"
}
step() { printf '\n== %s ==\n' "$1"; }
# A step that finds, AT RUN TIME, that there is nothing it may do (no router on this machine, a port
# it cannot read) says so and is recorded ` + "`skipped`" + ` — never ` + "`applied`" + `, which would put a version
# in the manifest for something that did not move (C-15).
skip_step() { STEP_SKIPPED="$1"; }
` + verdictHelpers

// applyStarted is the one "update started" report (AC-D63, #407). Output goes to the operator's terminal as a
// single line; a failure to send is a printed note, never an exit.
const applyStarted = `
report_started || printf '   (the control plane was not told this update has started — the page will not show it as in progress)\n' >&2
`

// verdictHelpers is AC-D53 (#329): a health check has THREE outcomes, not two.
//
//	healthy      the probe completed and the answer is good
//	unhealthy    the probe completed and the answer is bad — the runtime ANSWERED, and what it said is named
//	unreachable  the probe could not be completed at all — which says NOTHING about the executor
//
// ⛔ THE VERDICT IS TAKEN OVER THE CHANNEL THE MOVE ITSELF USED. The recreate needed the Docker API (the
// Kubernetes API on k3d/managed) and nothing else, so the health reading does too. The host's published port
// was a second, independently breakable channel — measured 2026-09-29: an executor answering {"status":"ok"}
// inside its container while the host port relayed nothing, read as "unhealthy", and a correct update rolled
// back. After this change "unreachable" means the runtime itself stopped answering — which is rare.
//
// ⛔ AND "COULD NOT ASK" IS DECIDED FROM THE WORDS OF THE COMMAND THAT FAILED — never from a second question
// asked afterwards (a ` + "`docker version`" + ` that fails for its own reason would turn a GENUINE failure into an
// outage). Only the runtime's own words for "I could not be reached" count; anything else, including anything
// this list does not recognise, is a genuine failure. Getting that backwards clears the ` + "`rollback-failed`" + `
// refusal (updatecmd.go RenderBlock) for a stage whose undo/ really is the only record of what was live.
//
// The marker lines are how health_verdict_test.go tests the classifier exactly as it ships.
const verdictHelpers = `
# ── AC-D53 verdict helpers ── healthy · unhealthy · unreachable ─────────────────────────────────────────
HEALTH_STATE="" HEALTH_DETAIL=""
UNDO_STATE="" UNDO_DETAIL="" UNDO_WHO="" UNDO_AUTH=""
A3_RECREATED="" A3_CONFIRMED="" A3_FWD_HEALTH="" A3_FWD_DETAIL="" UNDO_RECREATED=""
# EXEC_CONFIRMED: the image this run last CONFIRMED the executor healthy on — the forward move's, or its undo's —
# and empty once either moves it again. A-8a says it when the record cannot be written.
EXEC_CONFIRMED=""
# A3_MOVE_STARTED: set IMMEDIATELY before the forward move's own effect (the compose recreate; on k3d and managed,
# the Deployment's image change). Empty, the
# executor was never touched, and the undo of A-3 does nothing to it (scripts_undo.go). A3_ENV_SET: the env file
# was (or began to be) rewritten. MARKER_KEPT: keep_marker saved the marker as it was before this run.
# MARKER_AT_COMMIT: a marker stood when A-8a's commit began — this run's, or an earlier run's put back; only a commit
# that has written the record removes it (record_says). MARK_AT: when this run's move began (mark_unconfirmed).
A3_MOVE_STARTED="" A3_ENV_SET="" MARKER_KEPT="" MARKER_AT_COMMIT="" MARK_AT=""
PROBE_ERR="$STAGE/probe.err"

# mark_unconfirmed — criterion (k): the record's NOT-CONFIRMED marker, written BEFORE this instance's executor is
# moved or put back. A plain file write, so it needs no container runtime — which is the point: when the runtime
# stops answering, the re-hash and the commit (they run in containers) cannot record "unknown", and the record
# they leave names the version before, for an executor that may be on the new one. While the marker stands every
# reader sees the executor unknown (manifest.go ReadManifest); "update commit" removes it once it records a known
# executor, and teardown removes it with the instance. It names the image and version this update is moving to —
# what a re-run of "argus up" targets, and the other version the plan's downgrade gate must count — and, on a
# rollback, that it was one: "argus up" cannot re-run a rollback as a forward update. And where the executor was when
# the plan read it, with the "previous" true there: what "previous" becomes if the move lands, and what it stays if not.
# at= is when the move BEGAN: the undo's own mark keeps the forward mark's time (MARK_AT), and a landed move's
# "previous" is stamped with it (manifest.go previousAfterUnconfirmed).
mark_unconfirmed() {
  MARK_AT="${MARK_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
  ( umask 077 && mkdir -p "$ROUTER_STATE/installed" && \
    { printf 'image=%s\nversion=%s\n' "$IMAGE_DIGEST" "$TARGET_VERSION"
      if [ -n "$ROLLBACK_TO" ]; then printf 'rollback_to=%s\n' "$ROLLBACK_TO"; fi
      printf 'from_version=%s\nfrom_image=%s\n' "$FROM_VERSION" "$FROM_IMAGE"
      printf 'from_previous_version=%s\nfrom_previous_image=%s\nfrom_previous_at=%s\n' "$FROM_PREVIOUS_VERSION" "$FROM_PREVIOUS_IMAGE" "$FROM_PREVIOUS_AT"
      if [ -n "$FROM_ONE_OF" ]; then printf 'from_one_of=%s\n' "$FROM_ONE_OF"; fi
      printf 'at=%s\n' "$MARK_AT"; } > "$ROUTER_STATE/installed/$INSTANCE.unconfirmed" )
}

# keep_marker / restore_marker — A MOVE THAT NEVER STARTED LEAVES THE MARKER EXACTLY AS IT WAS BEFORE THIS RUN: an
# earlier run's marker (its executor is still unconfirmed from then) stays byte for byte, and none is left where
# there was none. keep_marker runs before this run's own mark; a marker it cannot keep is a move that is not made.
keep_marker() {
  rm -f "$STAGE/undo/marker.before" || return 1
  if [ -f "$ROUTER_STATE/installed/$INSTANCE.unconfirmed" ]; then
    cp "$ROUTER_STATE/installed/$INSTANCE.unconfirmed" "$STAGE/undo/marker.before" || return 1
  fi
  MARKER_KEPT=1
}
restore_marker() {
  [ -n "$MARKER_KEPT" ] || return 0
  if [ -f "$STAGE/undo/marker.before" ]; then
    cp "$STAGE/undo/marker.before" "$ROUTER_STATE/installed/$INSTANCE.unconfirmed" || return 1
  elif [ -f "$ROUTER_STATE/installed/$INSTANCE.unconfirmed" ]; then
    rm -f "$ROUTER_STATE/installed/$INSTANCE.unconfirmed" || return 1
  fi
}

# one_line <text> — one printable line, as JSON and a terminal both take it
one_line() { printf '%s' "$1" | tr '\r\n\t' '   ' | tr -d '\000-\037' | cut -c1-400; }
json_text() { one_line "$1" | sed 's/[\\"]/\\&/g'; }

# health_note <state> <detail> — the one writer of HEALTH_STATE / HEALTH_DETAIL
health_note() { HEALTH_STATE="$1"; HEALTH_DETAIL="$(one_line "$2")"; }

# runtime_unreachable <text> — true ONLY for the container runtime's own words for "I could not be reached":
# the docker client's on each transport (unix socket · Windows named pipe · TCP), measured 2026-09-30 on
# client 29.5.3; Rancher Desktop's proxy when its engine does not answer (measured on this estate: "failed to
# connect to the backend: timed out dialing Hyper-V socket"); and Docker Desktop's proxy when its engine does
# not answer — an EMPTY-BODY 5xx, for which the moby client (docker and compose) falls back to "request returned
# <code> for API route and version …". A daemon's own error always carries a body ("Error response from daemon:
# …"), and a 404 in that sentence is an API-version mismatch the engine itself answered: both stay genuine.
# The client's sentence was measured 2026-09-30 against a stand-in proxy; Docker Desktop answering so is from
# docker/for-win#15049 and docker/for-mac#7240 — not measured on a live Docker Desktop. ⛔ BOTH SPELLINGS: moby v28
# writes the status with its number ("500 Internal Server Error"); v27 and older write http.StatusText alone
# ("Internal Server Error" — moby client/request.go:228 at v27.2.0, and for-mac#7240's own report from client 25.0.3).
# Matching only the newer one left every older docker CLI and compose ending rollback-failed, and refused.
runtime_unreachable() {
  case "$1" in
    *"Cannot connect to the Docker daemon at "*|*"failed to connect to the docker API at "*|*"error during connect: "*|*"failed to connect to the backend"*) return 0 ;;
    *"request returned 500 Internal Server Error for API route and version "*|*"request returned 502 Bad Gateway for API route and version "*|*"request returned 503 Service Unavailable for API route and version "*) return 0 ;;
    *"request returned Internal Server Error for API route and version "*|*"request returned Bad Gateway for API route and version "*|*"request returned Service Unavailable for API route and version "*) return 0 ;;
  esac
  return 1
}
# kube_unreachable <text> — the same, in kubectl's own words (measured 2026-09-30, kubectl v1.36.2). NOT a
# bare "connection refused" or "i/o timeout": an API server that ANSWERS with an admission-webhook error
# carries those words too, and it answered.
kube_unreachable() {
  case "$1" in
    *"Unable to connect to the server"*|*"The connection to the server "*" was refused"*|*"You must be logged in to the server"*) return 0 ;;
  esac
  return 1
}
# kube_refused_credential <text> — of kube_unreachable's words, the one that is an ANSWER: kubectl prints "You must be
# logged in to the server" for an HTTP 401. It stays could-not-run (the command did not run; nothing about the
# executor was observed), but its sentence names the credential — "stopped answering … once it answers" would send
# the operator to wait for a server that answered.
kube_refused_credential() {
  case "$1" in *"You must be logged in to the server"*) return 0 ;; esac
  return 1
}

# wrong_image <detail> — rung 5's own fact (scripts_steps.go executor_observe): the executor SERVES, on another image
# than the one asked for. Never "not healthy", and never "moved to" the image it is not on.
wrong_image() {
  case "$1" in "serving, but still on "*) return 0 ;; esac
  return 1
}

# probe_failed <what> — a docker command of a health reading failed: classify it from ITS OWN stderr
probe_failed() {
  local said
  said="$(cat "$PROBE_ERR" 2>/dev/null || true)"
  if runtime_unreachable "$said"; then health_note unreachable "$1: $said"
  else health_note unhealthy "$1: ${said:-docker gave no reason}"; fi
}

# container_health_log <container> — the last output of the container's own healthcheck: evidence, never a verdict.
# ⛔ The blank lines are dropped BEFORE the last line is taken: docker stores each check's output with its own
# trailing newline and the CLI adds one more, so the raw output always ENDS in a blank line and tail -1 kept
# nothing (measured 2026-09-30 on a live router: "0: ready\n\n").
container_health_log() {
  local l
  l="$(docker inspect "$1" --format '{{range .State.Health.Log}}{{.ExitCode}}: {{.Output}}{{"\n"}}{{end}}' 2>/dev/null | sed '/^[[:space:]]*$/d' | tail -1 || true)"
  if [ -n "$l" ]; then printf ' (its last check: %s)' "$(one_line "$l")"; fi
}

# health_says <what was done> <subject> — the operator's sentence for a health check that did not pass:
# what was OBSERVED, never a cause nobody measured.
health_says() {
  if [ "$HEALTH_STATE" = unreachable ]; then
    printf '   !! could not find out whether %s is healthy — %s. It may be running correctly. Rolling back because an unconfirmed update is not an accepted state.\n' "$2" "$HEALTH_DETAIL" >&2
  elif wrong_image "$HEALTH_DETAIL"; then
    # rung 5: it serves, on the wrong image — its own fact, not "not healthy: … is healthy"
    printf '   !! %s, %s is %s\n' "$1" "$2" "$HEALTH_DETAIL" >&2
  else
    printf '   !! %s, %s is not healthy: %s\n' "$1" "$2" "$HEALTH_DETAIL" >&2
  fi
}

# undo_classify <what failed> — an undo of A-3 could not complete: record WHY, from HEALTH_STATE when its
# health reading failed, else from the failing command's own words in $2. Default: a genuine failure.
undo_classify() {
  UNDO_DETAIL="$(one_line "$1")" UNDO_AUTH=""
  if [ "${3:-}" = health ]; then
    if [ "$HEALTH_STATE" = unreachable ]; then UNDO_STATE=runtime-unreachable; else UNDO_STATE=failed; fi
  elif runtime_unreachable "$2" || kube_unreachable "$2"; then UNDO_STATE=runtime-unreachable
  else UNDO_STATE=failed; fi
  if [ "$UNDO_STATE" = runtime-unreachable ] && kube_refused_credential "$1"; then UNDO_AUTH=1; fi
}
# ── end of the verdict helpers ──
`

// applyTrap is the heart of 1-ATOMIC.
//
// ⛔ IT COVERS A-1..A-7 AND STOPS (C-13). Every step that touched the machine is undone in REVERSE —
// undoing A-1 (the kit swap) before A-7 (the router) would leave the router pointed at a kit that no
// longer exists. An undo that itself fails escalates the word to `rollback-failed`.
const applyTrap = `
rollback() {
  trap - ERR
  OUTCOME=rolled-back
  printf '\n!! %s failed — undoing every applied step in reverse\n' "${FAILED_STEP:-a step}" >&2
  # ⛔ LOCAL. An undo that reads into a variable of the same name (undo_A_5's ` + "`read -r op dir s bk`" + `) would
  # overwrite the step id, and the step would be recorded "undone" under an empty id — so the re-hash kept
  # it "applied" and recorded the skills at the version that was just taken off the machine.
  local i step genuine="" unreachable="" unreachable_detail="" unreachable_who="" unreachable_auth="" said
  # AC-D56 (#390): WHICH steps were not restored, one id per line — what the page block's refusal names
  rm -f "$STAGE/unrestored"
  for (( i=${#APPLIED[@]} - 1; i >= 0; i-- )); do
    step="${APPLIED[$i]}"
    printf '   undo %s\n' "$step" >&2
    UNDO_STATE="" UNDO_DETAIL="" UNDO_WHO="" UNDO_AUTH=""
    if ! "undo_$(printf '%s' "$step" | tr -- '-' '_')"; then
      # AC-D56: every undo that did not complete is listed, whichever way it ended — an undo the runtime did not let
      # finish did not confirm its restore either, and the list is read only when the run ends rollback-failed
      printf '%s\n' "$step" >> "$STAGE/unrestored" 2>/dev/null || true
      # ⛔ AC-D53: ONLY the undo's own reading of the command that failed may say "the runtime did not answer".
      # An undo that says nothing — every undo but A-3's — or says anything else, FAILED, and arms the refusal.
      if [ "$UNDO_STATE" = runtime-unreachable ]; then
        unreachable="$unreachable $step" unreachable_detail="$UNDO_DETAIL" unreachable_who="$UNDO_WHO" unreachable_auth="$UNDO_AUTH"
        said="did not answer"
        if [ -n "$UNDO_AUTH" ]; then said="refused this machine's credential"; fi
        if [ -n "$UNDO_RECREATED" ]; then
          printf '   !! undo of %s put %s back, but %s %s before its health could be read: %s\n' "$step" "$PREV_EXECUTOR_IMAGE" "$UNDO_WHO" "$said" "$UNDO_DETAIL" >&2
        else
          printf '   !! undo of %s could not run — %s %s: %s\n' "$step" "$UNDO_WHO" "$said" "$UNDO_DETAIL" >&2
        fi
      else
        genuine="$genuine $step"
        printf '   !! undo of %s FAILED\n' "$step" >&2
      fi
    else
      progress "$step" undone
    fi
  done
  if [ -n "$genuine" ]; then
    OUTCOME=rollback-failed
  elif [ -n "$unreachable" ] && [ -z "$UNDO_RECREATED" ] && [ "$A3_FWD_HEALTH" = unhealthy ] && ! wrong_image "$A3_FWD_DETAIL"; then
    # ⛔ B1 (review of #399; the operator's rule): SEEN BROKEN, AND NOT PUT BACK. rollback-unreachable is
    # not refused because the executor "may be serving correctly"; this run SAW it unhealthy on the new image — the
    # runtime answering — and its undo's recreate / set image did not report success. That is worse than's
    # own case (back on the old image and seen unhealthy), so it is rollback-failed too: the next paste is refused and A-3
    # is listed unrestored. Before, its "look before re-running" was line 2 of the NOTE file, which the next paste never
    # prints, and the paste went straight on to the next update. (A wrong image — rung 5 — was seen SERVING: not this.)
    # ⛔ The sentence holds what this run RECORDED: last SEEN not healthy on the new image — never "it is still there": a
    # recreate that did not report success may have stopped the container, or reached the API server before it failed.
    OUTCOME=rollback-failed
    local said_who="$unreachable_who stopped answering" first="Look at it before anything else"
    if [ -n "$unreachable_auth" ]; then
      said_who="$unreachable_who refused this machine's credential"
      first="Once this machine is signed in to the cluster again, look at it before anything else"
    fi
    printf '\n!! rollback-failed — the executor was moved to %s and seen not healthy there (%s); it could not be put back because %s (%s). It was last seen not healthy on %s; which image it runs now is not known. %s: the next update block refuses until this stage is acknowledged.\n' "$IMAGE_DIGEST" "$A3_FWD_DETAIL" "$said_who" "$unreachable_detail" "$IMAGE_DIGEST" "$first" >&2
  elif [ -n "$unreachable" ]; then
    # ⛔ NOT rollback-failed, SO THE NEXT PASTE IS NOT REFUSED: the executor may be serving correctly on the new
    # image (never seen broken there), or it was put back, and a refused stage would hold the instance until someone
    # cleared it by hand. The one case seen broken AND left is B1, above.
    OUTCOME=rollback-unreachable
    # ⛔ AND EVERY FACT IN THE SENTENCE IS ONE THIS RUN RECORDED (design §11.4: each one changes what the operator
    # does next). A fixed text said "its health could not be confirmed… could not be put back… may be running
    # correctly" on every path — false after a move that WAS confirmed before a later step failed, after a new
    # image that was SEEN unhealthy, and after an undo that DID put the old image back.
    local saw put where advice once stopped
    # ⛔ A 401 IS AN ANSWER (kube_refused_credential): the words follow the failing command's own — never "stopped
    # answering … once it answers" for a server that answered and refused this machine's credential.
    once="once $unreachable_who answers" stopped="stopped answering"
    if [ -n "$unreachable_auth" ]; then
      once="once this machine is signed in to the cluster again"
      stopped="refused this machine's credential"
    fi
    if [ -n "$A3_CONFIRMED" ]; then
      saw="the executor was moved to $IMAGE_DIGEST and confirmed healthy there, then $FAILED_STEP failed"
      where="It was last seen healthy on $IMAGE_DIGEST."
    elif [ -n "$A3_RECREATED" ] && [ "$A3_FWD_HEALTH" = unhealthy ] && wrong_image "$A3_FWD_DETAIL"; then
      # ⛔ rung 5: it was seen SERVING, on another image than the one asked for — never "moved to" that image, never
      # "not healthy" there (the same collision health_says keeps apart)
      saw="the executor was recreated to move it to $IMAGE_DIGEST, and was then seen $A3_FWD_DETAIL"
      where="It was last seen $A3_FWD_DETAIL."
    elif [ -n "$A3_RECREATED" ] && [ "$A3_FWD_HEALTH" = unhealthy ]; then
      # reached only when the undo DID recreate (not recreated is B1, above), so "where" is the put-back one below
      saw="the executor was moved to $IMAGE_DIGEST, which was seen not healthy ($A3_FWD_DETAIL)"
    elif [ -n "$A3_RECREATED" ]; then
      saw="the executor was moved to $IMAGE_DIGEST and its health there could not be confirmed"
      where="It may be running correctly on $IMAGE_DIGEST."
    else
      saw="its move to $IMAGE_DIGEST was started and did not complete"
      where="Which image it runs, if any, is unknown."
    fi
    advice="Re-run this update $once — it is safe to run again."
    if [ -n "$UNDO_RECREATED" ]; then
      put="it was put back on $PREV_EXECUTOR_IMAGE, but its health there could not be confirmed because $unreachable_who $stopped ($unreachable_detail)"
      where="It should be on $PREV_EXECUTOR_IMAGE; whether it is healthy there is unknown."
    else
      put="it could not be put back because $unreachable_who $stopped ($unreachable_detail)"
    fi
    printf '\n!! rollback-unreachable — %s; %s.\n' "$saw" "$put" >&2
    printf '   %s Every other step that ran was put back. Its recorded version reads unknown until an update completes. %s\n' "$where" "$advice" >&2
    # ⛔ THE NOTE FILE: line 1 is what this run SAW, line 2 its advice. The next paste's NOTE (updatecmd.go
    # RenderBlock) prints line 1 only — that paste IS the re-run, so advice for "before re-running" would be read
    # after it began. "argus up --json" carries both lines (up_second_run.go), for whoever decides on the re-run.
    # (The one case whose advice was "look first" — seen broken and not put back — is B1, rollback-failed and refused.)
    printf '%s; %s. %s\n%s\n' "$saw" "$put" "$where" "$advice" > "$STAGE/outcome-note" 2>/dev/null || true
  fi
}
# ⛔ THE HANDLER FINISHES. Under set -e the shell exits as soon as the handler returns, so a handler
# that only rolls back leaves the outcome unprinted and unreported (C-14).
trap 'FAILED_STEP="${CURRENT:-unknown}"; rollback; finish' ERR
`

// renderSteps emits A-1..A-7.
//
// ⛔ A STEP REGISTERS FOR UNDO BEFORE ITS EFFECT, NOT AFTER. Registered after, a step that failed
// half-way — the env file rewritten and the container recreated onto an image that never came up
// healthy — was never undone: it was not yet in APPLIED, so the trap put the kit back and left the
// executor on the broken image while reporting `rolled-back`. Every undo therefore tolerates an effect
// that never started.
func renderSteps(p Plan, in PlanInput) string {
	var b strings.Builder
	for _, s := range p.Steps {
		if strings.HasPrefix(s.ID, "A-8") {
			continue // outside the trap; finish() runs them
		}
		fmt.Fprintf(&b, "\nCURRENT=%s\nstep %s\n", shq(s.ID), shq(s.ID+" — "+s.Action))
		if s.SkipReason != "" {
			// ⛔ A PLANNED SKIP IS RECORDED `skipped`, NEVER `applied`, and never registered for undo.
			fmt.Fprintf(&b, "printf '   skipped: %%s\\n' %s\nprogress %s skipped\n", shq(s.SkipReason), shq(s.ID))
			continue
		}
		fmt.Fprintf(&b, "APPLIED+=(%s)\nSTEP_SKIPPED=\"\"\n", shq(s.ID))
		b.WriteString(stepBody(s.ID, p, in))
		fmt.Fprintf(&b, "if [ -n \"$STEP_SKIPPED\" ]; then printf '   skipped: %%s\\n' \"$STEP_SKIPPED\"; progress %s skipped; else progress %s applied; fi\n",
			shq(s.ID), shq(s.ID))
	}
	return b.String()
}

// renderKitSwap is A-1, and C-7 lives here.
//
// ⛔ THE KIT HOLDS EVERY INSTANCE ONBOARDED FROM THIS MACHINE. Every instance's per-instance state is
// carried, by name. ⛔ AND A FAILED CARRY FAILS THE STEP: the first version ended each copy with
// `|| true`, so an identity key that did not arrive was an update that "succeeded" and an instance
// that could never authenticate again.
//
// ⛔ `deploy/compose/.env` IS NOT CARRIED. It pins the OLD image.
func renderKitSwap(in PlanInput) string {
	var b strings.Builder
	b.WriteString("mv \"$KIT\" \"$KEPT\"\nmv \"$STAGE/kit\" \"$KIT\"\n")
	b.WriteString(snapshotShippedScenarios)
	b.WriteString(carryUnshipped)
	b.WriteString("# and, BY NAME, the state no instance can live without (C-7) — a failed copy fails the step\n")
	ids := in.KitInstances
	if len(ids) == 0 {
		ids = []string{in.InstanceID}
	}
	for _, id := range ids {
		for _, rel := range perInstanceClasses(id) {
			fmt.Fprintf(&b, "carry %s || false\n", shq(rel))
		}
	}
	b.WriteString("# machine-wide, carried once\n")
	for _, f := range machineWideFiles {
		fmt.Fprintf(&b, "carry %s || false\n", shq(f))
	}
	for _, d := range machineWideDirs {
		fmt.Fprintf(&b, "carry_dir_contents %s || false\n", shq(d))
	}
	b.WriteString("# deploy/compose/.env is REGENERATED, never carried: it pins the image we are moving off\n")
	return b.String()
}

// snapshotShippedScenarios keeps the Path A scenario set EXACTLY AS THIS BUNDLE SHIPS IT, before carryUnshipped
// merges the old kit into the new one. A-6 refreshes from this copy (helpersScenarios).
//
// ⛔ WHY NOT FROM THE KIT (V32 adversary gate finding 25, re-read and fixed in the second 0.3.32 build): after the
// carry, `$KIT/order-service-demo/scenarios-baked` is the new set PLUS every old file the release dropped — and the
// whole old set when the bundle ships none, which made A-6's "ships no set" skip unreachable and would copy the
// old scenarios into test-agent.
const snapshotShippedScenarios = `# the Path A scenario set exactly as THIS bundle ships it — A-6 refreshes from this copy, never from the merged kit
rm -rf "$STAGE/scenarios-shipped"
if [ -d "$KIT/order-service-demo/scenarios-baked" ]; then cp -a "$KIT/order-service-demo/scenarios-baked" "$STAGE/scenarios-shipped" || false; fi
`

// carryUnshipped carries every file the image's bundle does not ship.
//
// ⭐ FOUND BY READING THE apply.sh GENERATED FOR A REAL KIT. A Path-A kit contains the agent folders
// themselves — orderservice-compose's PRODUCT_DIR and SCENARIOS_DIR are <kit>/product-agent and
// <kit>/test-agent — plus onboarding's logs/. The bundle ships none of them, and the class list carried
// none of them, so A-1 would have left both agent folders behind in .prev: A-3 recreating the executor
// over empty mounts, A-5 failing "agent folder is not there". No enumeration can know every file an
// operator or onboarding put in a kit; "what the bundle does not ship" can.
//
// MEASURED on Git Bash (coreutils 8.32): `cp -an OLD/. NEW/` keeps NEW's file, merges into existing
// directories and carries files NEW lacks, exit 0. Newer coreutils exit 1 when -n skips a file, so the
// exit status is not the check — the comparison after it is.
const carryUnshipped = `# ⛔ EVERYTHING THE BUNDLE DOES NOT SHIP IS CARRIED (agent folders inside the kit, logs/, …); the bundle's own files win
cp -an "$KEPT/." "$KIT/" || true
LEFT="$(diff -rq "$KEPT" "$KIT" 2>/dev/null | grep -F "Only in $KEPT" || true)"
if [ -n "$LEFT" ]; then printf '   !! not carried into the new kit:\n%s\n' "$LEFT" >&2; false; fi
`

// renderFinish is A-8a and A-8b, and it is a FUNCTION because BOTH exit paths must reach it (C-14).
//
// ⛔ IT IS OUTSIDE THE TRAP (C-13). finish() disarms before it does anything, so a control-plane
// outage cannot undo an update that succeeded on the machine. ⛔ AND NOTHING IN IT MAY ABORT IT: a
// failing verb here would exit under set -e before the outcome word was printed.
func renderFinish(in PlanInput) string {
	rollbackFlag := ""
	if in.RollbackTo != "" {
		rollbackFlag = " --rollback-to " + shq(in.RollbackTo)
	}
	return `
# ── A-8a / A-8b — OUTSIDE the trap (C-13), reached by EVERY exit path (C-14) ──────────────────────

# record_says <what failed> [commit] — what the standing record reads NOW, when A-8a could not finish. While the
# not-confirmed marker stands, every reader — "argus up", the plan, the siblings, and the A-8b report sent seconds
# later — reads the executor unknown (manifest.go ReadManifest), whatever this run confirmed; "the previous manifest
# stands" was true only with no marker. And after a run that DID confirm the executor, it says so, rather than
# "until an update completes" right after one that did.
# ⛔ AND A COMMIT THAT "FAILED" MAY HAVE WRITTEN THE RECORD. The commit removes the marker standing when it began — this
# run's, or an earlier run's that a move which never started put back — only after it has written the record: that
# marker gone means a record WAS written. And docker can exit 125 after the verb ran: runner() retries 125 alone and
# returns any other failure at once, the verb's own answer — so after a 125 nobody knows, and "no manifest was
# written" is said only after the verb's own failure.
# RECORD_SAID is what it concluded, for A-8a's word (finish): written · unknown · none.
record_says() {
  local m="$ROUTER_STATE/installed/$INSTANCE.unconfirmed" r="$ROUTER_STATE/installed/$INSTANCE.json"
  RECORD_SAID=none
  if [ "${2:-}" = commit ] && [ -n "$MARKER_AT_COMMIT" ] && [ ! -f "$m" ]; then
    RECORD_SAID=written
    printf '   !! %s — but the not-confirmed marker that stood when it began is gone, and only the commit removes it, after it has written the record: a record WAS written (%s)\n' "$1" "$r" >&2
  elif [ "${2:-}" = commit ] && [ -n "${RUNNER_125:-}" ] && [ -f "$m" ]; then
    RECORD_SAID=unknown
    printf '   !! %s — docker exited 125, which it can do after the verb ran, so a record may have been written%s; either way it is marked not confirmed (%s), so it reads the executor as unknown until a later update writes a record\n' "$1" "${EXEC_CONFIRMED:+ (this run confirmed the executor healthy on $EXEC_CONFIRMED)}" "$m" >&2
  elif [ "${2:-}" = commit ] && [ -n "${RUNNER_125:-}" ]; then
    RECORD_SAID=unknown
    printf '   !! %s — docker exited 125, which it can do after the verb ran, so a record may have been written: read %s before re-running\n' "$1" "$r" >&2
  elif [ ! -f "$m" ]; then
    printf '   !! %s, so no manifest was written — the previous record stands\n' "$1" >&2
  elif [ -n "$EXEC_CONFIRMED" ]; then
    printf '   !! %s, so no manifest was written — this run confirmed the executor healthy on %s, but the record is still marked not confirmed (%s), so it reads the executor as unknown until a later update writes a record\n' "$1" "$EXEC_CONFIRMED" "$m" >&2
  else
    printf '   !! %s, so no manifest was written — the record is marked not confirmed (%s), so it reads the executor as unknown until a later update writes a record\n' "$1" "$m" >&2
  fi
}

finish() {
  trap - ERR
  # the stage remembers how it ended: the page block refuses to clear a stage that ended rollback-failed.
  # Only the words A-8a cannot change are written before it — rollback-failed, so a run interrupted inside A-8a still
  # keeps its evidence, and rollback-unreachable (AC-D53), so it still gets the next paste's NOTE; every other word
  # is written AFTER A-8a, when it is final (AC-D58, #392).
  case "$OUTCOME" in rollback-failed|rollback-unreachable) printf '%s\n' "$OUTCOME" > "$STAGE/outcome" 2>/dev/null || true ;; esac

  CURRENT=A-8a
  step 'A-8a — write the installed manifest'
  # ⭐ THE RE-HASH READS progress.json — NOT THE PLAN (C-15), and it runs BEFORE the commit, which
  # stores whatever manifest it is handed. It stays the ONLY writer: nothing here writes a manifest itself.
  RECORDED=0 RECORD_SAID=""
  if runner update rehash --stage "$STAGE" --outcome "$OUTCOME" ${FAILED_STEP:+--failed-step "$FAILED_STEP"}; then
    if [ -f "$ROUTER_STATE/installed/$INSTANCE.unconfirmed" ]; then MARKER_AT_COMMIT=1; fi
    if runner update commit --router-state "$ROUTER_STATE" --manifest "$STAGE/manifest.next.json" \
      --outcome "$OUTCOME" ${FAILED_STEP:+--failed-step "$FAILED_STEP"}` + rollbackFlag + `; then
      RECORDED=1
    else
      record_says 'update commit failed' commit
      # ⛔ AC-D53 + AC-D58: A RECORD THE COMMIT PROVABLY WROTE IS A RECORD. The marker that stood when it began is
      # gone, and removing it is CommitManifest's last act, after WriteManifest — so this run's word is not
      # "unrecorded-…", which would contradict the sentence just printed. After a 125 that proves nothing the record
      # is NOT known to be written, and the word is the unrecorded one: the safe direction.
      if [ "$RECORD_SAID" = written ]; then RECORDED=1; fi
    fi
  else
    # ⚠ AC-D53: the re-hash runs in a container, so when the runtime is still down it cannot record "unknown"
    # either. The not-confirmed marker, written before the move, is what makes the standing record read so.
    record_says 'the re-hash failed'
  fi
  # ⛔ AC-D58 (#392): A-8a IS "applied" ONLY WHEN THE MANIFEST WAS WRITTEN. An update that changed the machine
  # and could not record it does not end on the word it would have had: it ends on the unrecorded form of it
  # (exit 1, below), and A-8b reports that word. The word is final here, so the stage records it only now.
  if [ "$RECORDED" = 1 ]; then
    progress A-8a applied
  else
    progress A-8a failed
    case "$OUTCOME" in
      updated|rolled-back-to*)
        OUTCOME="` + UnrecordedPrefix + `$OUTCOME"
        if [ "$RECORD_SAID" = unknown ]; then
          printf '   !! the machine was changed and this run could not confirm that its record was written — the word is %s\n' "$OUTCOME" >&2
        else
          printf '   !! the machine was changed but its record was not written — the word is %s\n' "$OUTCOME" >&2
        fi ;;
    esac
  fi
  printf '%s\n' "$OUTCOME" > "$STAGE/outcome" 2>/dev/null || true

  CURRENT=A-8b
  step 'A-8b — report the outcome (best-effort)'
  report "$OUTCOME" "$FAILED_STEP" || printf '   !! the control plane was not told — the machine record above is the authority\n' >&2
  progress A-8b applied

  printf '\n%s\n' "$OUTCOME"
  case "$OUTCOME" in updated|rolled-back-to*) exit 0 ;; *) exit 1 ;; esac
}
`
}

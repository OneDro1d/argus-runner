package updatecmd

import (
	"fmt"
	"strings"
)

// scripts_undo.go — V31-001 (VR13-UP): the compensations, one per step that runs.
//
// ⛔ EVERY EFFECT ENDS `|| return 1`. The trap calls these as `if ! undo_X`, and inside that condition
// bash suspends `set -e` for the whole function: a failure in the middle is silently stepped past and
// the function answers with its LAST command's status (measured). Without the explicit returns, an
// undo that failed to put the dashboard back would still report the machine `rolled-back`.
//
// ⛔ AND EVERY UNDO TOLERATES AN EFFECT THAT NEVER STARTED. Steps register BEFORE their effect
// (scripts_apply.go), so an undo can run for a step that failed on its first line.

// AC-D56 (#390): AN UNDO IS JUDGED BY ITS RESTORE. A-3's undo used to end on `executor_healthy`, which probed
// /healthz through the HOST's published port; a host that cannot reach that port (#329) failed the undo of an
// executor that was back and running, and the run was recorded `rollback-failed`. AC-D53 removed that probe: the
// undo's reading goes over the Docker API (the Kubernetes API on k3d/managed), the channel the restore itself used,
// and on compose its rung 5 checks the image ID. ⛔ ONE EXCEPTION TO's WORDING (the operator's
// decision): an executor that is back on PREV_EXECUTOR_IMAGE and SEEN unhealthy there — the runtime
// answering — is `rollback-failed`, so the next paste is refused. The reason that rule gave for a warning (a probe the
// host may not be able to make) is gone; what is left is an observation of an unhealthy executor.

func renderUndoFuncs(p Plan, in PlanInput) string {
	var b strings.Builder
	b.WriteString("\n# ── the compensations, one per step that runs ──────────────────────────────────\n")
	for _, s := range p.Steps {
		if s.Undo == "" || s.SkipReason != "" {
			continue
		}
		fmt.Fprintf(&b, "undo_%s() { # %s\n", shellID(s.ID), s.Undo)
		b.WriteString(undoBody(s.ID, in))
		b.WriteString("}\n")
	}
	return b.String()
}

func undoBody(id string, in PlanInput) string {
	switch id {
	case "A-1":
		return `  [ -d "$KEPT" ] || return 0
  rm -rf "$KIT" || return 1
  mv "$KEPT" "$KIT" || return 1
`
	case "A-3":
		// ⛔ AC-D53 (option C): THE UNDO IS ALWAYS ATTEMPTED, AND WHEN IT FAILS IT SAYS WHY — FROM THE COMMAND THAT
		// FAILED. UNDO_STATE=runtime-unreachable only when that command's own words say the runtime could not be
		// reached (or its health reading completed nothing); everything else is `failed`, which arms the refusal.
		// Never "the undo failed, now ask docker version": a second question that fails for its own reason would
		// clear the refusal for a stage whose undo/ really is the only record of what was live.
		// ⛔ AND A MOVE THAT NEVER STARTED IS UNDONE AS NOTHING (the header's rule, which the fix round's undo broke):
		// with A3_MOVE_STARTED empty the forward move stopped before its recreate / set image — no marker, no env file,
		// a failed pull or import — so the executor was never touched. Recording A-3 unconfirmed, marking the record
		// and recreating the executor then restarted an executor the move had said it would not touch, and on k3d,
		// with the API down, turned a correct record into "unknown". Only what the move did write is put back.
		if in.Tier == "compose" {
			return `  [ -n "$PREV_EXECUTOR_IMAGE" ] || return 0
  UNDO_WHO="the container runtime"
  if [ -z "$A3_MOVE_STARTED" ]; then
    if [ -n "$A3_ENV_SET" ]; then
      set_env_image "$PREV_EXECUTOR_IMAGE" || { undo_classify "could not write $PREV_EXECUTOR_IMAGE back into $ENV_FILE" ""; return 1; }
    fi
    restore_marker || printf '   !! could not put the not-confirmed marker under %s back as it was\n' "$ROUTER_STATE/installed" >&2
    return 0
  fi
  # a file write: its failure never needs the runtime, so it is always genuine
  set_env_image "$PREV_EXECUTOR_IMAGE" || { undo_classify "could not write $PREV_EXECUTOR_IMAGE back into $ENV_FILE" ""; return 1; }
  # ⛔ THE UNDO MOVES THE EXECUTOR TOO, so A-3 is unconfirmed again BEFORE its recreate: left "applied" (a forward
  # move that was confirmed, then a later step failed), an undo that could not be confirmed was re-hashed as the
  # NEW version for an executor already put back on the old one. A clean undo ends "undone" (rollback() writes it).
  progress A-3 unconfirmed
  mark_unconfirmed || printf '   !! could not write the not-confirmed marker under %s\n' "$ROUTER_STATE/installed" >&2
  local said
  EXEC_CONFIRMED=""
  if ! dc up -d --no-deps --force-recreate executor 2>"$PROBE_ERR"; then
    said="$(cat "$PROBE_ERR" 2>/dev/null || true)"
    printf '%s\n' "$said" >&2
    undo_classify "$said" "$said"
    return 1
  fi
  UNDO_RECREATED=1
  cat "$PROBE_ERR" >&2 2>/dev/null || true
  # SEEN unhealthy on PREV_EXECUTOR_IMAGE, or serving another image (rung 5), is UNDO_STATE=failed: rollback-failed
  # and the next paste refused. Only a reading the runtime did not answer is runtime-unreachable.
  if ! executor_healthy "$HEALTH_BUDGET" "$PREV_EXECUTOR_IMAGE"; then
    undo_classify "$HEALTH_DETAIL" "" health
    # the forward move's own sentence (health_says): what was OBSERVED after this recreate — never "back on <old>" for
    # an executor rung 5 saw serving another image
    if [ "$UNDO_STATE" = failed ]; then health_says "after the undo's recreate on $PREV_EXECUTOR_IMAGE" "the executor"; fi
    return 1
  fi
  EXEC_CONFIRMED="$PREV_EXECUTOR_IMAGE"
`
		}
		return `  [ -n "$PREV_EXECUTOR_IMAGE" ] || return 0
  UNDO_WHO="the Kubernetes API server"
  if [ -z "$A3_MOVE_STARTED" ]; then
    restore_marker || printf '   !! could not put the not-confirmed marker under %s back as it was\n' "$ROUTER_STATE/installed" >&2
    return 0
  fi
  # see the compose undo: unconfirmed again BEFORE the Deployment is set back
  progress A-3 unconfirmed
  mark_unconfirmed || printf '   !! could not write the not-confirmed marker under %s\n' "$ROUTER_STATE/installed" >&2
  local said
  EXEC_CONFIRMED=""
  if ! said="$(kube -n "$NS" set image deploy/executor "executor=$PREV_EXECUTOR_IMAGE" 2>&1)"; then
    printf '%s\n' "$said" >&2
    undo_classify "$said" "$said"
    return 1
  fi
  UNDO_RECREATED=1
  # AC-D56 (#390): THE RESTORE ITSELF FIRST — the Deployment must name the image it was put back on (dev's order: so
  # "back on <old>" below is a fact that was read). The read's OWN words decide, as for every AC-D53 reading: an API
  # server that did not answer is runtime-unreachable, never a mismatch. ⛔ stdout ONLY is the image: kubectl can
  # write to stderr on a read that succeeded (klog discovery lines, API "Warning:" headers), and folded into the value
  # those made a correct restore a mismatch — rollback-failed, the next paste refused (review of the merge, high).
  local named said
  if ! named="$(kube -n "$NS" get deploy executor -o jsonpath='{.spec.template.spec.containers[0].image}' 2>"$PROBE_ERR")"; then
    said="$(cat "$PROBE_ERR" 2>/dev/null || true)"
    printf '%s\n' "$said" >&2
    undo_classify "could not read the image deploy/executor names after the undo: $said" "$said"
    return 1
  fi
  if [ "$named" != "$PREV_EXECUTOR_IMAGE" ]; then
    printf '   !! deploy/executor in %s names %s after the undo, not %s — it was not restored\n' "$NS" "$named" "$PREV_EXECUTOR_IMAGE" >&2
    undo_classify "deploy/executor names $named after the undo, not $PREV_EXECUTOR_IMAGE" ""
    return 1
  fi
  # a rollout that does not finish while the API answers is SEEN unhealthy: rollback-failed
  if ! executor_healthy "$HEALTH_BUDGET" "$PREV_EXECUTOR_IMAGE"; then
    undo_classify "$HEALTH_DETAIL" "" health
    if [ "$UNDO_STATE" = failed ]; then printf '   !! deploy/executor is back on %s but did not finish rolling out: %s\n' "$PREV_EXECUTOR_IMAGE" "$HEALTH_DETAIL" >&2; fi
    return 1
  fi
  EXEC_CONFIRMED="$PREV_EXECUTOR_IMAGE"
`
	case "A-4":
		// ⛔ TOLERATE ABSENCE, NEVER TOLERATE FAILURE. A document captured as absent is DELETEd; one
		// that was live is re-posted. Every body goes through STDIN: Git Bash's curl is a native
		// Windows program, and with path conversion off it cannot open `/c/…` from `-d @file`.
		return `  [ -n "$GRAFANA" ] || return 0
  # ⛔ NO CAPTURE IS NOT "NOTHING TO PUT BACK". Preflight captures both documents whenever A-4 will run;
  # a missing file means the live state before this update is unknown, and an undo that silently did
  # nothing would still report the machine rolled-back.
  for doc in dashboard datasource; do
    [ -f "$STAGE/undo/$doc.json" ] || { printf '   !! no captured %s in %s/undo — it cannot be put back\n' "$doc" "$STAGE" >&2; return 1; }
  done
  if grep -q '"present":false' "$STAGE/undo/dashboard.json" 2>/dev/null; then
    curl -fsS -X DELETE "$GRAFANA/api/dashboards/uid/argus-overview-$INSTANCE" >/dev/null || return 1
  elif [ -f "$STAGE/undo/dashboard.json" ]; then
    # jq-free: the save API takes "dashboard" + "overwrite" and ignores the GET body's "meta"
    sed '1s/^{/{"overwrite":true,/' "$STAGE/undo/dashboard.json" \
      | curl -fsS -X POST -H 'Content-Type: application/json' -d @- "$GRAFANA/api/dashboards/db" >/dev/null || return 1
  fi
  if grep -q '"present":false' "$STAGE/undo/datasource.json" 2>/dev/null; then
    curl -fsS -X DELETE "$GRAFANA/api/datasources/uid/loki-$INSTANCE" >/dev/null || return 1
  elif [ -f "$STAGE/undo/datasource.json" ]; then
    curl -fsS -X PUT -H 'Content-Type: application/json' -d @- \
      "$GRAFANA/api/datasources/uid/loki-$INSTANCE" < "$STAGE/undo/datasource.json" >/dev/null || return 1
  fi
  if [ -n "$TARGETS_FILE" ]; then
    if [ -f "$STAGE/undo/targets.json" ]; then cp "$STAGE/undo/targets.json" "$TARGETS_FILE" || return 1
    else rm -f "$TARGETS_FILE" || return 1; fi
  fi
`
	case "A-5":
		// ⛔ LOCAL: these names would otherwise overwrite the caller's (rollback()'s step id was `s`).
		return `  [ -f "$STAGE/undo/skills.tsv" ] || return 0
  local op dir s bk
  while IFS=$'\t' read -r op dir s bk; do
    [ -n "$op" ] || continue
    rm -rf "$dir/.claude/skills/$s" || return 1
    if [ "$op" = restore ]; then cp -r "$bk" "$dir/.claude/skills/$s" || return 1; fi
  done < "$STAGE/undo/skills.tsv"
`
	case "A-6":
		// tolerates a refresh that never started: no recorded backup means the folder was not touched
		return `  [ -f "$STAGE/undo/scenarios.path" ] || return 0
  local bk
  bk="$(cat "$STAGE/undo/scenarios.path")" || return 1
  [ -d "$bk" ] || { printf '   !! the scenario backup %s is gone — the scenarios cannot be put back\n' "$bk" >&2; return 1; }
  # IN PLACE, like the refresh: the folder itself is the executor's bind-mount source (see helpersScenarios)
  mkdir -p "$TEST_DIR/scenarios" || return 1
  find "$TEST_DIR/scenarios" -mindepth 1 -maxdepth 1 -exec rm -rf {} + || return 1
  cp -a "$bk/." "$TEST_DIR/scenarios/" || return 1
`
	case "A-7":
		return `  [ -n "$PREV_ROUTER_IMAGE" ] || return 0
  router_compose_up "$PREV_ROUTER_IMAGE" || return 1
  router_healthy "$ROUTER_PORT" "$HEALTH_BUDGET" || return 1
`
	}
	// ⛔ NO SILENT `:`. A step that runs with no compensation is a rollback that claims success.
	return "  printf '   !! %s has no compensation in this release\\n' " + shq(id) + " >&2\n  return 1\n"
}

package updatecmd

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/role"
	"github.com/OneDro1d/argus-runner/internal/skills"
)

// scripts_steps.go — V31-001 (VR13-UP): what each A-step actually does, per tier.
//
// Every function here is called as `fn || false` (see scripts.go's header for the measured reason) and
// passes failure up with `|| return 1`. A library is sourced from the NEW kit, AFTER A-1 has landed it,
// so the update runs the mechanism it ships rather than the one it is replacing.

// renderStepHelpers emits the functions the step bodies call.
func renderStepHelpers(p Plan, in PlanInput) string {
	var b strings.Builder
	b.WriteString(helpersCarry)
	b.WriteString("\nPREV_EXECUTOR_IMAGE=\"\"\n")
	if in.Tier == "compose" {
		b.WriteString(helpersExecutorCompose)
	} else {
		b.WriteString(helpersExecutorK8s(in.Tier == "k3d"))
	}
	b.WriteString("\nGRAFANA=" + shq(a4Grafana(p, in)) + "\nTARGETS_FILE=\"\"\n")
	if in.Tier != "managed" {
		b.WriteString(helpersObs(in.Tier))
	}
	b.WriteString(helpersSkills)
	if dir := pathATestDir(p, in); dir != "" {
		b.WriteString("\nTEST_DIR=" + shq(MSYSPath(dir)) + "\n")
		b.WriteString(helpersScenarios)
	}
	rollback := "0"
	if in.RollbackTo != "" {
		rollback = "1"
	}
	b.WriteString(strings.Replace(helpersRouter, "@ROLLBACK@", rollback, 1))
	return b.String()
}

func stepBody(id string, p Plan, in PlanInput) string {
	switch id {
	case "A-1":
		return renderKitSwap(in)
	case "A-3":
		// AC-D53: no auth-curl.sh — the health reading goes over the Docker/Kubernetes API and sends no token
		lib := ""
		if in.Tier == "k3d" {
			lib = `. "$KIT/onboarding/lib/k3d-image.sh"` + "\n"
		}
		return lib + "move_executor || false\n"
	case "A-4":
		return `. "$KIT/onboarding/lib/grafana-refs.sh"` + "\nprovision_obs || false\n"
	case "A-5":
		var b strings.Builder
		b.WriteString(": > \"$STAGE/undo/skills.tsv\"\n")
		for _, f := range in.Observed.Folders {
			names := skills.Installed(role.Role(f.Hat))
			quoted := make([]string, len(names))
			for i, n := range names {
				quoted[i] = shq(n)
			}
			fmt.Fprintf(&b, "install_skills_into %s %s || false\n", shq(MSYSPath(f.Path)), strings.Join(quoted, " "))
		}
		return b.String()
	case "A-6":
		return "refresh_path_a_scenarios || false\n"
	case "A-7":
		// the upgrade order is printed wherever an operator moves a component (router-version.sh:100) —
		// update.sh printed it on every instance update, and the page block is that update now
		return `. "$KIT/onboarding/lib/router-version.sh"` + "\nprintf '   %s\\n' \"$ROUTER_UPGRADE_ORDER\"\nmove_router || false\n"
	}
	// A step with no mechanism in this release must never reach here unskipped: rendering `:` would
	// record `applied` for something that did not happen.
	return "printf '   !! %s has no mechanism in this release\\n' " + shq(id) + " >&2\nfalse\n"
}

const helpersCarry = `
carry() { # <path relative to the kit> — replaced whole, never merged into a staged copy of the same name
  [ -e "$KEPT/$1" ] || return 0
  mkdir -p "$(dirname "$KIT/$1")" || return 1
  rm -rf "$KIT/$1" || return 1
  cp -a "$KEPT/$1" "$KIT/$1" || return 1
}
carry_dir_contents() { # <dir relative to the kit>
  [ -d "$KEPT/$1" ] || return 0
  mkdir -p "$KIT/$1" || return 1
  cp -a "$KEPT/$1/." "$KIT/$1/" || return 1
}
`

// helpersExecutorCompose is update.sh:641-651 (apply_image), compose half, and AC-D53's health reading.
//
// ⛔ AC-D53 (#329): THE HEALTH OF THE EXECUTOR IS READ OVER THE DOCKER API ONLY. update.sh:399-425 asked
// `localhost:<published port>/healthz` from the host — a question with two subjects, the executor AND the host's
// port forwarding, answered by the same silence when either is broken. Measured 2026-09-29: the executor said
// {"status":"ok"} inside its container while the host port relayed nothing; the update read that as "the
// executor did not come back healthy on <image>" and rolled a correct update back. The recreate needed the
// Docker API and nothing else, so the verdict does too — the rungs, in order:
//
//	1  executor_container -a        the runtime holds the container
//	2  .State.Status                 it is running (not exited / created), with its exit code and restarts
//	3  .State.Health.Status          its OWN healthcheck (docker-compose.byo-m3.yml) — free, docker ran it
//	4  docker exec … curl            no healthcheck (a container from an older kit — EVERY executor in the
//	                                 estate on 2026-09-30): asked from INSIDE, on its routable address
//	5  .Image vs the asked image     the image serving is the one asked for, not the one before
//
// Rung 5 IS AC-D55's image-ID check (#389): the container's .Image against `docker image inspect`'s
// .Id of the image asked for — /healthz answers the same on any version. It runs on every reading, so it covers the
// forward move and the undo alike; a second check after it would only add a docker call whose failure nobody classifies.
//
// No token anywhere: /healthz is on the runner's plain mux (internal/runner/runner.go). The budget is
// unchanged: it was never the defect, and shortening it manufactures false negatives.
const helpersExecutorCompose = `
set_env_image() { # <image> — update.sh:642-646
  if grep -q '^ARGUS_MCP_IMAGE=' "$ENV_FILE"; then
    sed -i.bak "s|^ARGUS_MCP_IMAGE=.*|ARGUS_MCP_IMAGE=$1|" "$ENV_FILE" || return 1
    rm -f "$ENV_FILE.bak"
  else
    printf 'ARGUS_MCP_IMAGE=%s\n' "$1" >> "$ENV_FILE" || return 1
  fi
}

# executor_observe <image asked for> — ONE reading over the Docker API. 0 only for healthy; otherwise
# HEALTH_STATE says unhealthy (the runtime answered, and HEALTH_DETAIL says what it said), unreachable, or
# starting (the container's own healthcheck has not decided yet — never left in HEALTH_STATE after the wait).
executor_observe() {
  local c info st code rs health img ref ports cport out want
  c="$(executor_container -a 2>"$PROBE_ERR")" || { probe_failed "docker could not list the executor of $PROJECT"; return 1; }
  [ -n "$c" ] || { health_note unhealthy "compose project $PROJECT has no executor container"; return 1; }
  info="$(docker inspect "$c" --format '{{.State.Status}}|{{.State.ExitCode}}|{{.RestartCount}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}|{{.Image}}|{{.Config.Image}}|{{range $p, $b := .NetworkSettings.Ports}}{{if $b}}{{$p}},{{end}}{{end}}' 2>"$PROBE_ERR")" \
    || { probe_failed "docker inspect $c"; return 1; }
  IFS='|' read -r st code rs health img ref ports <<< "$info"
  [ "$st" = running ] || { health_note unhealthy "the executor container is $st (exit code $code, restarts $rs)"; return 1; }
  case "$health" in
    healthy) ;;
    none)
      # the container port is READ, never assumed — the same refusal to guess as finding it by compose labels
      cport="${ports%%/*}"; case "$cport" in ''|*[!0-9]*) cport=8080 ;; esac
      # ⛔ THE ROUTABLE ADDRESS, NEVER LOOPBACK: a process bound to 127.0.0.1 answers a loopback probe and nothing
      # else (docker-compose.router.yml's measured lesson). $(hostname), not $(hostname -i): on this service's two
      # networks hostname -i prints two addresses (measured 2026-09-30). The probe's own verdict comes back on
      # stdout, so a probe that ran and failed is never mistaken for a docker exec that could not start.
      # --noproxy: compose puts ~/.docker/config.json's proxies into the container's env, and curl would ask the
      # PROXY for the container's own name — a serving executor read as unhealthy (measured 2026-09-30).
      out="$(docker exec "$c" sh -c 'r="$(curl -fsS -m 5 --noproxy "*" "http://$(hostname):'"$cport"'/healthz" 2>&1)"; printf "ARGUS-PROBE rc=%s %s\n" "$?" "$r"' 2>"$PROBE_ERR")" || true
      case "$out" in
        *"ARGUS-PROBE rc=0 "*) ;;
        *"ARGUS-PROBE rc="*) health_note unhealthy "asked from inside its container, /healthz on port $cport failed: ${out#*ARGUS-PROBE }"; return 1 ;;
        *) probe_failed "docker exec $c"; return 1 ;;
      esac ;;
    # docker has not decided yet: no verdict, and the wait decides what it means (executor_healthy)
    starting) health_note starting "its own healthcheck says starting"; return 1 ;;
    *) health_note unhealthy "its own healthcheck says $health$(container_health_log "$c")"; return 1 ;;
  esac
  want="$(docker image inspect --format '{{.Id}}' "$1" 2>"$PROBE_ERR")" || { probe_failed "docker image inspect $1"; return 1; }
  [ "$img" = "$want" ] || { health_note unhealthy "serving, but still on $ref ($img), not $1"; return 1; }
  health_note healthy "running and serving on $1"
}

# executor_healthy <seconds> <image asked for> — up AND serving AND on the image asked for, over the Docker API.
executor_healthy() {
  local deadline seen="" seen_detail="" last=""
  deadline=$(( $(date +%s) + $1 ))
  while :; do
    if executor_observe "$2"; then return 0; fi
    # ⛔ AN ESTABLISHED READING IS KEPT: a later poll the runtime did not answer never turns "it was seen, and it
    # was not healthy" into "could not reach".
    # ⛔ AND "starting" IS NOT SUCH A READING: docker has not decided yet, and the FIRST reading after every recreate
    # says it (the loop asks ~1 s after dc up, docker's first check runs 5-10 s later — measured 2026-09-30). Kept,
    # it turned "starting, then the runtime stopped answering" into "unhealthy" — in the undo, an armed refusal.
    last="$HEALTH_STATE"
    if [ "$HEALTH_STATE" = unhealthy ]; then seen=1; seen_detail="$HEALTH_DETAIL"; fi
    [ "$(date +%s)" -lt "$deadline" ] || break
    sleep 3
  done
  if [ -n "$seen" ]; then health_note unhealthy "$seen_detail"
  elif [ "$last" = starting ]; then
    # the runtime answered AT the deadline and the container's own healthcheck had still not reported: that IS a verdict
    # ⚠ ALSO when the daemon restarted seconds before the deadline and the container has run only that long — in the
    # undo, that arms the refusal. Kept on purpose (operator decision 2026-09-30): it errs toward the refusal.
    health_note unhealthy "its own healthcheck never reported healthy within ${1}s (it still says starting)"
  fi
  # otherwise the last reading could not be completed and nothing but "starting" came before it: unreachable, in
  # the failing command's own words (probe_failed wrote them)
  return 1
}

move_executor() {
  local c
  c="$(executor_container -a)" || { printf '   !! docker did not answer while looking for the executor\n' >&2; return 1; }
  [ -n "$c" ] || { printf '   !! compose project %s has no executor container\n' "$PROJECT" >&2; return 1; }
  PREV_EXECUTOR_IMAGE="$(docker inspect "$c" --format '{{.Config.Image}}' 2>/dev/null || true)"
  [ -n "$PREV_EXECUTOR_IMAGE" ] || { printf '   !! could not read the image the executor runs, so it could not be put back — not moved\n' >&2; return 1; }
  # ⛔ THE RECORD IS MARKED "NOT CONFIRMED" BEFORE ANYTHING CHANGES (criterion k) — a move it cannot mark is not made,
  # and the undo then does nothing to the executor (A3_MOVE_STARTED is still empty)
  keep_marker || { printf '   !! could not keep a copy of the not-confirmed marker in %s/undo — not moved\n' "$STAGE" >&2; return 1; }
  mark_unconfirmed || { printf '   !! could not write the not-confirmed marker under %s — not moved\n' "$ROUTER_STATE/installed" >&2; return 1; }
  # ⛔ THE IMAGE IS AN INDIRECTION (docker-compose.byo-m3.yml:16): recreating without this re-runs the OLD one.
  A3_ENV_SET=1
  set_env_image "$IMAGE_DIGEST" || return 1
  # ⛔ FROM HERE THE MACHINE MAY HAVE CHANGED, so A-3 says so BEFORE the recreate: when neither the move nor its
  # undo can be confirmed, the re-hash records the executor "unknown" (rehash.go) — never the version it had
  # before, which would be a confident claim about a machine that may well be on the new one. A success
  # overwrites it with "applied"; a clean undo with "undone" (the last writing wins).
  progress A-3 unconfirmed
  # ONLY the executor. Never the SUT, never obs, never their volumes (update.sh:650, UC152/UC153).
  A3_MOVE_STARTED=1
  dc up -d --no-deps --force-recreate executor || return 1
  A3_RECREATED=1
  if ! executor_healthy "$HEALTH_BUDGET" "$IMAGE_DIGEST"; then
    A3_FWD_HEALTH="$HEALTH_STATE" A3_FWD_DETAIL="$HEALTH_DETAIL"   # the undo's own reading overwrites HEALTH_*
    progress A-3 unconfirmed "$HEALTH_STATE" "$HEALTH_DETAIL"
    health_says "after the recreate on $IMAGE_DIGEST" "the executor"
    return 1
  fi
  A3_CONFIRMED=1 EXEC_CONFIRMED="$IMAGE_DIGEST"
  printf '   executor now on %s (was %s)\n' "$IMAGE_DIGEST" "$PREV_EXECUTOR_IMAGE"
}
`

// helpersExecutorK8s is update.sh:626-639, k8s half. The readiness rollout IS the health check.
func helpersExecutorK8s(k3d bool) string {
	imp := ""
	if k3d {
		// V28-002 / V28-020 (update.sh:630-637): the nodes import from THIS machine — pull, never fall
		// through; the library reads the node back and returns 1 only on a verified miss.
		imp = `  docker image inspect "$IMAGE_DIGEST" >/dev/null 2>&1 || docker pull "$IMAGE_DIGEST" >/dev/null \
    || { printf '   !! could not pull %s onto this machine — run docker login ghcr.io; the k3d nodes import it from here\n' "$IMAGE_DIGEST" >&2; return 1; }
  k3d_import_image "$KUBE_CONTEXT" "$IMAGE_DIGEST" || return 1
`
	}
	return `
# executor_healthy <seconds> [image asked for] — the Deployment's own rollout, on the API the move itself used
# (the readiness it waits for is the kubelet's httpGet /healthz, judged inside the cluster — internal/k8srender).
# AC-D53: kubectl returns 1 for BOTH "the rollout did not finish" and "the API server did not answer", so its
# WORDS decide, never its exit code (preflightTierChecks keeps them for the same reason).
executor_healthy() {
  local out rc=0 counts
  out="$(kube -n "$NS" rollout status deploy/executor --timeout="$1s" 2>&1)" || rc=$?
  if [ "$rc" = 0 ]; then health_note healthy "$out"; return 0; fi
  if kube_unreachable "$out"; then health_note unreachable "$out"; return 1; fi
  # the API answered and the rollout did not finish: all four counts, because a PENDING pod counts as "updated" —
  # only total above desired shows an old pod still serving (internal/federation/wire.go, ReplicasTotal). Evidence
  # only: when the counts cannot be read, the verdict stands.
  counts="$(kube -n "$NS" get deploy executor -o 'jsonpath=desired={.spec.replicas} ready={.status.readyReplicas} updated={.status.updatedReplicas} total={.status.replicas}' 2>&1)" \
    || counts="the replica counts could not be read: $counts"
  # the COUNTS FIRST, then kubectl's last two lines: rollout status prints one line per change of the Deployment,
  # and one_line keeps 400 characters — the counts put after them were cut off, total=2 first of all
  health_note unhealthy "$counts — $(printf '%s\n' "$out" | tail -n 2)"
  return 1
}

move_executor() {
  PREV_EXECUTOR_IMAGE="$(kube -n "$NS" get deploy executor -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
  [ -n "$PREV_EXECUTOR_IMAGE" ] || { printf '   !! could not read the executor image in %s, so it could not be put back — not moved\n' "$NS" >&2; return 1; }
` + imp + `  # from here the Deployment may have changed — see the compose move_executor
  keep_marker || { printf '   !! could not keep a copy of the not-confirmed marker in %s/undo — not moved\n' "$STAGE" >&2; return 1; }
  mark_unconfirmed || { printf '   !! could not write the not-confirmed marker under %s — not moved\n' "$ROUTER_STATE/installed" >&2; return 1; }
  progress A-3 unconfirmed
  A3_MOVE_STARTED=1
  kube -n "$NS" set image deploy/executor "executor=$IMAGE_DIGEST" >/dev/null || return 1
  A3_RECREATED=1
  if ! executor_healthy "$HEALTH_BUDGET" "$IMAGE_DIGEST"; then
    A3_FWD_HEALTH="$HEALTH_STATE" A3_FWD_DETAIL="$HEALTH_DETAIL"
    progress A-3 unconfirmed "$HEALTH_STATE" "$HEALTH_DETAIL"
    health_says "after set image to $IMAGE_DIGEST" "deploy/executor"
    return 1
  fi
  A3_CONFIRMED=1 EXEC_CONFIRMED="$IMAGE_DIGEST"
  printf '   executor now on %s (was %s)\n' "$IMAGE_DIGEST" "$PREV_EXECUTOR_IMAGE"
}
`
}

// helpersObs is A-4 on the two local tiers — onboard.sh:2992-3053 (k3d) and :3172-3216 (compose),
// using the kit's grafana-refs.sh, with every WARN there made a failure here: an update that says
// `updated` over a dashboard it could not provision is the silence this row exists to end.
func helpersObs(tier string) string {
	addr := `  ds_url="http://argus-inst-$INSTANCE-loki-1:3100"            # onboard.sh:3184
  tgt="argus-inst-$INSTANCE-pushgateway-1:9091"                 # onboard.sh:3175
`
	promtail := `  dc up -d --force-recreate --no-deps promtail || return 1         # onboard.sh:3303
`
	if tier == "k3d" {
		addr = `  local cluster node loki_np pg_np c
  cluster="${KUBE_CONTEXT#k3d-}"; node="k3d-${cluster}-server-0"   # onboard.sh:2999-3000
  loki_np="$(kube -n "$NS" get svc loki -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
  pg_np="$(kube -n "$NS" get svc pushgateway -o jsonpath='{.spec.ports[0].nodePort}' 2>/dev/null || true)"
  if [ -z "$loki_np" ] || [ -z "$pg_np" ]; then
    printf '   !! could not read the obs NodePorts (loki=%s pushgateway=%s)\n' "$loki_np" "$pg_np" >&2; return 1
  fi
  for c in argus-obs-grafana-1 argus-obs-prometheus-1; do  # onboard.sh:3008-3010 — "already exists" is success
    docker network connect "k3d-$cluster" "$c" >/dev/null 2>&1 || true
  done
  ds_url="http://$node:$loki_np"
  tgt="$node:$pg_np"
`
		promtail = "  # k3d: promtail is a DaemonSet in the instance namespace (internal/k8srender), not a host container\n"
	}
	return `
provision_obs() {
  local ds_url tgt tgt_dir src dash code
` + addr + `  # the Prometheus target, into the directory the RUNNING shared Prometheus mounts (onboard.sh:2992-2997)
  tgt_dir="$COMPOSE_DIR/targets.d"
  src="$(docker inspect argus-obs-prometheus-1 --format '{{range .Mounts}}{{if eq .Destination "/etc/prometheus/targets.d"}}{{.Source}}{{end}}{{end}}' 2>/dev/null || true)"
  if [ -n "$src" ]; then
    tgt_dir="$(printf '%s' "$src" | sed -e 's#^/mnt/\([a-zA-Z]\)/#/\1/#' -e 's#^/host_mnt/\([a-zA-Z]\)/#/\1/#' -e 's#^/run/desktop/mnt/host/\([a-zA-Z]\)/#/\1/#')"
  fi
  mkdir -p "$tgt_dir" "$STAGE/undo" || return 1
  TARGETS_FILE="$tgt_dir/$INSTANCE.json"
  rm -f "$STAGE/undo/targets.json"
  if [ -f "$TARGETS_FILE" ]; then cp "$TARGETS_FILE" "$STAGE/undo/targets.json" || return 1; fi
  printf '[{"targets": ["%s"], "labels": {"argus_instance": "%s"}}]\n' "$tgt" "$INSTANCE" > "$TARGETS_FILE" || return 1

  grafana_provision_datasource "$GRAFANA" "loki-$INSTANCE" \
    "{\"name\":\"Loki ($INSTANCE)\",\"uid\":\"loki-$INSTANCE\",\"type\":\"loki\",\"access\":\"proxy\",\"url\":\"$ds_url\"}" \
    "shared Grafana" || return 1

  if [ -f "$COMPOSE_DIR/grafana/dashboards/argus-overview.json" ]; then
    # the kit's one derivation (grafana-refs.sh), so an updated dashboard still OPENS on its own instance.
    # ⛔ THIS SCRIPT IS GENERATED BY THE NEWER IMAGE BUT SOURCES THE TARGET KIT'S LIBRARY, and a
    # rollback target is OLDER. grafana_instance_dashboard exists from release 0.3.61 (#545); a kit before it does not
    # define it, and calling it there killed the rollback 0.3.61 -> 0.3.60 at A-4 (release-import run 37351718184).
    # Without it the dashboard is derived as that older kit's own onboarding did (v0.3.60 onboard.sh / this file's sed).
    # DELETE THE FALLBACK only when no instance can hold a kit older than 0.3.61: when the ENFORCED floor
    # (MIN_EXECUTOR_VERSION_ABSOLUTE) is above 0.3.60 — MIN_EXECUTOR_VERSION_RECOMMENDED does not stop a rollback.
    if declare -F grafana_instance_dashboard >/dev/null 2>&1; then
      dash="$(grafana_instance_dashboard "$COMPOSE_DIR/grafana/dashboards/argus-overview.json" "$INSTANCE")" || return 1
    else
      dash="$(sed -e "s/\"uid\": \"loki\"/\"uid\": \"loki-$INSTANCE\"/g" \
                  -e "s/\"uid\": \"argus-overview\"/\"uid\": \"argus-overview-$INSTANCE\"/" \
                  -e "s/\"title\": \"Argus Overview (Argus M2)\"/\"title\": \"Argus Overview — $INSTANCE\"/" \
                  "$COMPOSE_DIR/grafana/dashboards/argus-overview.json")" || return 1
    fi
    # capture first, then default — never ` + "`|| echo 000`" + ` into the substitution (onboard.sh:3185-3187)
    code="$(printf '{"dashboard": %s, "overwrite": true}' "$dash" \
      | curl -sS --max-time 15 -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' -d @- "$GRAFANA/api/dashboards/db" 2>/dev/null \
      | tail -n 1)" || true
    case "$code" in ''|*[!0-9]*) code=000 ;; esac
    [ "$code" = 200 ] || { printf '   !! the dashboard import returned HTTP %s\n' "$code" >&2; return 1; }
    # VR5-O2: verify through the API, never /d/ (which answers 200 for a dashboard that does not exist)
    if [ "$(grafana_dashboard_state "$GRAFANA" "argus-overview-$INSTANCE")" = missing ]; then
      printf '   !! Grafana holds no argus-overview-%s after the import\n' "$INSTANCE" >&2; return 1
    fi
  else
    printf '   NOTE: the new kit ships no dashboard template — the dashboard was left as it is\n'
  fi
` + promtail + `}
`
}

// helpersSkills is onboard.sh:2828-2845's install_skills, recording every change so A-5's undo can put
// back exactly what was there — and remove what was not.
const helpersSkills = `
install_skills_into() { # <folder> <skill>...
  local dir="$1" s bk; shift
  [ -d "$dir" ] || { printf '   !! agent folder %s is not there\n' "$dir" >&2; return 1; }
  mkdir -p "$dir/.claude/skills" "$STAGE/undo" || return 1
  # ⛔ the backup goes BESIDE .claude/skills, never inside: every subdirectory there is a loadable skill
  bk="$dir/.argus-skill-backups/$STAMP"
  for s in "$@"; do
    [ -d "$KIT/skills/$s" ] || { printf '   !! the new kit ships no skill %s\n' "$s" >&2; return 1; }
    if [ -d "$dir/.claude/skills/$s" ]; then
      mkdir -p "$bk" || return 1
      cp -r "$dir/.claude/skills/$s" "$bk/$s" || return 1
      printf 'restore\t%s\t%s\t%s\n' "$dir" "$s" "$bk/$s" >> "$STAGE/undo/skills.tsv" || return 1
    else
      printf 'remove\t%s\t%s\t-\n' "$dir" "$s" >> "$STAGE/undo/skills.tsv" || return 1
    fi
    rm -rf "$dir/.claude/skills/$s" || return 1
    cp -r "$KIT/skills/$s" "$dir/.claude/skills/$s" || return 1
  done
  if [ -d "$bk" ]; then printf '   kept what was there: %s\n' "$bk"; fi
}
`

// helpersScenarios is A-6: the Path A demo scenarios, replaced WHOLE from the new kit with what was there
// kept beside the folder — A-5's file rules (V32-01 PO: "A-6 Path A scenarios, same file rules").
//
// ⛔ REPLACED, NEVER MERGED: a merge would leave an old-format file the new release refuses sitting next
// to its replacement. ⛔ And the backup path is recorded for the undo only AFTER the copy succeeded, so
// an undo never "restores" a half-written backup over the live folder.
//
// ⛔ THE CONTENTS ARE REPLACED, NEVER THE FOLDER (V32 adversary gate). On a compose Path A kit this folder
// IS the executor's /scenarios bind-mount source (docker-compose.byo-m3.yml:82), and A-3 has already
// recreated the executor on it. Deleting and re-creating the directory leaves a bind mount that pins the
// directory itself (Linux Docker Engine) on the deleted, empty one — the files land where the container
// can no longer see them.
const helpersScenarios = `
refresh_path_a_scenarios() {
  # the set A-1 snapshotted from the bundle BEFORE its carry merged the old kit in (scripts_apply.go snapshotShippedScenarios)
  local src="$STAGE/scenarios-shipped" dst="$TEST_DIR/scenarios" bk
  [ -d "$src" ] || { skip_step "the new kit ships no Path A scenario set (order-service-demo/scenarios-baked) — the scenarios were left as they are"; return 0; }
  [ -d "$dst" ] || { skip_step "there is no $dst to refresh — the scenarios were left as they are"; return 0; }
  bk="$TEST_DIR/.argus-scenario-backups/$STAMP"
  mkdir -p "$bk/scenarios" "$STAGE/undo" || return 1
  cp -a "$dst/." "$bk/scenarios/" || return 1
  printf '%s\n' "$bk/scenarios" > "$STAGE/undo/scenarios.path" || return 1
  find "$dst" -mindepth 1 -maxdepth 1 -exec rm -rf {} + || return 1
  cp -a "$src/." "$dst/" || return 1
  printf '   kept what was there: %s\n' "$bk/scenarios"
  # ⚠ THE CATALOG IS NOT THESE FILES. A catalog-wired executor runs the control plane's set, and an update
  # never rewrites a catalog the account may have edited since — measured: after this release's update
  # every run errored until the catalog itself was re-imported.
  printf '   NOTE: the control plane catalog is NOT changed by an update — if it still holds older scenarios, re-import these into it\n'
}
`

// helpersRouter is update.sh:170-247 (move_router) and :696-740 (the skew decision). Its DECLINE cases
// become skips with their reason; its failure is left to the trap, which puts the router back with
// everything else instead of leaving a half-atomic machine.
const helpersRouter = `
ROUTER_COMPOSE="$COMPOSE_DIR/docker-compose.router.yml"
PREV_ROUTER_IMAGE=""
ROUTER_PORT="" ROUTER_HOST_NAME="" ROUTER_HOST_ALIAS="" ROUTER_CP=""
router_container() { docker ps "$@" --filter 'name=^/argus-router$' --format '{{.Names}}' 2>/dev/null | head -1 || true; }
router_env() {
  docker inspect argus-router --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
    | grep -E "^$1=" | tail -1 | cut -d= -f2- || true
}
# router_observe <port> — AC-D53: ONE reading of the router's health over the Docker API. Its own healthcheck asks
# /ready on its ROUTABLE address (docker-compose.router.yml) — never the host's port, never /healthz: /healthz is
# liveness, and it stayed green for 13 hours through a router whose state directory had been deleted (VR8-K2).
# A router started from a kit with no healthcheck is asked the same question from inside.
router_observe() {
  local info st health out
  info="$(docker inspect argus-router --format '{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>"$PROBE_ERR")" \
    || { probe_failed "docker inspect argus-router"; return 1; }
  IFS='|' read -r st health <<< "$info"
  [ "$st" = running ] || { health_note unhealthy "argus-router is $st"; return 1; }
  case "$health" in
    healthy) health_note healthy "its own healthcheck says healthy" ;;
    none)
      out="$(docker exec argus-router sh -c 'r="$(curl -fsS -m 3 --noproxy "*" "http://$(hostname):'"$1"'/ready" 2>&1)"; printf "ARGUS-PROBE rc=%s %s\n" "$?" "$r"' 2>"$PROBE_ERR")" || true
      case "$out" in
        *"ARGUS-PROBE rc=0 "*) health_note healthy "asked from inside: /ready answered" ;;
        *"ARGUS-PROBE rc="*) health_note unhealthy "asked from inside argus-router, /ready on port $1 failed: ${out#*ARGUS-PROBE }"; return 1 ;;
        *) probe_failed "docker exec argus-router"; return 1 ;;
      esac ;;
    starting) health_note starting "its own healthcheck says starting"; return 1 ;;
    *) health_note unhealthy "its own healthcheck says $health$(container_health_log argus-router)"; return 1 ;;
  esac
}
router_healthy() { # <port> <seconds> — three states in HEALTH_STATE, and "starting" decided as executor_healthy does
  local deadline seen="" seen_detail="" last=""
  deadline=$(( $(date +%s) + $2 ))
  while :; do
    if router_observe "$1"; then return 0; fi
    last="$HEALTH_STATE"
    if [ "$HEALTH_STATE" = unhealthy ]; then seen=1; seen_detail="$HEALTH_DETAIL"; fi
    [ "$(date +%s)" -lt "$deadline" ] || break
    sleep 3
  done
  if [ -n "$seen" ]; then health_note unhealthy "$seen_detail"
  elif [ "$last" = starting ]; then
    health_note unhealthy "its own healthcheck never reported healthy within ${2}s (it still says starting)"
  fi
  return 1
}
router_compose_up() { # <image> — the state dir is the one the block computed (C-5), never read back from docker
  ARGUS_ROUTER_IMAGE="$1" \
  ARGUS_ROUTER_STATE_HOST="$(hostpath "$ROUTER_STATE")" \
  ARGUS_ROUTER_PORT="$ROUTER_PORT" \
  ARGUS_ROUTER_HOST="$ROUTER_HOST_NAME" \
  ARGUS_ROUTER_HOST_ALIAS="${ROUTER_HOST_ALIAS:-host.docker.internal}" \
  ARGUS_CP_URL="$ROUTER_CP" \
    docker compose -f "$(hostpath "$ROUTER_COMPOSE")" -p argus-router up -d
}
move_router() {
  local cur
  if [ ! -f "$ROUTER_COMPOSE" ]; then skip_step "the new kit has no docker-compose.router.yml — the router was left as it is"; return 0; fi
  if [ -z "$(router_container)" ]; then
    skip_step "no running argus-router on this machine — an update starts nothing that was not running"; return 0
  fi
  cur="$(docker inspect argus-router --format '{{.Config.Image}}' 2>/dev/null || true)"
  case "$(router_skew_decision "$IMAGE_DIGEST" "$cur" @ROLLBACK@)" in
    keep-rollback) skip_step "this is a rollback, and the machine router is shared by every agent folder here — rolling one instance back does not roll the machine back"; return 0 ;;
    keep-newer)    skip_step "the machine router already runs a newer version than this update — it runs the highest version any instance here needs"; return 0 ;;
    # the library's contract for this answer is "MOVE, and say why" (router-version.sh) — never silently
    move-uncomparable) printf '   NOTE: the version of the running router or of the new image could not be read, so "never lower" could not be checked; the router is moved, as onboarding moves it\n' ;;
  esac
  if [ "$cur" = "$IMAGE_DIGEST" ]; then printf '   the router already runs %s\n' "$cur"; return 0; fi
  # the port is STICKY and already in every folder's .mcp.json: read, never guessed (update.sh:215-222)
  ROUTER_PORT="$(docker port argus-router 2>/dev/null | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -1 || true)"
  if [ -z "$ROUTER_PORT" ]; then
    skip_step "could not read the port argus-router is published on — a router brought back on a guessed port is invisible to every agent folder, so it was not moved"; return 0
  fi
  ROUTER_HOST_NAME="$(router_env ARGUS_ROUTER_HOST)"
  ROUTER_HOST_ALIAS="$(router_env ARGUS_ROUTER_HOST_ALIAS)"
  ROUTER_CP="$(router_env ARGUS_CP_URL)"
  PREV_ROUTER_IMAGE="$cur"
  router_compose_up "$IMAGE_DIGEST" || return 1
  if ! router_healthy "$ROUTER_PORT" "$HEALTH_BUDGET"; then
    health_says "after moving it to $IMAGE_DIGEST" "argus-router"
    return 1
  fi
  printf '   argus-router now on %s (port %s)\n' "$IMAGE_DIGEST" "$ROUTER_PORT"
  # VR6-R3: the move is ANNOUNCED in the one sentence onboarding prints too (router-version.sh)
  printf '   %s\n' "$(router_move_line "$PREV_ROUTER_IMAGE" "$IMAGE_DIGEST")"
}
`

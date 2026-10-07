package updatecmd

import (
	"fmt"
	"strings"
)

// discover.go — V31-001 (VR13-UP): PHASE 1's host half.
//
// ⛔ C-1 — THE CONTAINER CANNOT LOOK AT THE MACHINE. The executor image has no docker CLI, no
// kubectl and no compose plugin (Dockerfile.onedroid-argus-execution-plane:88), so "what is installed
// here?" is a question it can only ask by writing a script for the host to run. `update discover`
// writes that script; the operator runs it; it writes `$STAGE/observed.json`, which is the ONLY thing
// the planner learns about the machine from.
//
// ⛔ SA-D27 — THE SCRIPT CARRIES LITERALS, NOT THE BLOCK'S SHELL VARIABLES.
//
// ⛔ AND IT ASKS BY THE NAMES ONBOARDING GIVES. The first version listed services of a project called
// `argus-<id>` without `-p`, inspected containers called `<svc>-<id>`, and read a namespace called
// `argus-<id>` — three names no onboarding has produced. On a real machine it would have written an
// observation with no services and no objects, which the planner refuses as empty: a dead update path
// that looked, from its tests, like a working one.

// DiscoverInput is what `update discover` is given. Everything in it becomes a literal in the script.
type DiscoverInput struct {
	InstanceID  string
	Tier        string
	KitDir      string
	StageDir    string
	KubeContext string
	Kubeconfig  string
	CPURL       string
	// Folders come from the ROUTER's own records via the /state:ro mount — never a second list, which
	// would drift from the one the router actually serves. Only folders wired to THIS instance.
	Folders []Folder
	// GrafanaURL is probed for document presence; empty skips that section.
	GrafanaURL string
}

// executorPodSelector is the label the rendered executor Deployment selects its pods by (internal/k8srender
// RenderExecutor; pinned by TestDiscover_TheExecutorPodSelectorIsTheRenderedDeployments). A LABEL — the Deployment
// itself is named "executor".
const executorPodSelector = "app.kubernetes.io/name=argus-executor"

// VersionNotOverridden is the text of `argus version`'s answer that says no ARGUS_VERSION override was in effect
// (cmd/argus/main.go, json.MarshalIndent). discover keeps a version only from an answer holding it, matched as text in
// bash; cmd/argus TestVersion_ACD53_TheAnswerSaysWhetherItWasOverriddenAsDiscoverMatchesIt pins it to the binary.
const VersionNotOverridden = `"overridden": false`

// RenderDiscover writes the host script that produces observed.json.
//
// ⛔ THE ENV SECTION IS AN ALLOW-LIST, ENFORCED IN BASH (SA-D18). Only the keys in EnvValueAllowed
// carry their value; every other key is emitted as a NAME with an empty value, and a key nobody has
// classified is denied by default.
func RenderDiscover(in DiscoverInput) string {
	var b strings.Builder
	b.WriteString(scriptHeader)
	fmt.Fprintf(&b, `
STAGE=%s
KIT=%s
INSTANCE=%s
TIER=%s
KUBE_CONTEXT=%s
KUBECONFIG_ARG=%s
GRAFANA=%s
ALLOW=%s
`, shq(MSYSPath(in.StageDir)), shq(MSYSPath(in.KitDir)), shq(in.InstanceID), shq(in.Tier), shq(in.KubeContext),
		shq(in.Kubeconfig), shq(in.GrafanaURL), shq(strings.Join(allowList(), " ")))

	b.WriteString(discoverPrelude)
	b.WriteString(discoverBody(in))
	return b.String()
}

// allowList is EnvValueAllowed as a stable, ordered list — the bash side and the Go side read the
// SAME definition, so one cannot drift from the other.
func allowList() []string {
	out := make([]string, 0, len(EnvValueAllowed))
	for k := range EnvValueAllowed {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

const discoverPrelude = `COMPOSE_DIR="$KIT/deploy/compose"
ENV_FILE="$KIT/deploy/compose/env.$INSTANCE"
PROJECT="argus-inst-$INSTANCE"   # onboard.sh:1148
NS="argus-inst-$INSTANCE"        # update.sh:292
OUT="$STAGE/observed.json"
# path conversion off for docker and kubectl ONLY — the global export breaks curl (see scripts.go)
docker()  { env MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' docker "$@"; }
kubectl() { env MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' kubectl "$@"; }
hostpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }
kube() {
  local a=()
  if [ -n "$KUBECONFIG_ARG" ]; then a+=(--kubeconfig "$(hostpath "$KUBECONFIG_ARG")"); fi
  if [ -n "$KUBE_CONTEXT" ]; then a+=(--context "$KUBE_CONTEXT"); fi
  kubectl "${a[@]}" "$@"
}

mkdir -p "$STAGE"
[ -f "$ENV_FILE" ] || { printf 'no env file for %s at %s — this kit did not onboard it\n' "$INSTANCE" "$ENV_FILE" >&2; exit 1; }

emit_env() {
  printf '  "env": {\n'
  local first=1 k v shown
  while IFS='=' read -r k v; do
    case "$k" in ''|'#'*) continue ;; esac
    shown=""
    case " $ALLOW " in *" $k "*) shown="$v" ;; esac
    [ $first -eq 1 ] || printf ',\n'
    first=0
    printf '    "%s": "%s"' "$k" "$(printf '%s' "$shown" | tr -d '\r' | sed 's/[\\"]/\\&/g')"
  done < "$ENV_FILE"
  printf '\n  },\n'
}
`

func discoverBody(in DiscoverInput) string {
	var b strings.Builder
	b.WriteString(`
{
  printf '{\n'
  printf '  "tier": "%s",\n' "$TIER"
`)
	if in.Tier == "compose" {
		// update.sh:375 finds the executor by LABEL; so does this, for every service of the project.
		b.WriteString(`  printf '  "compose": {"project": "%s", "services": [' "$PROJECT"
  first=1
  while IFS='|' read -r svc img cid; do
    [ -n "$svc" ] || continue
    case "$img" in
      *@sha256:*) dig="$img" ;;
      *)
        # ⛔ NEVER TRUST $img — IT CAN BE A LOCAL ALIAS (a leftover k3d-import tag, or one reused for
        # something else since). The container's OWN image id (docker inspect, never the tag it started
        # from) is the only thing that names what is actually running, so the repo digest is looked up
        # against THAT — a rollback built from an alias's own RepoDigests can name an image nobody can
        # pull back.
        imgid="$(docker inspect "$cid" --format '{{.Image}}' 2>/dev/null || true)"
        case "$imgid" in
          sha256:*) dig="$(docker image inspect --format '{{index .RepoDigests 0}}' "$imgid" 2>/dev/null || true)" ;;
          *) dig="" ;;
        esac ;;
    esac
    [ $first -eq 1 ] || printf ','
    first=0
    printf '{"name":"%s","image":"%s","digest":"%s"}' "$svc" "$img" "$dig"
  done < <(docker ps -a --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Label "com.docker.compose.service"}}|{{.Image}}|{{.ID}}' 2>/dev/null || true)
  printf ']},\n'
  printf '  "k8s": {},\n'
`)
	} else {
		b.WriteString(`  printf '  "compose": {},\n'
  printf '  "k8s": {"context": "%s", "namespace": "%s", "objects": [' "$KUBE_CONTEXT" "$NS"
  first=1
  while IFS='|' read -r kind name img; do
    [ -n "$name" ] || continue
    [ $first -eq 1 ] || printf ','
    first=0
    printf '{"kind":"%s","name":"%s","image":"%s","digest":"%s"}' "$kind" "$name" "$img" "$img"
  done < <(kube -n "$NS" get deploy,daemonset \
             -o jsonpath='{range .items[*]}{.kind}{"|"}{.metadata.name}{"|"}{.spec.template.spec.containers[0].image}{"\n"}{end}' 2>/dev/null || true)
  printf ']},\n'
`)
	}

	// ⭐ V32 Release QA (d): THE VERSION THE RUNNING EXECUTOR REPORTS, asked before anything moves. An
	// instance onboarded before 0.3.32 has no manifest, so this reading is the only record of where the
	// update takes it FROM — without it the rollback block can never render for any existing instance.
	// Asked the same way preflight asks runner-state (by compose label / deploy name), and every failure
	// leaves it EMPTY: an executor that cannot answer is not on any version we may write down.
	// ⛔ AC-D53 (round-5 review R5-GATE-1): THE BINARY IS ASKED FOR ITS OWN VERSION, WITH ARGUS_VERSION CLEARED. With the
	// override set, `argus version` reports that value (buildinfo.Resolve: a supported pin — k3d bakes it into the
	// Deployment at render, compose takes it from the shell at every `up`), which is not the version the executor runs.
	// Taken as a reading it became the not-confirmed marker's origin, and an override above the target then refused
	// every plan after one unconfirmed update; written as the version of an executor a run did not move, it refused
	// every update after. Cleared, the binary reports its own link-time stamp. And only an answer that SAYS it was not
	// overridden ("overridden": false — printed by every Argus image) is kept: a missing line, a clearing that did not
	// take, or no answer leaves the version unread.
	if in.Tier == "compose" {
		b.WriteString(`  ev_c="$(docker ps -q --filter "label=com.docker.compose.project=$PROJECT" --filter "label=com.docker.compose.service=executor" 2>/dev/null | head -1 || true)"
  ev_out=""
  if [ -n "$ev_c" ]; then
    ev_out="$(docker exec -e ARGUS_VERSION= "$ev_c" argus version 2>/dev/null || true)"
  fi
`)
	} else {
		// ⛔ AC-D53 (round-4 review G-7): A DEPLOYMENT IN THE MIDDLE OF A ROLLOUT HAS NO ONE VERSION. `exec deploy/…` asks
		// ONE pod — kubectl picks the one Ready longest, an OLD one while a rollout replaces them — while the image recorded
		// above is the Deployment SPEC's. Paired, they gave the marker's origin (and so `previous`) one release's version
		// with another's image. So the version is read only when the controller has seen the spec (observedGeneration =
		// generation) and every pod it counts runs it (updated = total, and total not 0) —'s rule
		// (internal/rollout Classify), NEVER a comparison with desired: a quota holding a pod back, or the park lag after a
		// scale-down, leaves every pod on the spec while desired differs (round-5 review R5-K8S-2). kubectl leaves a zero
		// status field OUT, so the fields are separated and each keeps its own place. Otherwise the version is left
		// unread — nothing is invented from it.
		// ⛔ AND NO POD IS STILL TERMINATING (round-5 review R5-K8S-1). The status counts only pods that are not
		// terminating (apps/v1 DeploymentStatus), so in the seconds an old pod drains after the last rollout step — at one
		// replica, right after `set image` — the check above passes while exec may still reach that old pod. So the
		// executor's pods are listed by the Deployment's selector (executorPodSelector), and
		// the version is read only when the listing answered with at least one pod and none has a deletionTimestamp.
		// With that, every pod exec can pick runs the spec.
		b.WriteString(`  ev_out=""
  IFS='|' read -r ro_gen ro_obs ro_upd ro_tot <<<"$(kube -n "$NS" get deploy executor -o 'jsonpath={.metadata.generation}|{.status.observedGeneration}|{.status.updatedReplicas}|{.status.replicas}' 2>/dev/null || true)"
  if [ -n "$ro_gen" ] && [ "$ro_gen" = "$ro_obs" ] && [ -n "$ro_tot" ] && [ "$ro_tot" != 0 ] && [ "$ro_upd" = "$ro_tot" ]; then
    ro_pods="$(kube -n "$NS" get pods -l ` + executorPodSelector + ` -o 'jsonpath={range .items[*]}{.metadata.deletionTimestamp}{"|"}{.metadata.name}{"\n"}{end}' 2>/dev/null)" || ro_pods=""
    ro_term=""
    while IFS='|' read -r ro_del _; do if [ -n "$ro_del" ]; then ro_term=1; fi; done <<<"$ro_pods"
    if [ -n "$ro_pods" ] && [ -z "$ro_term" ]; then
      ev_out="$(kube -n "$NS" exec deploy/executor -- env ARGUS_VERSION= argus version 2>/dev/null || true)"
    fi
  fi
`)
	}
	b.WriteString(`  ev=""
  case "$ev_out" in *'` + VersionNotOverridden + `'*) ev="$(printf '%s\n' "$ev_out" | sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' | head -1 || true)" ;; esac
  case "$ev" in *[!0-9A-Za-z.+-]*) ev="" ;; esac   # anything JSON would need escaped is not a version
  printf '  "executor_version": "%s",\n' "$ev"
`)

	// ⭐ V32 Release QA defect Q1: THE MACHINE ROUTER AS IT RUNS, before anything moves — one host container per
	// machine, the same on every tier. A router A-7 does not move (every rollback, a router already newer) is
	// recorded from this reading; without it the manifest recorded that router at the TARGET image. A router
	// that does not answer leaves both EMPTY. Its version is asked as the executor's is: with ARGUS_VERSION cleared
	// (the router's compose file passes the shell's through), and kept only from an answer that says it was not
	// overridden (AC-D53).
	b.WriteString(`  rimg="$(docker inspect argus-router --format '{{.Config.Image}}' 2>/dev/null || true)"
  case "$rimg" in *[!0-9A-Za-z.:/@_+-]*) rimg="" ;; esac
  rver="" rout=""
  if [ -n "$rimg" ]; then
    rout="$(docker exec -e ARGUS_VERSION= argus-router argus version 2>/dev/null || true)"
  fi
  case "$rout" in *'` + VersionNotOverridden + `'*) rver="$(printf '%s\n' "$rout" | sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' | head -1 || true)" ;; esac
  case "$rver" in *[!0-9A-Za-z.+-]*) rver="" ;; esac
  printf '  "router": {"image": "%s", "version": "%s"},\n' "$rimg" "$rver"
`)

	// is this an `--obs none` instance? Read off the env file, because emit_env records a NAME
	// only for a key that is not allow-listed, and a name cannot tell ARGUS_OBS_LOKI= (set, empty: what `--obs
	// none` onboarding on compose writes) from ARGUS_OBS_LOKI=http://... (an operator's own) or from unset. Like
	// compose, the LAST line of a name wins. ARGUS_OBS_MODE=none is the mode itself, written beside the empties
	// by the same onboarding. The planner skips A-4 for such an instance, so nothing below asks Grafana about it.
	b.WriteString(`  obs_none=false
  _obs_loki="$(tr -d '\r' < "$ENV_FILE" | grep -E '^ARGUS_OBS_LOKI=' | tail -1 || true)"
  _obs_mode="$(tr -d '\r' < "$ENV_FILE" | grep -E '^ARGUS_OBS_MODE=' | tail -1 || true)"
  if [ "$_obs_loki" = "ARGUS_OBS_LOKI=" ] || [ "$_obs_mode" = "ARGUS_OBS_MODE=none" ]; then obs_none=true; fi
  printf '  "obs_none": %s,\n' "$obs_none"
`)
	b.WriteString("  emit_env\n")

	// the agent folders wired to this instance, from the ROUTER's records
	b.WriteString("  printf '  \"folders\": ['\n  first=1\n")
	for _, f := range in.Folders {
		path := MixedPath(f.Path)
		b.WriteString("  { d=" + shq(path) + "; h=" + shq(f.Hat) + `
    [ $first -eq 1 ] || printf ','
    first=0
    if [ -d "$d" ] && [ -w "$d" ]; then w=true; else w=false; fi
    printf '{"path":"%s","hat":"%s","writable":%s}' "$d" "$h" "$w"
  }
`)
	}
	b.WriteString(`  printf '],\n'

  # ⛔ ABSENT IS A STATE (INT-039: social-aks-v1 has no dashboard at all) — BUT ONLY A 404 IS ABSENT.
  # A read that got no real answer is neither present nor absent, so it marks Grafana UNREACHABLE and
  # the planner skips A-4 with that reason, instead of planning an undo that deletes a live document.
  doc_code() { # <path>
    local c
    c="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$GRAFANA$1" 2>/dev/null)" || true
    case "$c" in ''|*[!0-9]*) c=000 ;; esac
    printf '%s' "$c"
  }
  reach=false; dpres=false; spres=false
  if [ -n "$GRAFANA" ] && [ "$obs_none" != true ]; then
    hc="$(doc_code /api/health)"
    dcode="$(doc_code "/api/dashboards/uid/argus-overview-$INSTANCE")"
    scode="$(doc_code "/api/datasources/uid/loki-$INSTANCE")"
    case "$hc:$dcode:$scode" in 200:200:200|200:200:404|200:404:200|200:404:404) reach=true ;; esac
    if [ "$dcode" = 200 ]; then dpres=true; fi
    if [ "$scode" = 200 ]; then spres=true; fi
  fi
  printf '  "grafana": {"reachable": %s, "dashboard": {"present": %s, "uid": "argus-overview-%s"}, "datasource": {"present": %s, "uid": "loki-%s"}},\n' \
    "$reach" "$dpres" "$INSTANCE" "$spres" "$INSTANCE"

  port="$(grep -E '^ARGUS_MCP_PORT=' "$ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '\r' || true)"
  printf '  "healthz": "%s"\n' "$(curl -fsS --max-time 5 "http://localhost:${port:-8765}/healthz" >/dev/null 2>&1 && printf 'ok' || printf 'unreachable')"
  printf '}\n'
} > "$OUT"

printf 'wrote %s\n' "$OUT"
`)
	return b.String()
}

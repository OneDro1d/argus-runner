package updatecmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// apply_exec_test.go — V31-001 (VR13-UP), P-11 BULLET 13: THE FAILURE-INJECTION MATRIX.
//
// ⭐ EVERY OTHER TEST IN THIS PACKAGE READS THE SCRIPT. This one RUNS it — real bash, real `mv`/`cp`,
// and THE KIT'S REAL LIBRARIES (auth-curl.sh, k3d-image.sh, grafana-refs.sh, router-version.sh, copied
// from onboarding/lib into the staged kit). Only the programs the host owns — docker, kubectl, curl —
// are stubs, and they answer the way those programs answer: compose LABEL queries return a container
// id, `-w '%{http_code}'` prints a code, `docker port` prints a mapping.
//
// ⛔ WHY THE STUBS ANSWER. The first matrix's stubs printed nothing and exited 0, so every lookup came
// back empty and the scripts' real branches were never taken — while the scripts addressed names no
// machine has. A stub that answers nothing tests the error paths of a machine that does not exist.

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash is not on PATH: %v", err)
	}
}

// stubBin writes docker/kubectl/curl stubs. `failing` maps a program to an argv substring that makes
// THAT call exit 1; every call is appended to $CALLS.
func stubBin(t *testing.T, dir string, failing map[string]string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	answers := map[string]string{
		// docker-down, when it holds text, is the runtime's OWN words for "I could not be reached" — the forms
		// measured 2026-09-30 are in builds/AC-D53/02-measurements (and TestAC_D53_TheRuntimesOwnWords). Empty,
		// it is the Hyper-V socket timeout measured on this estate earlier.
		"docker": `if [ -f "$STUB_ROOT/docker-down" ]; then
  if [ -s "$STUB_ROOT/docker-down" ]; then cat "$STUB_ROOT/docker-down" >&2
  else echo "docker: Error response from daemon: failed to connect to the backend: timed out dialing Hyper-V socket" >&2; fi
  exit 1
fi
# proxy-env: docker compose copied ~/.docker/config.json's proxies into the container's env, so an inside curl
# without --noproxy asks the proxy for the container's own name (curl 8.14.1's words, measured 2026-09-30)
if [ -f "$STUB_ROOT/proxy-env" ]; then
  case "$*" in
    *"--noproxy"*) ;;
    *"exec cid-executor sh -c"*|*"exec argus-router sh -c"*) echo 'ARGUS-PROBE rc=5 curl: (5) Could not resolve proxy: proxy.corp'; exit 0 ;;
  esac
fi
# AC-D55: THE COMPOSE STUB OBEYS COMPOSE'S OWN PRECEDENCE. A variable set in the SHELL beats the same variable in
# --env-file (documented Compose behaviour, measured on orderservice-compose 2026-09-30), so the image the stub
# "starts" is the shell's when the shell carries one. The NAME of every ARGUS_/PROMTAIL_ variable a compose call
# for an instance project SEES is logged (names only, never values), so a test asserts what reached compose and
# not only what the env file says.
if [ "$1" = compose ]; then
  case "$*" in
    *"-p argus-inst-"*) compgen -e | grep -E '^(ARGUS|PROMTAIL)_' | sort | sed 's/^/compose-env /' >> "$CALLS" ;;
  esac
  case "$*" in
    *"--force-recreate executor"*)
      ef=""; prev=""
      for a in "$@"; do [ "$prev" = --env-file ] && ef="$a"; prev="$a"; done
      img="${ARGUS_MCP_IMAGE:-}"
      [ -n "$img" ] || img="$(grep '^ARGUS_MCP_IMAGE=' "$ef" | tail -1 | cut -d= -f2-)"
      case "$img" in
        *sha256:new) [ -f "$STUB_ROOT/wrong-on-forward" ] && img=ghcr.io/x/exec@sha256:wrong ;;
        # AC-D56's undo-image-mismatch and AC-D55's wrong-on-undo are the same fault seen by two tests
        *sha256:old) { [ -f "$STUB_ROOT/wrong-on-undo" ] || [ -f "$STUB_ROOT/undo-image-mismatch" ]; } && img=ghcr.io/x/exec@sha256:wrong ;;
      esac
      printf '%s' "$img" > "$STUB_ROOT/running-image" ;;
  esac
fi
case "$*" in
  # commit-writes-then-125: the commit ran — wrote the record and removed the not-confirmed marker, as CommitManifest
  # does — and then the docker CLI exited 125 anyway (its "Error waiting for container" path), so runner() retries
  *"update commit"*)
    if [ -f "$STUB_ROOT/commit-writes-then-125" ]; then
      rm -f "$STUB_ROOT/state/installed/i1.unconfirmed"
      echo 'docker: Error waiting for container: context canceled' >&2; exit 125
    fi
    # commit-125: docker exits 125 and the marker is left — whether the verb ran nobody can tell from here
    if [ -f "$STUB_ROOT/commit-125" ]; then
      echo 'docker: Error waiting for container: context canceled' >&2; exit 125
    fi ;;
  # rehash-125-once: the re-hash's first attempt exits 125, its retry succeeds. outcome-at-rehash: records what the
  # stage's outcome file held when A-8a's re-hash began (the words written BEFORE A-8a)
  *"update rehash"*)
    if [ -f "$STUB_ROOT/outcome-at-rehash" ]; then cat "$STUB_ROOT/stage/outcome" > "$STUB_ROOT/outcome-at-rehash" 2>/dev/null || : > "$STUB_ROOT/outcome-at-rehash"; fi
    if [ -f "$STUB_ROOT/rehash-125-once" ]; then
      rm -f "$STUB_ROOT/rehash-125-once"
      echo 'docker: Error response from daemon: invalid mode: /c/stage' >&2; exit 125
    fi ;;
  # ── AC-D53: THE EXECUTOR'S HEALTH, READ OVER THE DOCKER API ONLY ─────────────────────────────────────────
  # The container runs the image the compose block above wrote to running-image for the LAST ` + "`--force-recreate executor`" + `
  # (the old image before any recreate). down-after-recreate: the runtime stops answering right after that recreate
  # (criterion d).
  *"--force-recreate executor"*)
    # at-recreate: what the stage and the record said at the MOMENT of each move — so a test can see that A-3 was
    # recorded unconfirmed, and the record marked, BEFORE the executor changed, not only by a later undo
    { echo "--- recreate"; cat "$STUB_ROOT/stage/progress.json" 2>/dev/null; [ -f "$STUB_ROOT/state/installed/i1.unconfirmed" ] && echo "MARKER-PRESENT"; grep -h '^at=' "$STUB_ROOT/state/installed/i1.unconfirmed" 2>/dev/null; } >> "$STUB_ROOT/at-recreate"
    # AC-D56: a recreate of the executor is counted; the UNDO's is the second. undo-recreate-fails makes it fail, in
    # words that are not the runtime's own for "could not be reached" — a genuine failure
    n=$(( $(cat "$STUB_ROOT/recreates" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$STUB_ROOT/recreates"
    if [ -f "$STUB_ROOT/undo-recreate-fails" ] && [ "$n" -ge 2 ]; then echo "INJECTED undo recreate failure" >&2; exit 1; fi
    # exported-image: every recreate lands on this image, whatever the env file says. Before AC-D55's scrub (dc() in
    # scripts.go) an ARGUS_MCP_IMAGE exported in the operator's shell did exactly this (measured 2026-09-30, compose
    # v5.1.4); it stays as the stand-in for any recreate that lands on another image than the one asked for
    if [ -f "$STUB_ROOT/exported-image" ]; then cp "$STUB_ROOT/exported-image" "$STUB_ROOT/running-image"; fi
    if [ -f "$STUB_ROOT/down-after-recreate" ]; then cp "$STUB_ROOT/down-after-recreate" "$STUB_ROOT/docker-down"; fi
    # down-after-recreate-n: the runtime stops answering right after the N-th recreate (2 = the undo's, after a
    # forward move that was confirmed), in the words of down-words
    if [ -f "$STUB_ROOT/down-after-recreate-n" ]; then
      n="$(cat "$STUB_ROOT/down-after-recreate-n")"; n=$((n - 1)); printf '%s\n' "$n" > "$STUB_ROOT/down-after-recreate-n"
      if [ "$n" -le 0 ]; then cp "$STUB_ROOT/down-words" "$STUB_ROOT/docker-down"; rm -f "$STUB_ROOT/down-after-recreate-n"; fi
    fi ;;
  # down-at-promtail: the runtime dies DURING A-4's promtail recreate (that call fails in its words, and so does every
  # call after it) — A-3 was already moved and confirmed
  *"--force-recreate --no-deps promtail"*)
    if [ -f "$STUB_ROOT/down-at-promtail" ]; then cp "$STUB_ROOT/down-words" "$STUB_ROOT/docker-down"; cat "$STUB_ROOT/down-words" >&2; exit 1; fi ;;
  # one observation of the container: status | exit code | restarts | its own healthcheck | image id | image ref | published ports.
  # Every exec-<field> marker has an image-scoped form, exec-<field>-new / exec-<field>-old, so a test can break the NEW
  # image alone and let the undo bring back a healthy OLD one. exec-health: healthy (default) | unhealthy | starting |
  # none (a container created from a kit with no healthcheck — every instance in the estate today). exec-health-seq:
  # one word per observation, then the last word sticks (a healthcheck that starts, then reports).
  *"inspect cid-executor --format {{.State.Status}}|"*)
    img="$(cat "$STUB_ROOT/running-image" 2>/dev/null || true)"; [ -n "$img" ] || img=ghcr.io/x/exec@sha256:old
    [ -f "$STUB_ROOT/exec-old-image" ] && img=ghcr.io/x/exec@sha256:old
    tag="${img##*:}"; st=running; code=0; rs=0; health=healthy
    for f in status exit-code restarts health; do
      v=""; if [ -f "$STUB_ROOT/exec-$f-$tag" ]; then v="$(cat "$STUB_ROOT/exec-$f-$tag")"; elif [ -f "$STUB_ROOT/exec-$f" ]; then v="$(cat "$STUB_ROOT/exec-$f")"; fi
      [ -n "$v" ] || continue
      case "$f" in status) st="$v" ;; exit-code) code="$v" ;; restarts) rs="$v" ;; health) health="$v" ;; esac
    done
    if [ -f "$STUB_ROOT/exec-health-seq" ]; then
      set -- $(cat "$STUB_ROOT/exec-health-seq"); health="$1"; [ "$#" -gt 1 ] && shift; printf '%s\n' "$*" > "$STUB_ROOT/exec-health-seq"
    fi
    # down-after-readings: this many readings are ANSWERED, then the runtime stops answering (its words: down-words)
    if [ -f "$STUB_ROOT/down-after-readings" ]; then
      n="$(cat "$STUB_ROOT/down-after-readings")"; n=$((n - 1)); printf '%s\n' "$n" > "$STUB_ROOT/down-after-readings"
      if [ "$n" -le 0 ]; then cp "$STUB_ROOT/down-words" "$STUB_ROOT/docker-down"; rm -f "$STUB_ROOT/down-after-readings"; fi
    fi
    printf '%s|%s|%s|%s|sha256:id-%s|%s|8080/tcp,\n' "$st" "$code" "$rs" "$health" "$tag" "$img" ;;
  # the REAL shape: docker stores each check's Output with its own trailing newline, the template adds one and the
  # CLI one more — so the output ENDS IN A BLANK LINE (measured 2026-09-30 on a live router: "0: ready\n\n")
  *"inspect cid-executor --format {{range .State.Health.Log}}"*)
    printf '1: curl: (22) The requested URL returned error: 503\n\n' ;;
  # rung 4, asked from INSIDE the container; the probe's own verdict travels on stdout as ARGUS-PROBE rc=<curl's exit>
  *"exec cid-executor sh -c"*)
    img="$(cat "$STUB_ROOT/running-image" 2>/dev/null || true)"; [ -n "$img" ] || img=ghcr.io/x/exec@sha256:old
    if [ -f "$STUB_ROOT/exec-inside-fails" ] || [ -f "$STUB_ROOT/exec-inside-fails-${img##*:}" ]; then
      echo 'ARGUS-PROBE rc=7 curl: (7) Failed to connect to cid-executor port 8080 after 0 ms: Could not connect to server'
    else echo 'ARGUS-PROBE rc=0 {"status":"ok"}'; fi ;;
  "image inspect --format {{.Id}} "*) ref="${*##* }"; printf 'sha256:id-%s\n' "${ref##*:}" ;;
  # A-7: the machine router's own healthcheck (/ready on its routable address), and its inside fallback
  *"inspect argus-router --format {{.State.Status}}|"*)
    rh=healthy; [ -f "$STUB_ROOT/router-health" ] && rh="$(cat "$STUB_ROOT/router-health")"
    # router-down-after-readings: this many router readings are ANSWERED, then the runtime stops answering (down-words)
    if [ -f "$STUB_ROOT/router-down-after-readings" ]; then
      n="$(cat "$STUB_ROOT/router-down-after-readings")"; n=$((n - 1)); printf '%s\n' "$n" > "$STUB_ROOT/router-down-after-readings"
      if [ "$n" -le 0 ]; then cp "$STUB_ROOT/down-words" "$STUB_ROOT/docker-down"; rm -f "$STUB_ROOT/router-down-after-readings"; fi
    fi
    printf 'running|%s\n' "$rh" ;;
  *"inspect argus-router --format {{range .State.Health.Log}}"*)
    printf '1: curl: (22) The requested URL returned error: 503\n\n' ;;
  # /healthz is LIVENESS and answers ok whatever the router can serve — the 13-hour blind spot (VR8-K2); /ready is not
  *"exec argus-router sh -c"*)
    case "$*" in
      *"/ready"*) if [ -f "$STUB_ROOT/router-inside-fails" ]; then echo 'ARGUS-PROBE rc=22 curl: (22) The requested URL returned error: 503'
                  else echo 'ARGUS-PROBE rc=0 {"status":"ready"}'; fi ;;
      *) echo 'ARGUS-PROBE rc=0 ok' ;;
    esac ;;
  # the ROUTER's own version, settable so a test can tell a router reading from an executor reading; the default is
  # the answer every argus container gave before this case existed
  # router-override: the router runs with ARGUS_VERSION set, so its binary answers with that value unless the call
  # clears it (as the executor below)
  *"argus-router argus version"*) [ -f "$STUB_ROOT/no-version" ] && exit 1
    rv="$(cat "$STUB_ROOT/router-version" 2>/dev/null || echo 0.3.30)" ro=false rc=""
    case "$*" in *"-e ARGUS_VERSION= "*) rc=1 ;; esac
    [ -f "$STUB_ROOT/exec-env-ignored" ] && rc=""
    if [ -f "$STUB_ROOT/router-override" ] && [ -z "$rc" ]; then rv="$(cat "$STUB_ROOT/router-override")" ro=true; fi
    printf '{\n  "built": "2026-09-01T00:00:00Z",\n  "commit": "043e004",\n  "full": "%s (commit 043e004)",\n  "overridden": %s,\n  "version": "%s"\n}\n' "$rv" "$ro" "$rv" ;;
  # the executor's own "argus version", as json.MarshalIndent prints it (keys sorted: cmd/argus/main.go). Its binary's
  # stamp is executor-version (default 0.3.30). executor-override: the container runs with ARGUS_VERSION set, so the
  # binary answers with that value and "overridden": true unless the call clears it; exec-env-ignored: the clearing
  # does not take; version-no-field: an answer with no "overridden" line
  *"argus version"*) [ -f "$STUB_ROOT/no-version" ] && exit 1
    ev_v="$(cat "$STUB_ROOT/executor-version" 2>/dev/null || echo 0.3.30)" ev_o=false ev_clear=""
    case "$*" in *"-e ARGUS_VERSION= "*) ev_clear=1 ;; esac
    [ -f "$STUB_ROOT/exec-env-ignored" ] && ev_clear=""
    if [ -f "$STUB_ROOT/executor-override" ] && [ -z "$ev_clear" ]; then ev_v="$(cat "$STUB_ROOT/executor-override")" ev_o=true; fi
    if [ -f "$STUB_ROOT/version-no-field" ]; then printf '{\n  "full": "%s",\n  "version": "%s"\n}\n' "$ev_v" "$ev_v"
    else printf '{\n  "built": "2026-09-01T00:00:00Z",\n  "commit": "043e004",\n  "full": "%s (commit 043e004, built 2026-09-01T00:00:00Z)",\n  "overridden": %s,\n  "version": "%s"\n}\n' "$ev_v" "$ev_o" "$ev_v"; fi ;;
  *"argus runner-state"*) cat "$STUB_ROOT/runner-state.json" 2>/dev/null ;;
  *"com.docker.compose.service=executor"*) [ -f "$STUB_ROOT/no-executor" ] || echo cid-executor ;;
  *'name=^/argus-router$'*) [ -f "$STUB_ROOT/no-router" ] || echo argus-router ;;
  "ps --format {{.Names}}") [ -f "$STUB_ROOT/no-k3d-nodes" ] || echo k3d-memstore-server-0 ;;
  *"inspect argus-router --format {{.Config.Image}}"*) [ -f "$STUB_ROOT/no-router" ] && exit 1
    if [ -f "$STUB_ROOT/router-image" ]; then cat "$STUB_ROOT/router-image"; else echo ghcr.io/x/exec@sha256:old; fi ;;
  *"inspect argus-router --format {{range .Config.Env}}"*) printf 'ARGUS_ROUTER_HOST=DESKTOP-MEASURED\nARGUS_CP_URL=https://cp.measured\n' ;;
  # the image the executor was configured with BEFORE the move (move_executor's PREV_EXECUTOR_IMAGE). Only that read asks
  # it now: after the undo the image serving is read by rung 5 (the State inspect above), so undo-image-mismatch acts
  # through running-image, in the compose block at the top
  *"inspect cid-executor --format {{.Config.Image}}"*) echo ghcr.io/x/exec@sha256:old ;;
  *"inspect ghcr.io/x/exec@sha256:old --format {{index .Config.Labels"*) cat "$STUB_ROOT/label-old" 2>/dev/null ;;
  *"inspect ghcr.io/x/exec@sha256:new --format {{index .Config.Labels"*) cat "$STUB_ROOT/label-new" 2>/dev/null ;;
  # the router's port is STICKY and deliberately NOT onboarding's default 9765: a router brought back on the
  # default is the regression, so the default must not be what the stub answers
  *"port argus-router"*) echo "8080/tcp -> 0.0.0.0:9999" ;;
  "image inspect ghcr.io/x/exec@sha256:new") [ -f "$STUB_ROOT/image-absent" ] && exit 1 ;;
  # kube-down-at-pull: on k3d the API server runs in Docker too — a Docker restart during the pull takes both down
  "pull ghcr.io/x/exec@sha256:new") [ -f "$STUB_ROOT/kube-down-at-pull" ] && : > "$STUB_ROOT/kube-down"
    [ -f "$STUB_ROOT/pull-fails" ] && exit 1 ;;
  "image inspect ghcr.io/x/exec@sha256:old") [ -f "$STUB_ROOT/rollback-image-absent" ] && exit 1 ;;
  "pull ghcr.io/x/exec@sha256:old") [ -f "$STUB_ROOT/rollback-pull-fails" ] && exit 1 ;;
  # a compose project's containers, for discover's digest resolution: svc|image-as-started|container-id.
  # The image column may be a LOCAL ALIAS (a leftover k3d-import tag) rather than a registry reference.
  *"label=com.docker.compose.project=argus-inst-i1"*) [ -f "$STUB_ROOT/compose-services" ] && cat "$STUB_ROOT/compose-services" ;;
  *"inspect stub-exec-cid --format {{.Image}}"*) echo sha256:cafef00dcafef00dcafef00dcafef00dcafef00dcafef00dcafef00dcafef0 ;;
  "image inspect --format {{index .RepoDigests 0}} sha256:cafef00dcafef00dcafef00dcafef00dcafef00dcafef00dcafef00dcafef0") echo ghcr.io/x/exec@sha256:realnew ;;
  # the ALIAS TAG's own inspect answers something ELSE — proof that discover must never ask this.
  "image inspect --format {{index .RepoDigests 0}} argus-k3d-import:12345") echo ghcr.io/stale-alias/x@sha256:stale ;;
  # a k3d node's containerd: the import lands only when the save|import pipe really ran, and a node that
  # could not take it (import-miss) never lists the ref — the verified miss k3d-image.sh returns 1 for
  *"ctr --namespace k8s.io images import -"*) cat >/dev/null; [ -f "$STUB_ROOT/import-miss" ] || : > "$STUB_ROOT/imported" ;;
  *"ctr --namespace k8s.io images ls"*) [ -f "$STUB_ROOT/imported" ] && printf '%s application/vnd.oci.image.index.v1+json sha256:imported 100MiB linux/amd64 -\n' ghcr.io/x/exec@sha256:new ;;
  # AC-D47: the node's CRI — the index the kubelet asks, and now the verify's authority — answers as the real one
  # (shapes measured 2026-09-25 on k3d-argus): an id once the import landed, else "no such image" + exit 1, so an
  # import-miss stays a VERIFIED miss rather than a check that could not be performed.
  *"crictl inspecti"*) a="$*"
    if [ -f "$STUB_ROOT/imported" ]; then echo sha256:imported; else printf 'level=fatal msg="no such image \\"%s\\" present"\n' "${a##* }" >&2; exit 1; fi ;;
  # the router's environment reaches compose as PREFIX ASSIGNMENTS, which argv never shows — so it is logged
  *"-p argus-router up -d"*) printf 'router-env PORT=%s HOST=%s CP=%s STATE_HOST=%s IMAGE=%s\n' "$ARGUS_ROUTER_PORT" "$ARGUS_ROUTER_HOST" "$ARGUS_CP_URL" "$ARGUS_ROUTER_STATE_HOST" "$ARGUS_ROUTER_IMAGE" >> "$CALLS" ;;
esac
`,
		// kube-down, when present, is kubectl's own words for an API server it could not reach (measured 2026-09-30,
		// kubectl v1.36.2). AC-D53: kube-down-after-set-image makes the API stop answering right after the forward move.
		"kubectl": `if [ -f "$STUB_ROOT/kube-down" ]; then
  echo 'Unable to connect to the server: dial tcp 127.0.0.1:6443: connect: connection refused' >&2; exit 1
fi
# kube-401: the API server ANSWERS, and refuses this machine's credential (kubectl's words for an HTTP 401)
if [ -f "$STUB_ROOT/kube-401" ]; then
  echo 'error: You must be logged in to the server (Unauthorized)' >&2; exit 1
fi
case "$*" in
  *"set image deploy/executor"*)
    { echo "--- set image"; cat "$STUB_ROOT/stage/progress.json" 2>/dev/null; [ -f "$STUB_ROOT/state/installed/i1.unconfirmed" ] && echo "MARKER-PRESENT"; grep -h '^at=' "$STUB_ROOT/state/installed/i1.unconfirmed" 2>/dev/null; } >> "$STUB_ROOT/at-recreate"
    # AC-D56: every set image is counted; the UNDO's is the second (undo-image-mismatch below reads the count).
    # undo-set-image-fails: the undo's set image is REFUSED by an API server that answered — a genuine failure
    n=$(( $(cat "$STUB_ROOT/setimages" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$STUB_ROOT/setimages"
    if [ -f "$STUB_ROOT/undo-set-image-fails" ] && [ "$n" -ge 2 ]; then
      echo 'Error from server (Forbidden): deployments.apps "executor" is forbidden: User "argus" cannot patch resource "deployments" in API group "apps" in the namespace "argus-inst-i1"' >&2; exit 1
    fi
    if [ -f "$STUB_ROOT/kube-down-after-set-image" ]; then : > "$STUB_ROOT/kube-down"; fi
    if [ -f "$STUB_ROOT/kube-401-after-set-image" ]; then : > "$STUB_ROOT/kube-401"; fi
    # kube-401-after-set-image-n: the API refuses this machine's credential right after the N-th set image (2 = the undo's)
    if [ -f "$STUB_ROOT/kube-401-after-set-image-n" ]; then
      n="$(cat "$STUB_ROOT/kube-401-after-set-image-n")"; n=$((n - 1)); printf '%s\n' "$n" > "$STUB_ROOT/kube-401-after-set-image-n"
      if [ "$n" -le 0 ]; then : > "$STUB_ROOT/kube-401"; rm -f "$STUB_ROOT/kube-401-after-set-image-n"; fi
    fi
    # kube-down-after-set-image-n: the API stops answering right after the N-th set image (2 = the undo's)
    if [ -f "$STUB_ROOT/kube-down-after-set-image-n" ]; then
      n="$(cat "$STUB_ROOT/kube-down-after-set-image-n")"; n=$((n - 1)); printf '%s\n' "$n" > "$STUB_ROOT/kube-down-after-set-image-n"
      if [ "$n" -le 0 ]; then : > "$STUB_ROOT/kube-down"; rm -f "$STUB_ROOT/kube-down-after-set-image-n"; fi
    fi
    echo 'deployment.apps/executor image updated' ;;
  # rollout-stalled: an old pod still serving — the API answers throughout, the rollout does not finish (criterion g)
  *"rollout status deploy/executor"*)
    # rollout-slow: the watch takes a while before it answers (so two marks land in different seconds)
    [ -f "$STUB_ROOT/rollout-slow" ] && sleep 1.2
    if [ -f "$STUB_ROOT/rollout-stalled" ]; then
      # one line per change of the Deployment, as kubectl prints them — together longer than one_line's 400 chars
      echo 'Waiting for deployment spec update to be observed...'
      echo 'Waiting for deployment "executor" rollout to finish: 0 out of 1 new replicas have been updated...'
      echo 'Waiting for deployment "executor" rollout to finish: 1 old replicas are pending termination...'
      echo 'Waiting for deployment "executor" rollout to finish: 1 old replicas are pending termination...'
      echo 'Waiting for deployment "executor" rollout to finish: 1 old replicas are pending termination...'
      # kube-401-after-rollout: the credential is refused from the first stalled rollout on (so at the undo's set image)
      if [ -f "$STUB_ROOT/kube-401-after-rollout" ]; then : > "$STUB_ROOT/kube-401"; fi
      # kube-down-after-rollout: the API server stops answering right after the first stalled rollout has ANSWERED — so the
      # forward reading is seen (stalled), its replica-count read and the undo's set image then find the API down
      if [ -f "$STUB_ROOT/kube-down-after-rollout" ]; then : > "$STUB_ROOT/kube-down"; fi
      echo 'error: timed out waiting for the condition' >&2; exit 1
    fi
    echo 'deployment "executor" successfully rolled out' ;;
  *"get deploy executor -o jsonpath=desired="*) echo 'desired=1 ready=1 updated=1 total=2' ;;
  # discover's rollout state as kubectl prints this jsonpath: generation|observedGeneration|updatedReplicas|replicas,
  # a zero status field left out (omitempty) and no newline — rolled out unless rollout-state says otherwise
  # ("unreadable": the API answered with an error)
  # ⚠ the WHOLE command discover renders, its namespace included: any other shape gets no answer (round-6 review R6-POD-1)
  *"-n argus-inst-i1 get deploy executor -o jsonpath={.metadata.generation}|{.status.observedGeneration}|{.status.updatedReplicas}|{.status.replicas}"*)
    if [ -f "$STUB_ROOT/rollout-state" ]; then
      [ "$(cat "$STUB_ROOT/rollout-state")" = unreadable ] && { echo 'Error from server (NotFound): deployments.apps "executor" not found' >&2; exit 1; }
      cat "$STUB_ROOT/rollout-state"
    else printf '2|2|1|1'; fi ;;
  # the executor's pods by the Deployment's selector, one "deletionTimestamp|name" line each — one running pod unless
  # pods-state says otherwise ("unreadable": the API answered with an error). ⚠ The WHOLE command, as the rollout state
  # above: a listing with its fields swapped or in another namespace gets no answer (round-6 review R6-POD-1)
  *'-n argus-inst-i1 get pods -l app.kubernetes.io/name=argus-executor -o jsonpath={range .items[*]}{.metadata.deletionTimestamp}{"|"}{.metadata.name}{"\n"}{end}'*)
    if [ -f "$STUB_ROOT/pods-state" ]; then
      [ "$(cat "$STUB_ROOT/pods-state")" = unreadable ] && { echo 'Error from server (Forbidden): pods is forbidden' >&2; exit 1; }
      cat "$STUB_ROOT/pods-state"
    else printf '|executor-5c7d9-x2k4q\n'; fi ;;
  # discover's listing of the instance's objects, as rendered (round-7 review R7-3): the executor Deployment on the image
  # in k8s-executor-image (default ghcr.io/x/exec@sha256:old); any other shape gets no answer
  *'-n argus-inst-i1 get deploy,daemonset -o jsonpath={range .items[*]}{.kind}{"|"}{.metadata.name}{"|"}{.spec.template.spec.containers[0].image}{"\n"}{end}'*)
    printf 'Deployment|executor|%s\n' "$(cat "$STUB_ROOT/k8s-executor-image" 2>/dev/null || echo ghcr.io/x/exec@sha256:old)" ;;
  # the executor's "argus version", as in the docker stub — asked only as discover asks it, in the instance's namespace
  # with the override cleared by "env ARGUS_VERSION=" (round-7 review R7-3); exec-env-ignored: the clearing does not take
  *"-n argus-inst-i1 exec deploy/executor -- env ARGUS_VERSION= argus version"*) [ -f "$STUB_ROOT/no-version" ] && exit 1
    ev_v="$(cat "$STUB_ROOT/executor-version" 2>/dev/null || echo 0.3.30)" ev_o=false ev_clear=""
    case "$*" in *"env ARGUS_VERSION= argus version"*) ev_clear=1 ;; esac
    [ -f "$STUB_ROOT/exec-env-ignored" ] && ev_clear=""
    if [ -f "$STUB_ROOT/executor-override" ] && [ -z "$ev_clear" ]; then ev_v="$(cat "$STUB_ROOT/executor-override")" ev_o=true; fi
    if [ -f "$STUB_ROOT/version-no-field" ]; then printf '{\n  "full": "%s",\n  "version": "%s"\n}\n' "$ev_v" "$ev_v"
    else printf '{\n  "built": "2026-09-01T00:00:00Z",\n  "commit": "043e004",\n  "full": "%s (commit 043e004, built 2026-09-01T00:00:00Z)",\n  "overridden": %s,\n  "version": "%s"\n}\n' "$ev_v" "$ev_o" "$ev_v"; fi ;;
  *"argus runner-state"*) cat "$STUB_ROOT/runner-state.json" 2>/dev/null ;;
  # the image the Deployment's pod template names. After the undo's set image: undo-image-mismatch names another one;
  # undo-image-read-down makes the API stop answering for this read, in kubectl's own words. kube-read-noise: a read
  # that SUCCEEDS and still writes to stderr, as kubectl's klog discovery lines do when an aggregated API is not ready
  *"get deploy executor -o jsonpath"*)
    if [ "$(cat "$STUB_ROOT/setimages" 2>/dev/null || echo 0)" -ge 2 ] && [ -f "$STUB_ROOT/undo-image-read-down" ]; then
      echo 'Unable to connect to the server: dial tcp 127.0.0.1:6443: connect: connection refused' >&2; exit 1
    fi
    if [ -f "$STUB_ROOT/kube-read-noise" ]; then
      echo 'E1001 08:00:00.000000   12345 memcache.go:265] "Unhandled Error" err="couldn'"'"'t get current server API group list: the server is currently unable to handle the request (get metrics.k8s.io)"' >&2
    fi
    if [ -f "$STUB_ROOT/undo-image-mismatch" ] && [ "$(cat "$STUB_ROOT/setimages" 2>/dev/null || echo 0)" -ge 2 ]; then echo ghcr.io/x/exec@sha256:wrong
    else echo ghcr.io/x/exec@sha256:old; fi ;;
  *"get svc loki"*) echo 30100 ;;
  *"get svc pushgateway"*) echo 30091 ;;
esac
`,
		"curl": `case "$*" in *"/api/dashboards/db"*) cat > "$STUB_ROOT/dashboard-body.json" ;; esac
case "$*" in
  *"%{http_code}"*) if [ -f "$STUB_ROOT/grafana-500" ]; then printf 500; else printf 200; fi ;;
  *"/api/dashboards/uid/"*|*"/api/datasources/uid/"*) printf '{"stub":true}' ;;
esac
`,
	}
	for _, prog := range []string{"docker", "kubectl", "curl"} {
		body := "#!/usr/bin/env bash\nprintf '%s %s\\n' \"" + prog + "\" \"$*\" >> \"$CALLS\"\n"
		if pat, ok := failing[prog]; ok {
			// ⚠ the pattern is QUOTED: unquoted, spaces make it a bash syntax error — a stub failing for
			// the wrong reason that would still turn the test green.
			body += "case \"$*\" in\n  *\"" + pat + "\"*) printf 'INJECTED FAILURE: %s\\n' \"$*\" >&2; exit 1 ;;\nesac\n"
		}
		body += answers[prog] + "exit 0\n"
		if err := os.WriteFile(filepath.Join(bin, prog), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func stubEnv(t *testing.T, root string, failing map[string]string) []string {
	t.Helper()
	bin := stubBin(t, root, failing)
	return append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CALLS="+filepath.Join(root, "calls.log"),
		"STUB_ROOT="+root,
	)
}

// fakeMachine lays out a real kit, a staged kit carrying the REAL onboarding libraries, a router state
// directory and an agent folder that already holds an operator-edited skill.
func fakeMachine(t *testing.T, root, tier string) PlanInput {
	t.Helper()
	kit := filepath.Join(root, "kit")
	stage := filepath.Join(root, "stage")
	state := filepath.Join(root, "state")
	// ⛔ INSIDE THE KIT, as on orderservice-compose: PRODUCT_DIR is <kit>/product-agent. The first fixture
	// put the folder outside, which is how a kit swap that abandoned it passed.
	folder := filepath.Join(kit, "product-agent")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cd := filepath.Join(kit, "deploy", "compose")
	// the per-instance and machine-wide state A-1 must carry — each distinguishable by CONTENT
	write(filepath.Join(cd, "env.i1"), "ARGUS_MCP_IMAGE=ghcr.io/x/exec@sha256:old\nARGUS_MCP_PORT=8765\nARGUS_RUNNER_TOKEN=rt_live\n")
	write(filepath.Join(cd, "identity.i1.key"), "IDENTITY-I1")
	write(filepath.Join(cd, "cp-author.token"), "AUTHOR-TOKEN")
	write(filepath.Join(cd, "cp-session.token"), "SESSION-TOKEN")
	write(filepath.Join(cd, "sut-secrets.env"), "SUT_SECRET=x")
	write(filepath.Join(cd, "demo-sut.i1.project"), "compose:order-service")
	write(filepath.Join(cd, "promtail-config.i1.yaml"), "PROMTAIL-I1")
	write(filepath.Join(cd, "targets.d", "i1.json"), `{"targets":["i1"]}`)
	write(filepath.Join(cd, "targets.d", "other-kit-k3d.json"), `{"targets":["from another kit"]}`)
	write(filepath.Join(cd, "k8s-rendered", "i1", "executor.yaml"), "K8S-I1")
	// what no class list names: onboarding's logs, the agent folder's own config, and a file the bundle
	// ALSO ships (the bundle's copy must win)
	write(filepath.Join(kit, "logs", "onboard-transcript.txt"), "TRANSCRIPT")
	write(filepath.Join(folder, "argus-config.yaml"), "ARGUS-CONFIG")
	write(filepath.Join(cd, "docker-compose.yaml"), "# OLD docker-compose.yaml")

	// the staged kit: the image's bundle — which renders NO per-instance k8s set
	sk := filepath.Join(stage, "kit")
	write(filepath.Join(sk, "MARKER"), "NEW-KIT")
	for _, f := range []string{"docker-compose.yaml", "docker-compose.byo.yml", "docker-compose.byo-m3.yml", "docker-compose.router.yml"} {
		write(filepath.Join(sk, "deploy", "compose", f), "# staged "+f)
	}
	write(filepath.Join(sk, "deploy", "compose", "grafana", "dashboards", "argus-overview.json"),
		// the templating block carries the SHAPE of the real template: argus_instance first with an empty
		// current, and a later variable that ALSO has an empty current
		`{"uid": "argus-overview", "title": "Argus Overview (Argus M2)", "panels": [{"datasource": {"uid": "loki"}}],
  "templating": {"list": [
    {
      "name": "argus_instance",
      "type": "query",
      "query": "label_values(argus_scenarios_total, argus_instance)",
      "refresh": 2,
      "current": {}
    },
    {
      "name": "current_run",
      "type": "query",
      "refresh": 2,
      "current": {}
    }
  ]}}`)
	for _, lib := range []string{"auth-curl.sh", "k3d-image.sh", "grafana-refs.sh", "router-version.sh"} {
		write(filepath.Join(sk, "onboarding", "lib", lib), repoFile(t, "onboarding/lib/"+lib))
	}
	for _, s := range []string{"scenario-runner", "failure-triage", "scenario-author"} {
		write(filepath.Join(sk, "skills", s, "SKILL.md"), "NEW-"+s)
	}
	if err := os.MkdirAll(filepath.Join(stage, "undo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(folder, ".claude", "skills", "scenario-runner", "SKILL.md"), "OPERATOR-EDITED-RUNNER")
	writeRunnerState(t, root, "")

	in := fixtureInput(tier)
	in.KitDir, in.StageDir, in.RouterState = kit, stage, state
	in.KitInstances = []string{"i1"}
	in.Observed.Folders = []Folder{{Path: folder, Hat: "product", Writable: true}}
	in.HealthBudget = 2
	if tier != "compose" {
		in.KubeContext = "k3d-memstore"
	}
	return in
}

// runApply runs the REAL preflight on a clean machine, then `prep` (what breaks after preflight
// passed), then apply.sh with the injected failures.
//
// ⛔ THE PREFLIGHT IS RUN, NOT FAKED. A hand-written preflight.json captured no Grafana documents, so
// A-4's undo had nothing to re-post and "passed" — the matrix was proving an undo on a machine state
// the real sequence never produces.
func runApply(t *testing.T, in PlanInput, failing map[string]string, prep func(PlanInput)) (string, int) {
	t.Helper()
	return runApplyWith(t, in, failing, prep, applyOpts{})
}

// applyOpts is how a test changes the operator's SHELL around the scripts. shellEnv, when set, REPLACES every
// ARGUS_*/PROMTAIL_* variable of the test process with exactly these (so a reviewer's own exports can neither
// hide nor fake the defect); pathPre is put in front of PATH (the seams for uname and id).
type applyOpts struct {
	shellEnv []string
	pathPre  string
}

func applyEnv(t *testing.T, root string, failing map[string]string, o applyOpts) []string {
	t.Helper()
	env := stubEnv(t, root, failing)
	if o.shellEnv != nil {
		var kept []string
		for _, kv := range env {
			if strings.HasPrefix(kv, "ARGUS_") || strings.HasPrefix(kv, "PROMTAIL_") {
				continue
			}
			kept = append(kept, kv)
		}
		env = append(kept, o.shellEnv...)
	}
	if o.pathPre != "" {
		path := ""
		for _, kv := range env {
			if strings.HasPrefix(kv, "PATH=") {
				path = strings.TrimPrefix(kv, "PATH=")
			}
		}
		env = append(env, "PATH="+o.pathPre+string(os.PathListSeparator)+path)
	}
	return env
}

func runApplyWith(t *testing.T, in PlanInput, failing map[string]string, prep func(PlanInput), o applyOpts) (string, int) {
	t.Helper()
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(in.KitDir)
	pre := filepath.Join(root, "preflight.sh")
	if err := os.WriteFile(pre, []byte(RenderPreflight(p, in)), 0o700); err != nil {
		t.Fatal(err)
	}
	pc := exec.Command("bash", pre)
	pc.Dir = root
	pc.Env = applyEnv(t, root, nil, o)
	if out, err := pc.CombinedOutput(); err != nil {
		t.Fatalf("the preflight of a clean fake machine failed: %v\n%s", err, out)
	}
	if prep != nil {
		prep(in)
	}
	apply := filepath.Join(root, "apply.sh")
	if err := os.WriteFile(apply, []byte(RenderApply(p, in)), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", apply)
	cmd.Dir = root
	cmd.Env = applyEnv(t, root, failing, o)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("could not run apply.sh: %v\n%s", err, out)
	}
	return string(out), code
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

func lastWord(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\r\n \t"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// VR10-O3 (V28-013) — THE k3d TUNNEL IS TOLD WHICH CONTEXT TO USE, ON EVERY TIER AND AT BOTH START
// SITES, AND IT REFUSES TO START ON A GUESS.
//
// ── THE DEFECT ───────────────────────────────────────────────────────────────────────────────────
//
// The port-forward container was given a kubeconfig, a server address and a namespace — never a
// CONTEXT. So it presented the credentials of whatever `kubectl config current-context` was AT THE
// MOMENT IT STARTED, and every k3d tunnel crash-looped with "You must be logged in to the server"
// the day the machine default was switched to the managed cluster. The tunnel had always worked by
// accident; nothing announced the dependency.
//
// ── WHAT IS PINNED HERE, AND HOW ─────────────────────────────────────────────────────────────────
//
//  1. the template carries the context argument beside --kubeconfig, with the `:---v=0` no-op
//     default the two per-tier args already use (a compose command list cannot hold a
//     conditionally-absent element; an empty string would reach kubectl as an empty argument)
//  2. BOTH `_tunnel_compose_up` call sites — the normal path and the conflict-retry path — pass it
//     (the function was extracted precisely so the rare path could not drift from the common one)
//  3. tunnel_up REFUSES when the context is unknown, before any container exists
//  4. the context is pinned on EVERY tier — the apiserver/TLS args stay k3d-only, the context does not
//  5. after the start the forwarded port is PROBED (≤10 s) and a failure is named: the port, the
//     container, its state — not discovered by a scenario hours later
//
// 3-5 are RUN, not read: tunnel_up is extracted from onboard.sh and executed against a docker/curl
// stub, and the assertions are about what it did.
func tunnelRepoFile(t *testing.T, rel ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", ".."}, rel...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// extractShellFn returns `name() { … }` from a script, by its own landmark.
func extractShellFn(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "\n"+name+"() {")
	if i < 0 {
		t.Fatalf("no function %s() in the script — re-point this test deliberately", name)
	}
	i++ // past the newline
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("could not find the end of %s()", name)
	}
	return src[i : i+j+3]
}

// tunnelUpHarness extracts tunnel_up and every _tunnel_* helper it calls (except the interactive
// conflict resolver, stubbed to refuse), puts a docker + curl stub on PATH, and runs ONE tunnel_up.
type tunnelUpResult struct {
	out     string
	rc      int
	tunnErr string
	log     string
	elapsed time.Duration
}

func runTunnelUp(t *testing.T, tier, kubeContext, inst, port string, curlAnswers bool, bound time.Duration) tunnelUpResult {
	t.Helper()
	src := tunnelRepoFile(t, "onboarding", "onboard.sh")

	var fns []string
	fnRe := regexp.MustCompile(`(?m)^(_tunnel_[a-z_]+|tunnel_up)\(\) \{`)
	for _, m := range fnRe.FindAllStringSubmatch(src, -1) {
		if m[1] == "_tunnel_resolve_conflict" {
			continue
		}
		fns = append(fns, extractShellFn(t, src, m[1]))
	}
	if len(fns) < 2 {
		t.Fatalf("expected at least _tunnel_compose_up and tunnel_up, extracted %d functions", len(fns))
	}

	stubDir := t.TempDir()
	logPath := filepath.ToSlash(filepath.Join(stubDir, "stub.log"))
	dockerStub := `#!/usr/bin/env bash
printf 'ARGV %s\n' "$*" >>"$STUB_LOG"
case "$*" in
  compose*)
    printf 'ENV ctx=[%s] api=[%s] tls=[%s]\n' "${ARGUS_TUNNEL_CONTEXT_ARG-UNSET}" \
      "${ARGUS_TUNNEL_APISERVER_ARG-UNSET}" "${ARGUS_TUNNEL_TLS_ARG-UNSET}" >>"$STUB_LOG"
    exit 0 ;;
  "port k3d-argus-serverlb"*) echo "6443/tcp -> 0.0.0.0:52655"; exit 0 ;;
  inspect*) echo "restarting (restarts: 7)"; exit 0 ;;
  logs*)    echo "error: You must be logged in to the server (Unauthorized)"; exit 0 ;;
esac
exit 0
`
	curlStub := `#!/usr/bin/env bash
printf 'CURL %s\n' "$*" >>"$STUB_LOG"
[ "${CURL_ANSWERS:-0}" = 1 ] && exit 0
exit 7
`
	for name, body := range map[string]string{"docker": dockerStub, "curl": curlStub} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	script := filepath.Join(t.TempDir(), "t.sh")
	body := "hostpath() { printf '%s' \"$1\"; }\n" +
		"_tunnel_resolve_conflict() { return 1; }\n" +
		"COMPOSE_DIR=/kit/deploy/compose\nKUBECONFIG_ARG=\"\"\nHOME=/home/t\n" +
		strings.Join(fns, "\n") + "\n" +
		"TUNNEL_ERR=\"\"\n" +
		"if tunnel_up \"$INST\" \"$PORT\"; then echo rc=0; else echo rc=1; fi\n" +
		"printf 'TUNNEL_ERR[%s]\\n' \"$TUNNEL_ERR\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	c := exec.CommandContext(ctx, "bash", script)
	answers := "0"
	if curlAnswers {
		answers = "1"
	}
	c.Env = append(os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_LOG="+logPath, "TIER="+tier, "KUBE_CONTEXT="+kubeContext,
		"INST="+inst, "PORT="+port, "CURL_ANSWERS="+answers)
	t0 := time.Now()
	outB, err := c.CombinedOutput()
	el := time.Since(t0)
	out := string(outB)
	if ctx.Err() != nil {
		t.Fatalf("tunnel_up did not return within %s (%v)\n%s", bound, err, out)
	}
	res := tunnelUpResult{out: out, rc: 1, elapsed: el}
	if strings.Contains(out, "rc=0") {
		res.rc = 0
	}
	if i := strings.Index(out, "TUNNEL_ERR["); i >= 0 {
		v := out[i+len("TUNNEL_ERR["):]
		if j := strings.LastIndex(v, "]"); j >= 0 {
			v = v[:j]
		}
		res.tunnErr = v
	}
	if b, err := os.ReadFile(logPath); err == nil {
		res.log = string(b)
	}
	return res
}

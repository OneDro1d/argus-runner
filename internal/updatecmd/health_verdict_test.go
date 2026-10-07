package updatecmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// health_verdict_test.go — AC-D53 (#329): THE UPDATE'S HEALTH VERDICT HAS THREE STATES, AND "COULD NOT
// REACH" IS NEVER READ AS "UNHEALTHY".
//
// Every test here RUNS apply.sh through the matrix harness (apply_exec_test.go): real bash, the kit's
// real libraries, and docker/kubectl/curl stubs that answer the way those programs answer.
//
// ⛔ (j) COMES FIRST, ON PURPOSE. This change NARROWS when the sticky `rollback-failed` refusal fires. A
// gate only ever seen letting things through has not been shown to be capable of saying no — so the first
// thing proved is that a GENUINE undo failure still arms it, before anything proves that a runtime outage
// no longer does.

func touch(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runBlockGuards runs THE PAGE BLOCK'S OWN stage guards — every rendered line from the first that reads the
// stage's outcome file up to the line that clears the stage — against a stage directory, exactly as the pasted
// block runs them before it clears the stage. Taken from RenderBlock, never restated, so a change to the block is
// a change to what this proves. (A region, not the matching lines: AC-D56's refusal is a multi-line if … fi.)
func runBlockGuards(t *testing.T, stage string) (string, int) {
	t.Helper()
	return runBlockGuardsOf(t, stage, Instance{Tier: "compose", InstanceID: "i1", Image: someImg, KitDir: "/home/op/kits/i1"})
}

// runBlockGuardsOf runs the guards of the block rendered for inst — a forward update or a rollback — with the
// block's own IMG literal, which the NOTE names.
func runBlockGuardsOf(t *testing.T, stage string, inst Instance) (string, int) {
	t.Helper()
	block, _ := RenderBlock(inst)
	var guards []string
	started, ended := false, false
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(l, "IMG=") {
			guards = append(guards, l)
			continue
		}
		if ended {
			continue
		}
		if !started && strings.Contains(l, `"$STAGE_U/outcome"`) {
			started = true
		}
		if started && l == `rm -rf "$STAGE_U"` {
			ended = true
			continue
		}
		if started {
			guards = append(guards, l)
		}
	}
	if !started {
		t.Fatalf("the rendered block reads no outcome file at all — the refusal is gone:\n%s", block)
	}
	if !ended {
		t.Fatalf("the rendered block's guards are not followed by the line that clears the stage:\n%s", block)
	}
	script := "set -euo pipefail\nSTAGE_U=" + shq(stage) + "\n" + strings.Join(guards, "\n") + "\necho GUARDS-PASSED\n"
	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("could not run the block's guards: %v\n%s", err, out)
	}
	return string(out), code
}

// progressOf reads $STAGE/progress.json the way `update rehash` reads it (cmd/argus readProgress): JSON lines,
// the LAST writing of a step wins.
func progressOf(t *testing.T, stage string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(readFile(t, filepath.Join(stage, "progress.json")), "\n") {
		var e struct{ ID, State string }
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &e) == nil && e.ID != "" {
			out[e.ID] = e.State
		}
	}
	return out
}

// The runtime's OWN words for "I could not be reached", as measured 2026-09-30 (builds/AC-D53/02-measurements):
// docker client 29.5.3 on the three transports, and the Hyper-V timeout this estate measured before.
var runtimeDownWords = map[string]string{
	"unix socket":   "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
	"windows pipe":  "failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine; check if the path is correct and if the daemon is running: open //./pipe/dockerDesktopLinuxEngine: The system cannot find the file specified.",
	"tcp":           `error during connect: Get "http://127.0.0.1:2375/v1.54/containers/json": dial tcp 127.0.0.1:2375: connectex: No connection could be made because the target machine actively refused it.`,
	"hyper-v proxy": "docker: Error response from daemon: failed to connect to the backend: timed out dialing Hyper-V socket",
	// Docker Desktop with its ENGINE down behind its still-running API proxy: the proxy answers an empty-body 5xx, and
	// the moby client (docker and compose alike) falls back to this sentence. The client's words were measured
	// 2026-09-30 on this PC's docker 29.5.3 against a stand-in proxy answering 500 with no body; that Docker
	// Desktop's proxy answers so when its engine is down is from docker/for-win#15049 and docker/for-mac#7240 —
	// NOT measured on a live Docker Desktop.
	"docker desktop engine down": "request returned 500 Internal Server Error for API route and version http://%2F%2F.%2Fpipe%2FdockerDesktopLinuxEngine/v1.54/containers/json?all=1, check if the server supports the requested API version",
	// the SAME fallback from a client built on moby v27 or older, which spells the status with http.StatusText alone
	// (no number): moby client/request.go at v27.5.1, and docker/for-mac#7240's own report (client 25.0.3). Docker
	// v28 added the number. Read from the source, not measured against a live proxy.
	"docker desktop engine down (moby v27)": "request returned Internal Server Error for API route and version http://%2FUsers%2Fop%2F.docker%2Frun%2Fdocker.sock/v1.24/containers/json?all=1, check if the server supports the requested API version",
}

// renderedVerdictHelpers returns the verdict helpers exactly as apply.sh renders them, between their two marker
// lines, so the classifier is tested as shipped rather than restated.
func renderedVerdictHelpers(t *testing.T) string {
	t.Helper()
	in := fixtureInput("compose")
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	apply := RenderApply(p, in)
	const begin, end = "# ── AC-D53 verdict helpers ──", "# ── end of the verdict helpers ──"
	i, j := strings.Index(apply, begin), strings.Index(apply, end)
	if i < 0 || j < i {
		t.Fatalf("apply.sh carries no verdict helpers between %q and %q", begin, end)
	}
	return apply[i:j]
}

// ⛔ THE CLASSIFIER, BOTH DIRECTIONS. "Could not reach" ONLY for the runtime's own words for it — the forms
// measured 2026-09-30 — and EVERYTHING else, including anything it does not recognise, is a genuine failure
// (the brief's rule 2): the other direction would clear a refusal that should hold.
func TestAC_D53_TheRuntimesOwnWords(t *testing.T) {
	requireBash(t)
	helpers := renderedVerdictHelpers(t)
	classify := func(fn, text string) string {
		// STAGE is a literal apply.sh always sets before these helpers (renderLiterals); set -u holds the rest
		script := "set -u\nSTAGE=" + shq(t.TempDir()) + "\n" + helpers + "\nif " + fn + " \"$1\"; then echo unreachable; else echo failed; fi\n"
		out, err := exec.Command("bash", "-c", script, "classify", text).CombinedOutput()
		if err != nil {
			t.Fatalf("the classifier did not run: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for name, text := range runtimeDownWords {
		if got := classify("runtime_unreachable", text); got != "unreachable" {
			t.Errorf("docker, %s: %q classified %s, want unreachable", name, text, got)
		}
	}
	// the same proxy fallback, in compose's own sentence and for the other gateway codes a proxy answers with
	for _, text := range []string{
		"unable to get image 'ghcr.io/x/exec@sha256:old': request returned 500 Internal Server Error for API route and version http://%2F%2F.%2Fpipe%2FdockerDesktopLinuxEngine/v1.54/images/ghcr.io/x/exec@sha256:old/json, check if the server supports the requested API version",
		"request returned 502 Bad Gateway for API route and version http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/json, check if the server supports the requested API version",
		"request returned 503 Service Unavailable for API route and version http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/json, check if the server supports the requested API version",
		// moby v27 and older: the same three, with no number
		"request returned Bad Gateway for API route and version http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json, check if the server supports the requested API version",
		"request returned Service Unavailable for API route and version http://%2Fvar%2Frun%2Fdocker.sock/v1.47/containers/json, check if the server supports the requested API version",
	} {
		if got := classify("runtime_unreachable", text); got != "unreachable" {
			t.Errorf("docker, proxy fallback: %q classified %s, want unreachable", text, got)
		}
	}
	for _, text := range []string{
		"Unable to connect to the server: dial tcp 127.0.0.1:1: connectex: No connection could be made because the target machine actively refused it.",
		"Unable to connect to the server: net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)",
		"The connection to the server localhost:8080 was refused - did you specify the right host or port?",
		"error: You must be logged in to the server (Unauthorized)",
	} {
		if got := classify("kube_unreachable", text); got != "unreachable" {
			t.Errorf("kubectl: %q classified %s, want unreachable", text, got)
		}
	}
	// the runtime ANSWERED — measured, or the documented shape — and every one of these is a real failure
	for _, c := range []struct{ fn, text string }{
		{"runtime_unreachable", "error: no such object: cid-executor"},
		{"runtime_unreachable", `docker: Error response from daemon: failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: exec: "docker": executable file not found in $PATH`},
		{"runtime_unreachable", "permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/json\": dial unix /var/run/docker.sock: connect: permission denied"},
		{"runtime_unreachable", "Error response from daemon: Conflict. The container name \"/argus-inst-i1-executor-1\" is already in use"},
		{"runtime_unreachable", "INJECTED FAILURE: compose up -d --no-deps --force-recreate executor"},
		// the ENGINE answered: a 404 in the fallback sentence is an API-version mismatch — a genuine failure. (A real
		// daemon's own 500 carries its message, "Error response from daemon: …", like the Conflict line above.)
		{"runtime_unreachable", "request returned 404 Not Found for API route and version http://%2F%2F.%2Fpipe%2FdockerDesktopLinuxEngine/v1.99/containers/json, check if the server supports the requested API version"},
		{"runtime_unreachable", "request returned Not Found for API route and version http://%2Fvar%2Frun%2Fdocker.sock/v1.99/containers/json, check if the server supports the requested API version"},
		{"runtime_unreachable", ""},
		{"kube_unreachable", "error: timed out waiting for the condition"},
		{"kube_unreachable", "Error from server (InternalError): Internal error occurred: failed calling webhook \"validate.x\": Post \"https://x.svc:443/v\": dial tcp 10.0.0.1:443: connect: connection refused"},
		{"kube_unreachable", "Error from server (NotFound): deployments.apps \"executor\" not found"},
		{"kube_unreachable", ""},
	} {
		if got := classify(c.fn, c.text); got != "failed" {
			t.Errorf("%s(%q) classified %s — an answered error is a GENUINE failure, never could-not-reach", c.fn, c.text, got)
		}
	}
}

// ⛔ NO SECOND PROBE ANYWHERE IN apply.sh, AND NO TOKEN IN THE VERDICT. The verdict comes from what the
// failing command said (rule 1); `/healthz` needs no credential (it is registered on the plain mux).
func TestAC_D53_TheVerdictAsksNoSecondQuestionAndCarriesNoToken(t *testing.T) {
	for _, tier := range []string{"compose", "k3d", "managed"} {
		in := fixtureInput(tier)
		if tier != "compose" {
			in.KubeContext = "k3d-memstore"
		}
		p, err := BuildPlan(in)
		if err != nil {
			t.Fatal(err)
		}
		apply := RenderApply(p, in)
		// AC-D60's helperUserShell asks `docker info` ONCE, before anything moves, to choose the helpers' user — not a
		// question about a failure. It is taken out by its exact text, and must be there exactly once: any other
		// `docker info` is still a second question.
		if n := strings.Count(apply, helperUserShell); n != 1 {
			t.Errorf("%s apply.sh carries helperUserShell %d times, want 1", tier, n)
		}
		apply = strings.Replace(apply, helperUserShell, "", 1)
		for _, bad := range []string{"docker version", "docker info", "docker system", "kubectl version", "cluster-info", "auth_curl", "ARGUS_RUNNER_TOKEN", "localhost:$port", "127.0.0.1:$port", "$ARGUS_MCP_PORT", ":8765"} {
			if strings.Contains(apply, bad) {
				t.Errorf("%s apply.sh contains %q", tier, bad)
			}
		}
	}
}

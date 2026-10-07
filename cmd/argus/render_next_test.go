package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderNextCommandCreatesNeverApplies pins the one command render-k8s tells an operator to run
// next. An agent runs that string verbatim, so it carries the door's two guards itself:
//
//   - `create`, never `apply`: executor.yaml holds the exec-tokens Secret, and `apply` copies it into
//     the last-applied-configuration annotation (docs/DEPLOY-ARGUS.md §4.3). It printed `apply` until
//     2026-09-23, when a fresh-agent run of the door found it.
//   - an explicit `--context`, ALWAYS: the one given with --kube-context, else a placeholder the operator
//     must fill in — a bare `kubectl` runs against whatever context is current (§1.3).
func TestRenderNextCommandCreatesNeverApplies(t *testing.T) {
	got := renderNextCommand("/out/executor.yaml", "/out/obs.yaml", "")

	if strings.Contains(got, " apply ") {
		t.Fatalf("next = %q — `apply` copies the exec-tokens Secret into an annotation; it must be `create`", got)
	}
	if !strings.Contains(got, " create ") {
		t.Fatalf("next = %q, want a `kubectl … create` command", got)
	}
	if !strings.Contains(got, "--context <kube-context>") {
		t.Fatalf("next = %q — without an explicit --context it runs against whatever cluster is current", got)
	}
	for _, f := range []string{"-f /out/executor.yaml", "-f /out/obs.yaml"} {
		if !strings.Contains(got, f) {
			t.Fatalf("next = %q, missing %q", got, f)
		}
	}
}

// TestRenderNextCommandUsesTheGivenContext (T7.2): with a kube context the printed command names it,
// and one holding a shell-significant character is single-quoted, because an agent pastes it verbatim.
func TestRenderNextCommandUsesTheGivenContext(t *testing.T) {
	for ctx, want := range map[string]string{
		"example-cluster":                   "--context example-cluster create ",
		"arn:aws:eks:eu-west-1:1:cluster/x": "--context arn:aws:eks:eu-west-1:1:cluster/x create ",
		"admin@k3d-argus":                   "--context admin@k3d-argus create ",
		"my ctx; rm -rf ~":                  "--context 'my ctx; rm -rf ~' create ",
		"k3d-$(id)":                         "--context 'k3d-$(id)' create ",
	} {
		got := renderNextCommand("/out/executor.yaml", "/out/obs.yaml", ctx)
		if !strings.Contains(got, want) {
			t.Errorf("context %q: next = %q, want it to contain %q", ctx, got, want)
		}
		if strings.Contains(got, "<kube-context>") {
			t.Errorf("context %q: next = %q still carries the placeholder", ctx, got)
		}
	}
}

// renderK8sOutput is what render-k8s emits, read by the dispatch-level tests below.
type renderK8sOutput struct {
	Next              string `json:"next"`
	Executor          string `json:"executor"`
	Obs               string `json:"obs"`
	Tier              string `json:"tier"`
	KubeContext       string `json:"kube_context"`
	Warning           string `json:"warning"`
	Error             string `json:"error"`
	StorageClass      string `json:"storage_class"`
	ResultsAccessMode string `json:"results_access_mode"`
	ObsStorageClass   string `json:"obs_storage_class"`
	SUTAccessRole     string `json:"sut_access_role"`
}

// runRenderK8s drives the REAL render-k8s through dispatch with extra args, returning its exit code, its
// decoded output and the --out directory.
func runRenderK8s(t *testing.T, extra ...string) (int, renderK8sOutput, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte("project:\n  name: render-next-test\n"+
		"targets:\n  http:\n    base_url: https://example.invalid\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ARGUS_RUNNER_TOKEN", "runner-test-token")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "author-test-token")
	t.Setenv("ARGUS_ENROLLMENT_TOKEN", "enrollment-test-token")
	t.Setenv("ARGUS_CP_URL", "https://cp.example.invalid")
	t.Setenv("ARGUS_WORKSPACE_ID", "ws_test")
	out := filepath.Join(dir, "out")

	args := append([]string{"render-k8s", "--config", cfg, "--instance-id", "render-next",
		"--sut-namespace", "sut", "--image", "registry.invalid/argus@sha256:" + strings.Repeat("0", 64),
		"--tier", "aks", "--replicas", "1", "--out", out,
		// --collect-sut-logs default OFF (this file's tests predate the flag and were written
		// against the old always-on behaviour); pass it explicitly so they keep exercising promtail
		// unless a test overrides it (a later --collect-sut-logs=false in extra wins).
		"--collect-sut-logs"}, extra...)
	rc := exitOK
	stdout := captureStdout(t, func() { rc = dispatch(args) })
	var got renderK8sOutput
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("render-k8s output is not the JSON object it should be: %v\n%s", err, stdout)
	}
	return rc, got, out
}

// TestRenderK8sPrintsCreateNotApply reads the `next` field the REAL render-k8s emits. The unit test
// above cannot see a regression at the CALL SITE — a literal `"kubectl apply …"` pasted back into
// cmdRenderK8s would leave renderNextCommand untouched and that test green. This one reads what an
// operator actually gets. With no kube context anywhere it must also SAY so (T7.2).
func TestRenderK8sPrintsCreateNotApply(t *testing.T) {
	t.Setenv("ARGUS_KUBE_CONTEXT_HOST", "")
	rc, got, _ := runRenderK8s(t)
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, got)
	}
	if strings.Contains(got.Next, " apply ") || !strings.Contains(got.Next, " create ") {
		t.Fatalf("render-k8s next = %q — it must say `create`: `apply` copies the exec-tokens Secret into an annotation", got.Next)
	}
	if !strings.Contains(got.Next, "--context <kube-context>") {
		t.Fatalf("render-k8s next = %q — with no context it must carry the placeholder, or it runs against whatever cluster is current", got.Next)
	}
	if got.KubeContext != "" || !strings.Contains(got.Warning, "--kube-context") {
		t.Fatalf("no kube context given: kube_context=%q warning=%q — it must warn that `argus update` will treat the instance as legacy",
			got.KubeContext, got.Warning)
	}
}

// TestRenderK8sKubeContextIsRecordedOnTheInstance (T7.2) is the point of the flag: the context must reach
// the EXECUTOR's env (ARGUS_KUBE_CONTEXT_HOST), because that is what it registers and polls with, and the
// CP's update block reads it from there. Printing it in `next` alone would change nothing for `update`.
func TestRenderK8sKubeContextIsRecordedOnTheInstance(t *testing.T) {
	t.Setenv("ARGUS_KUBE_CONTEXT_HOST", "")
	rc, got, _ := runRenderK8s(t, "--kube-context", "example-cluster")
	if rc != exitOK {
		t.Fatalf("render-k8s exited %d: %+v", rc, got)
	}
	if got.KubeContext != "example-cluster" || got.Warning != "" {
		t.Fatalf("kube_context=%q warning=%q, want example-cluster and no warning", got.KubeContext, got.Warning)
	}
	if !strings.Contains(got.Next, "--context example-cluster create ") {
		t.Fatalf("next = %q, want it to name the given context", got.Next)
	}
	exec, err := os.ReadFile(got.Executor)
	if err != nil {
		t.Fatalf("read executor manifest: %v", err)
	}
	if !strings.Contains(string(exec), "- name: ARGUS_KUBE_CONTEXT_HOST\n              value: \"example-cluster\"") {
		t.Fatalf("executor.yaml does not carry ARGUS_KUBE_CONTEXT_HOST=example-cluster — the instance would register with no context")
	}
}

// TestRenderK8sKubeContextFlagBeatsEnv: onboarding passes the context by env; the flag, when given, wins,
// and the env alone still works exactly as it did before T7.2.
func TestRenderK8sKubeContextFlagBeatsEnv(t *testing.T) {
	t.Setenv("ARGUS_KUBE_CONTEXT_HOST", "from-env")
	if _, got, _ := runRenderK8s(t); got.KubeContext != "from-env" {
		t.Fatalf("env only: kube_context=%q, want from-env", got.KubeContext)
	}
	if _, got, _ := runRenderK8s(t, "--kube-context", "from-flag"); got.KubeContext != "from-flag" {
		t.Fatalf("flag + env: kube_context=%q, want from-flag", got.KubeContext)
	}
}

// TestRenderK8sRefusesAContextTheCPWouldDrop: the CP blanks a kube context that fails SafeHostPath, silently.
// render-k8s must refuse it up front, loudly, and write nothing — otherwise the operator believes the
// instance carries a context it never will.
func TestRenderK8sRefusesAContextTheCPWouldDrop(t *testing.T) {
	t.Setenv("ARGUS_KUBE_CONTEXT_HOST", "")
	rc, got, out := runRenderK8s(t, "--kube-context", "ctx'x")
	if rc != exitUsage {
		t.Fatalf("render-k8s exited %d, want exitUsage (%d): %+v", rc, exitUsage, got)
	}
	if !strings.Contains(got.Error, "kube-context") {
		t.Fatalf("error = %q, want it to name --kube-context", got.Error)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("render-k8s wrote %s before refusing (stat err=%v)", out, err)
	}
}

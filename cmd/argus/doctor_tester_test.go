package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/doctor"
)

// doctor_tester_test.go — `argus doctor --tester` (onboarding review item 18): per-phase evidence lines,
// PASS/FAIL/SKIP, never a token value. The control plane is an httptest server; kubectl is a script on PATH.

// testerCP serves what doctor --tester reads: the /mcp handshake (tools/list) and GET /api/instances.
type testerCP struct {
	srv      *httptest.Server
	mu       sync.Mutex
	bearers  []string
	tools    []string
	lastSeen string // JSON for last_seen: "null" or a quoted RFC3339 time
	inst     string
	// rowExtra is JSON members appended to each /api/instances row (version_state and the floors), e.g. `,"version_state":"current"`.
	rowExtra string
}

func newTesterCP(t *testing.T, token string, tools []string, inst, lastSeen string) *testerCP {
	t.Helper()
	cp := &testerCP{tools: tools, inst: inst, lastSeen: lastSeen}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		cp.bearers = append(cp.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		cp.mu.Unlock()
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != token {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="x"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/instances":
			// inst: "" = no instance registered, "a,b" = several.
			var rows []string
			for _, id := range strings.Split(cp.inst, ",") {
				if id != "" {
					rows = append(rows, fmt.Sprintf(`{"instance_id":%q,"runner_version":"0.3.46","last_seen":%s%s}`, id, cp.lastSeen, cp.rowExtra))
				}
			}
			_, _ = fmt.Fprintf(w, `{"workspace":"msgbus","instances":[%s]}`, strings.Join(rows, ","))
		case r.URL.Path == "/mcp":
			b, _ := io.ReadAll(r.Body)
			var req struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(b, &req)
			switch req.Method {
			case "initialize":
				w.Header().Set("Mcp-Session-Id", "s")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			case "tools/list":
				var ts []map[string]string
				for _, n := range cp.tools {
					ts = append(ts, map[string]string{"name": n})
				}
				out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"tools": ts}})
				_, _ = w.Write(out)
			default:
				w.WriteHeader(http.StatusAccepted)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func authorTools(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("author_tool_%d", i)
	}
	return out
}

// fakeKubectlDoctor puts a kubectl on PATH that logs its argv and answers get --raw and get namespace;
// FAKE_KUBECTL_NS_MISSING=1 makes the namespace lookup fail the way kubectl does.
func fakeKubectlDoctor(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.txt")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"" + calls + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"get --raw\"*) echo '{\"major\":\"1\",\"gitVersion\":\"v1.30.4+k3s1\"}' ;;\n" +
		"  *\"get namespace\"*) if [ -n \"$FAKE_KUBECTL_NS_MISSING\" ]; then echo 'Error from server (NotFound): namespaces \"x\" not found' >&2; exit 1; fi\n" +
		"     echo 'namespace/exists' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func writeTesterEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tester.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runDoctorTester(t *testing.T, args ...string) (rep doctor.Report, rc int, stdout, stderr string) {
	t.Helper()
	for _, v := range []string{"ARGUS_CP_AUTHOR_TOKEN", "ARGUS_CP_TOKEN", "ARGUS_CP_URL", "ARGUS_INSTANCE_ID"} {
		t.Setenv(v, "")
	}
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() { rc = cmdDoctor(append([]string{"--tester"}, args...)) })
	})
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("doctor --tester did not print one JSON report: %v\n%s", err, stdout)
	}
	return
}

func statusOf(t *testing.T, rep doctor.Report, id string) doctor.Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", id, rep.Checks)
	return doctor.Check{}
}

const plantedToken = "odts_PLANTED-tester-token-9f3c1d"

func recentSeen() string {
	return `"` + time.Now().Add(-5*time.Second).UTC().Format(time.RFC3339) + `"`
}

func TestDoctorTester_EveryPhasePassesWithEvidence(t *testing.T) {
	cp := newTesterCP(t, plantedToken, append(authorTools(7), "runner__run"), "msgbus-homelab", recentSeen())
	calls := fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_CP_URL=x\nARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")

	rep, rc, stdout, stderr := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL,
		"--instance", "msgbus-homelab", "--kubeconfig", "/kc/msgbus.kubeconfig")
	if rc != exitOK || rep.Verdict != doctor.VerdictOK {
		t.Fatalf("rc=%d verdict=%s\n%s", rc, rep.Verdict, stdout)
	}
	want := map[string]string{
		"tester-token":     "is set (yes",
		"tester-tools":     "8 tools, 7 author tools",
		"tester-workspace": `workspace "msgbus"`,
		"tester-cluster":   "v1.30.4+k3s1",
		"tester-namespace": "argus-inst-msgbus-homelab",
		"tester-executor":  "polling",
	}
	for id, sub := range want {
		c := statusOf(t, rep, id)
		if c.Status != doctor.StatusOK || !strings.Contains(c.Subject+" "+c.Detail, sub) {
			t.Errorf("%s = %+v; want ok and %q", id, c, sub)
		}
	}
	if len(rep.Lines) != len(rep.Checks) {
		t.Errorf("lines = %d, checks = %d", len(rep.Lines), len(rep.Checks))
	}
	for _, l := range rep.Lines {
		if !strings.HasPrefix(l, "PASS ") {
			t.Errorf("line %q is not PASS", l)
		}
	}
	if !strings.Contains(stderr, "PASS P3 tester-token") {
		t.Errorf("the human lines were not printed to stderr:\n%s", stderr)
	}
	// every kubectl call carried the kubeconfig
	b, _ := os.ReadFile(calls)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(l, "--kubeconfig /kc/msgbus.kubeconfig ") {
			t.Errorf("kubectl call %q lacks --kubeconfig", l)
		}
	}
	// the token reached the control plane as the bearer, and nowhere in the output
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.bearers) == 0 || cp.bearers[0] != plantedToken {
		t.Errorf("the token in the file was not the one presented (%d requests)", len(cp.bearers))
	}
	if strings.Contains(stdout+stderr, plantedToken) || strings.Contains(stdout+stderr, "PLANTED") {
		t.Errorf("LEAK: the token value is in doctor --tester's output:\n%s\n%s", stdout, stderr)
	}
}

func TestDoctorTester_NoTokenFailsTheTokenCheckAndSkipsTheRest(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "msgbus-homelab", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS=\n")
	rep, rc, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "msgbus-homelab")
	if rc != exitFailed || rep.Verdict != doctor.VerdictFail {
		t.Fatalf("rc=%d verdict=%s", rc, rep.Verdict)
	}
	if c := statusOf(t, rep, "tester-token"); c.Status != doctor.StatusFail || !strings.Contains(c.Fix, "after ARGUS_TESTER_TOKEN_MSGBUS=") {
		t.Errorf("tester-token = %+v", c)
	}
	for _, id := range []string{"tester-tools", "tester-workspace", "tester-executor"} {
		if c := statusOf(t, rep, id); c.Status != doctor.StatusSkip {
			t.Errorf("%s = %+v, want skip", id, c)
		}
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.bearers) != 0 {
		t.Errorf("the control plane was contacted %d times with no token", len(cp.bearers))
	}
}

func TestDoctorTester_ATokenThatSeesNoAuthorToolsFails(t *testing.T) {
	cp := newTesterCP(t, plantedToken, []string{"runner__run", "runner__get_report"}, "i", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	rep, rc, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i")
	if rc != exitFailed {
		t.Fatalf("rc=%d", rc)
	}
	if c := statusOf(t, rep, "tester-tools"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "0 author tools") {
		t.Errorf("tester-tools = %+v", c)
	}
}

func TestDoctorTester_RejectedTokenFailsAndSaysSo(t *testing.T) {
	cp := newTesterCP(t, "some-other-token", authorTools(3), "i", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	rep, _, stdout, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i")
	if c := statusOf(t, rep, "tester-tools"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "401") {
		t.Errorf("tester-tools = %+v", c)
	}
	if strings.Contains(stdout, plantedToken) {
		t.Errorf("LEAK in %s", stdout)
	}
}

func TestDoctorTester_ExecutorNeverPolledOrStaleFails(t *testing.T) {
	for name, seen := range map[string]string{
		"never polled": "null",
		"stale":        `"` + time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339) + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			cp := newTesterCP(t, plantedToken, authorTools(3), "i", seen)
			fakeKubectlDoctor(t)
			env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
			rep, rc, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i")
			if c := statusOf(t, rep, "tester-executor"); c.Status != doctor.StatusFail {
				t.Errorf("tester-executor = %+v, want fail", c)
			}
			if rc != exitFailed {
				t.Errorf("rc = %d", rc)
			}
		})
	}
}

func TestDoctorTester_MissingNamespaceFails(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "i", recentSeen())
	fakeKubectlDoctor(t)
	t.Setenv("FAKE_KUBECTL_NS_MISSING", "1")
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	rep, _, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i")
	if c := statusOf(t, rep, "tester-namespace"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "NotFound") {
		t.Errorf("tester-namespace = %+v", c)
	}
}

func TestDoctorTester_SeveralAppsNeedTheAppFlag(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "i", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\nARGUS_TESTER_TOKEN_SHOP=other\n")
	rep, _, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i")
	if c := statusOf(t, rep, "tester-token"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "--app") {
		t.Errorf("tester-token = %+v", c)
	}
	rep, _, _, _ = runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i", "--app", "msgbus")
	if c := statusOf(t, rep, "tester-token"); c.Status != doctor.StatusOK {
		t.Errorf("with --app: tester-token = %+v", c)
	}
}

// Review finding 1: with ZERO executor instances registered the two P5 checks used to SKIP ("pass
// --instance" - when there is nothing to pass) and the verdict was ok, exit 0. Nothing registered means
// the execution plane is not enrolled: that is a FAIL, with a message that does not send the tester to a flag.
func TestDoctorTester_ZeroInstancesIsAFailWithAnAccurateMessage(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	rep, rc, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL)
	c := statusOf(t, rep, "tester-executor")
	if c.Status != doctor.StatusFail {
		t.Errorf("tester-executor = %+v, want fail with no instance registered", c)
	}
	if strings.Contains(c.Detail, "--instance") || !strings.Contains(c.Detail, "no executor instance") {
		t.Errorf("tester-executor detail = %q: want it to say no instance is registered, not to pass --instance", c.Detail)
	}
	if ns := statusOf(t, rep, "tester-namespace"); strings.Contains(ns.Detail, "pass --instance") {
		t.Errorf("tester-namespace detail = %q: sends the tester to a flag when there is nothing to pick", ns.Detail)
	}
	if rc != exitFailed {
		t.Errorf("rc = %d, want %d (exitFailed): zero instances must not be verdict ok", rc, exitFailed)
	}
}

// Several instances without --instance may stay SKIP (ambiguous, not broken) and there --instance IS the fix.
func TestDoctorTester_SeveralInstancesWithoutTheFlagStaySkip(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "a,b", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	rep, _, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL)
	c := statusOf(t, rep, "tester-executor")
	if c.Status != doctor.StatusSkip || !strings.Contains(c.Detail, "--instance") {
		t.Errorf("tester-executor = %+v, want skip naming --instance", c)
	}
}

func TestDoctorTester_WorldReadableEnvFileFails(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "i", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	if err := os.Chmod(env, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i")
	if c := statusOf(t, rep, "tester-token"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "644") {
		t.Errorf("tester-token = %+v", c)
	}
}

func TestDoctorTester_SkipClusterSkipsBothClusterChecks(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "i", recentSeen())
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	t.Setenv("PATH", t.TempDir()) // no kubectl at all
	rep, rc, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i", "--skip-cluster")
	for _, id := range []string{"tester-cluster", "tester-namespace"} {
		if c := statusOf(t, rep, id); c.Status != doctor.StatusSkip {
			t.Errorf("%s = %+v", id, c)
		}
	}
	if rc != exitOK {
		t.Errorf("rc = %d", rc)
	}
}

func TestDoctorUsage_DocumentsTester(t *testing.T) {
	for _, want := range []string{"--tester", "--app", "--kubeconfig", "PASS", "SKIP"} {
		if !strings.Contains(doctorUsage, want) {
			t.Errorf("doctor usage lacks %q", want)
		}
	}
}

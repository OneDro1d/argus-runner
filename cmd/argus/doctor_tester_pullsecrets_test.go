package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/doctor"
)

// doctor_tester_pullsecrets_test.go — tester-pullsecrets: every image pull Secret the executor Deployment
// names must exist in the instance namespace.
//
// The executor's update guard reads only its own Deployment and holds no Secret access, so a Deployment that
// names a Secret nobody created passes the guard and then strands the maxSurge:0 executor. The tester's CLI
// can ask the cluster, so doctor --tester does. Existence only: the check must never read a Secret's data.

// fakeKubectlPullSecrets puts a kubectl on PATH that logs its argv and answers the calls this check makes:
// get namespace, get deployment executor (the names in FAKE_KUBECTL_PULL_SECRETS, space separated), and
// get secret <name> -o name (NotFound for names in FAKE_KUBECTL_SECRETS_MISSING). FAKE_KUBECTL_SECRETS_FORBIDDEN=1
// answers every Secret lookup the way an RBAC refusal does; FAKE_KUBECTL_DEPLOY_FORBIDDEN=1 refuses the
// Deployment read.
func fakeKubectlPullSecrets(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.txt")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"" + calls + "\"\n" +
		"case \"$*\" in\n" +
		"  *\"get --raw\"*) echo '{\"gitVersion\":\"v1.30.4+k3s1\"}' ;;\n" +
		"  *\"get namespace\"*) echo 'namespace/exists' ;;\n" +
		"  *\"get deployment\"*)\n" +
		"     if [ -n \"$FAKE_KUBECTL_DEPLOY_FORBIDDEN\" ]; then echo 'Error from server (Forbidden): deployments.apps \"executor\" is forbidden' >&2; exit 1; fi\n" +
		"     echo -n \"$FAKE_KUBECTL_PULL_SECRETS\" ;;\n" +
		"  *\"get secret\"*)\n" +
		"     name=\"\"; prev=\"\"\n" +
		"     for a in \"$@\"; do if [ \"$prev\" = secret ]; then name=\"$a\"; fi; prev=\"$a\"; done\n" +
		"     if [ -n \"$FAKE_KUBECTL_SECRETS_FORBIDDEN\" ]; then echo \"Error from server (Forbidden): secrets \\\"$name\\\" is forbidden: User cannot get resource secrets\" >&2; exit 1; fi\n" +
		"     for m in $FAKE_KUBECTL_SECRETS_MISSING; do if [ \"$m\" = \"$name\" ]; then echo \"Error from server (NotFound): secrets \\\"$name\\\" not found\" >&2; exit 1; fi; done\n" +
		"     echo \"secret/$name\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func pullSecretRun(t *testing.T, extra ...string) (doctor.Report, int, string) {
	t.Helper()
	cp := newTesterCP(t, plantedToken, authorTools(3), "i", recentSeen())
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	args := append([]string{"--env-file", env, "--control-plane", cp.srv.URL, "--instance", "i", "--kubeconfig", "/kc/x"}, extra...)
	rep, rc, _, stderr := runDoctorTester(t, args...)
	return rep, rc, stderr
}

func TestDoctorTester_PullSecretsAllExistPasses(t *testing.T) {
	calls := fakeKubectlPullSecrets(t)
	t.Setenv("FAKE_KUBECTL_PULL_SECRETS", "ghcr-pull acr-pull")
	rep, rc, _ := pullSecretRun(t)
	c := statusOf(t, rep, "tester-pullsecrets")
	if c.Status != doctor.StatusOK || !strings.Contains(c.Detail, "ghcr-pull") || !strings.Contains(c.Detail, "acr-pull") {
		t.Errorf("tester-pullsecrets = %+v; want ok naming both", c)
	}
	if rc != exitOK {
		t.Errorf("rc = %d", rc)
	}
	// existence only: a Secret is asked for in no form but -o name, and every call carries --kubeconfig
	b, _ := os.ReadFile(calls)
	sawSecret := false
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.HasPrefix(l, "--kubeconfig /kc/x ") {
			t.Errorf("kubectl call %q lacks --kubeconfig", l)
		}
		if strings.Contains(l, "get secret") {
			sawSecret = true
			if !strings.HasSuffix(l, "-o name") || strings.Contains(l, "yaml") || strings.Contains(l, "json") {
				t.Errorf("secret lookup %q may read contents: want -o name only", l)
			}
		}
	}
	if !sawSecret {
		t.Errorf("no secret lookup was made:\n%s", b)
	}
}

func TestDoctorTester_PullSecretsNoneNamedPassesAndSaysSo(t *testing.T) {
	calls := fakeKubectlPullSecrets(t)
	t.Setenv("FAKE_KUBECTL_PULL_SECRETS", "")
	rep, rc, _ := pullSecretRun(t)
	c := statusOf(t, rep, "tester-pullsecrets")
	if c.Status != doctor.StatusOK || !strings.Contains(c.Detail, "no imagePullSecrets") {
		t.Errorf("tester-pullsecrets = %+v; want ok saying none are named", c)
	}
	if rc != exitOK {
		t.Errorf("rc = %d", rc)
	}
	if b, _ := os.ReadFile(calls); strings.Contains(string(b), "get secret") {
		t.Errorf("a secret was looked up although none is named:\n%s", b)
	}
}

func TestDoctorTester_PullSecretsMissingFailsNamingEachWithConsequenceAndFix(t *testing.T) {
	fakeKubectlPullSecrets(t)
	t.Setenv("FAKE_KUBECTL_PULL_SECRETS", "ghcr-pull acr-pull gone-one")
	t.Setenv("FAKE_KUBECTL_SECRETS_MISSING", "acr-pull gone-one")
	rep, rc, stderr := pullSecretRun(t)
	c := statusOf(t, rep, "tester-pullsecrets")
	if c.Status != doctor.StatusFail {
		t.Fatalf("tester-pullsecrets = %+v; want fail", c)
	}
	for _, want := range []string{"acr-pull", "gone-one", "cannot pull", "maxSurge"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q lacks %q", c.Detail, want)
		}
	}
	if strings.Contains(c.Detail, "ghcr-pull") {
		t.Errorf("detail names the Secret that exists: %q", c.Detail)
	}
	if !strings.Contains(c.Fix, "create") || !strings.Contains(c.Fix, "imagePullSecrets") {
		t.Errorf("fix %q must say create the Secret or remove the reference", c.Fix)
	}
	if rc != exitFailed || rep.Verdict != doctor.VerdictFail {
		t.Errorf("rc=%d verdict=%s; want the failing exit", rc, rep.Verdict)
	}
	if !strings.Contains(stderr, "FAIL P5 tester-pullsecrets") {
		t.Errorf("line not printed:\n%s", stderr)
	}
}

// Not knowing is neither PASS nor FAIL: RBAC refusing Secret reads, the Deployment unreadable, --skip-cluster.
func TestDoctorTester_PullSecretsUndeterminableIsSkipWithTheReason(t *testing.T) {
	cases := map[string]struct {
		env    map[string]string
		args   []string
		reason string
	}{
		"rbac forbids secrets":  {env: map[string]string{"FAKE_KUBECTL_PULL_SECRETS": "ghcr-pull", "FAKE_KUBECTL_SECRETS_FORBIDDEN": "1"}, reason: "Forbidden"},
		"deployment unreadable": {env: map[string]string{"FAKE_KUBECTL_DEPLOY_FORBIDDEN": "1"}, reason: "Forbidden"},
		"skip cluster":          {env: map[string]string{"FAKE_KUBECTL_PULL_SECRETS": "ghcr-pull"}, args: []string{"--skip-cluster"}, reason: "--skip-cluster"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fakeKubectlPullSecrets(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			rep, rc, _ := pullSecretRun(t, tc.args...)
			c := statusOf(t, rep, "tester-pullsecrets")
			if c.Status != doctor.StatusSkip || !strings.Contains(c.Detail, tc.reason) {
				t.Errorf("tester-pullsecrets = %+v; want skip saying %q", c, tc.reason)
			}
			if rc != exitOK {
				t.Errorf("rc = %d; an undeterminable check must not fail the run", rc)
			}
		})
	}
}

// A provably missing Secret is a FAIL even when another lookup could not be answered.
func TestDoctorTester_PullSecretsMissingAmongExistingFails(t *testing.T) {
	fakeKubectlPullSecrets(t)
	t.Setenv("FAKE_KUBECTL_PULL_SECRETS", "gone-one other")
	t.Setenv("FAKE_KUBECTL_SECRETS_MISSING", "gone-one")
	rep, _, _ := pullSecretRun(t)
	if c := statusOf(t, rep, "tester-pullsecrets"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "gone-one") {
		t.Errorf("tester-pullsecrets = %+v", c)
	}
}

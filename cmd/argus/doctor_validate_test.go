package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/doctor"
)

// Findings F2-F5 of the 2026-10-03 live run of `argus doctor` on a working cloud tester.

// F2 (end to end): ARGUS_TOKEN holds a control-plane PAT, no role map: local-hats warns, the verdict is not fail.
func TestDoctor_CloudTesterWithOnlyAControlPlanePATWarnsOnLocalHats(t *testing.T) {
	noHats(t)
	pat := "odts_" + "planted-tester-pat"
	t.Setenv("ARGUS_TOKEN", pat)
	t.Setenv(cpTokenEnv, pat)
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, _, out := runDoctor(t, "--control-plane", "https://argus-dev.onedroid.ai", "--scenarios", scenariosDir(t))

	if c := checkByID(t, rep, "local-hats"); c.Status != doctor.StatusWarn {
		t.Errorf("local-hats = %+v, want warn", c)
	}
	for _, id := range rep.Failing {
		if id == "local-hats" {
			t.Errorf("local-hats still fails the run: failing = %v", rep.Failing)
		}
	}
	if strings.Contains(out, pat) {
		t.Error("LEAK: the PAT is in the report")
	}
}

// F3: the control plane answered doctor's own read with this credential, so the credential check says so.
func TestDoctor_CredentialCheckSaysTheControlPlaneAcceptedIt(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "odts_"+"planted-tester-pat")
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, _, _ := runDoctor(t, "--control-plane", cp, "--scenarios", scenariosDir(t))

	c := checkByID(t, rep, "control-plane-credential")
	if strings.Contains(strings.ToLower(c.Detail), "not verified") {
		t.Errorf("the control plane accepted the credential, yet: %s", c.Detail)
	}
	for _, s := range []string{cp, "accepted", "GET /api/instances"} {
		if !strings.Contains(c.Detail, s) {
			t.Errorf("detail lacks %q: %s", s, c.Detail)
		}
	}
}

func TestDoctor_CredentialCheckKeepsNotVerifiedWhenTheReadFailedForAnotherReason(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_"+"planted-tester-pat")
	stubInstances(t, "", nil, errors.New("dial tcp: connection refused"))

	rep, _, _ := runDoctor(t, "--control-plane", "https://argus-dev.onedroid.ai", "--scenarios", scenariosDir(t))

	if c := checkByID(t, rep, "control-plane-credential"); !strings.Contains(c.Detail, "Not verified against the control plane") {
		t.Errorf("a failed read proves nothing about the credential: %s", c.Detail)
	}
}

func TestDoctor_CredentialCheckKeepsNotVerifiedWhenDoctorDidNotAsk(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_"+"planted-tester-pat")
	calls := stubInstances(t, "ws-1", nil, nil)

	rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t)) // no control-plane URL anywhere: nothing to ask

	if calls.calls != 0 {
		t.Fatalf("asked %d times without a URL", calls.calls)
	}
	if c := checkByID(t, rep, "control-plane-credential"); !strings.Contains(c.Detail, "Not verified against the control plane") {
		t.Errorf("doctor did not ask: %s", c.Detail)
	}
}

// F4: first-run mode prints the same lines as tester mode: on stderr, and as `lines` in the JSON.
func TestDoctor_FirstRunPrintsLinesOnStderrAndInTheJSON(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_"+"planted-tester-pat")
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	var rep doctor.Report
	var rc int
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			rc = cmdDoctor([]string{"--control-plane", "https://argus-dev.onedroid.ai", "--scenarios", scenariosDir(t)})
		})
	})
	_ = rc
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not one JSON report: %v\n%s", err, stdout)
	}
	if len(rep.Lines) != len(rep.Checks) || len(rep.Lines) == 0 {
		t.Fatalf("lines = %d, checks = %d", len(rep.Lines), len(rep.Checks))
	}
	for i, c := range rep.Checks {
		if rep.Lines[i] != c.Line() {
			t.Errorf("line %d = %q, want %q", i, rep.Lines[i], c.Line())
		}
		if !strings.Contains(stderr, c.Line()) {
			t.Errorf("stderr lacks the line for %s:\n%s", c.ID, stderr)
		}
	}
	if strings.Contains(stdout+stderr, "planted-tester-pat") {
		t.Error("LEAK: the PAT is in the output")
	}
}

// F5: tester mode judges the executor's version from the rows it already read.
func TestDoctorTester_ExecutorBelowTheRecommendedFloorWarns(t *testing.T) {
	cp := newTesterCP(t, plantedToken, append(authorTools(7), "runner__run"), "msgbus-homelab", recentSeen())
	cp.rowExtra = `,"version_state":"update_recommended","min_supported_version":"0.3.0","min_recommended_version":"0.3.52","recommended_image":"argus-executor@sha256:abc"`
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")

	rep, rc, _, stderr := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "msgbus-homelab", "--kubeconfig", "/kc")

	c := statusOf(t, rep, "tester-executor-version")
	if c.Status != doctor.StatusWarn || c.Phase != "P5" {
		t.Fatalf("tester-executor-version = %+v, want warn in P5", c)
	}
	for _, s := range []string{"0.3.46", "0.3.52"} {
		if !strings.Contains(c.Subject+c.Detail+c.Fix, s) {
			t.Errorf("check does not name %s: %+v", s, c)
		}
	}
	if c.Fix == "" {
		t.Error("a warn without the update fix")
	}
	if rep.Verdict != doctor.VerdictWarn || rc != exitOK {
		t.Errorf("verdict = %s rc = %d, want warn and exit 0", rep.Verdict, rc)
	}
	if !strings.Contains(stderr, "tester-executor-version") {
		t.Errorf("the line was not printed:\n%s", stderr)
	}
}

func TestDoctorTester_ExecutorAtTheFloorPasses(t *testing.T) {
	cp := newTesterCP(t, plantedToken, append(authorTools(7), "runner__run"), "msgbus-homelab", recentSeen())
	cp.rowExtra = `,"version_state":"current","min_supported_version":"0.3.0","min_recommended_version":"0.3.46"`
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")

	rep, _, _, _ := runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "msgbus-homelab", "--kubeconfig", "/kc")

	if c := statusOf(t, rep, "tester-executor-version"); c.Status != doctor.StatusOK {
		t.Errorf("tester-executor-version = %+v, want ok", c)
	}
}

func TestDoctorTester_ExecutorVersionIsSkippedWhenTheTokenFailed(t *testing.T) {
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS=\n")
	rep, _, _, _ := runDoctorTester(t, "--env-file", env, "--skip-cluster")
	if c := statusOf(t, rep, "tester-executor-version"); c.Status != doctor.StatusSkip {
		t.Errorf("tester-executor-version = %+v, want skip", c)
	}
}

// The second network call must not appear: the version check reads the rows tester-workspace already read.
func TestDoctorTester_ExecutorVersionAddsNoSecondRead(t *testing.T) {
	cp := newTesterCP(t, plantedToken, authorTools(3), "msgbus-homelab", recentSeen())
	fakeKubectlDoctor(t)
	env := writeTesterEnv(t, "ARGUS_TESTER_TOKEN_MSGBUS="+plantedToken+"\n")
	runDoctorTester(t, "--env-file", env, "--control-plane", cp.srv.URL, "--instance", "msgbus-homelab", "--kubeconfig", "/kc")
	// one GET /api/instances + the MCP handshake (3 requests): 4 in all, as before the check existed.
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if n := len(cp.bearers); n != 4 {
		t.Errorf("%d control-plane requests, want 4 (initialize, notification, tools/list, /api/instances)", n)
	}
}

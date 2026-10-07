package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// testJWT builds a control-plane-shaped access token (RS256 header, scope + exp claims) with a
// garbage signature and a marker in `sub`, so a test can prove nothing of it reached stdout.
func testJWT(t *testing.T, scope string, exp time.Time, marker string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	body, _ := json.Marshal(map[string]any{"iss": "https://argus-dev.onedroid.ai", "sub": marker, "scope": scope, "exp": exp.Unix()})
	return enc(hdr) + "." + enc(body) + "." + enc([]byte(marker+"-sig"))
}

// noHats clears the three local-auth variables so a developer's shell does not steer the test.
func noHats(t *testing.T) {
	t.Helper()
	for _, v := range []string{"ARGUS_TOKEN", "ARGUS_RUNNER_TOKEN", "ARGUS_AUTHOR_TOKEN", "ARGUS_EXECUTOR_SECRET", "ARGUS_CP_URL"} {
		t.Setenv(v, "")
	}
}

func scenariosDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "one.md"), []byte(validScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runDoctor(t *testing.T, args ...string) (doctor.Report, int, string) {
	t.Helper()
	var rc int
	out := captureStdout(t, func() { rc = cmdDoctor(args) })
	var rep doctor.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor did not print one JSON report: %v\n%s", err, out)
	}
	return rep, rc, out
}

// instancesCall is what a stubbed doctorListInstances was asked.
type instancesCall struct {
	calls    int
	cpURL    string
	gotToken string
}

// stubInstances answers doctor's one network call with the given workspace and rows (or err), and
// records what it was asked. Restored when the test ends.
func stubInstances(t *testing.T, ws string, rows []doctor.ExecutorStatus, err error) *instancesCall {
	t.Helper()
	rec := &instancesCall{}
	prev := doctorListInstances
	doctorListInstances = func(_ context.Context, cpURL, token string) (string, []json.RawMessage, error) {
		rec.calls++
		rec.cpURL, rec.gotToken = cpURL, token
		if err != nil {
			return "", nil, err
		}
		var raws []json.RawMessage
		for _, r := range rows {
			b, _ := json.Marshal(r)
			raws = append(raws, b)
		}
		return ws, raws, nil
	}
	t.Cleanup(func() { doctorListInstances = prev })
	return rec
}

// currentExecutor is an executor with nothing to report: 0.3.44 against a 0.3.44 floor (it carries
// every fix in doctor's knownFixes, the newest being #308 in 0.3.43).
func currentExecutor() doctor.ExecutorStatus {
	return sutReady(doctor.ExecutorStatus{InstanceID: "sut-demo", RunnerVersion: "0.3.44", VersionState: "current",
		MinSupported: "0.3.0", MinRecommended: "0.3.44", RecommendedImage: "argus-executor@sha256:06dc0675"})
}

// sutReady gives a row the fresh "ready" SUT reading a current control plane publishes, for the tests
// whose subject is not the SUT. (A 0.3.36 executor reports no reading at all; what doctor says then is
// internal/doctor TestCheckSUTReachable's subject, not these.)
func sutReady(e doctor.ExecutorStatus) doctor.ExecutorStatus {
	at := time.Now().Add(-30 * time.Second)
	e.SUTState, e.SUTCheckedAt = "ready", &at
	return e
}

// behindWithDockerBlock is sut-demo on 2026-09-29: 0.3.36 against a 0.3.44 floor, with the update
// block the control plane rendered for it — `docker pull`, then `docker run` (DEC-U1).
func behindWithDockerBlock() doctor.ExecutorStatus {
	return sutReady(doctor.ExecutorStatus{InstanceID: "sut-demo", RunnerVersion: "0.3.36", VersionState: "update_recommended",
		MinSupported: "0.3.0", MinRecommended: "0.3.44", RecommendedImage: "argus-executor@sha256:06dc0675",
		UpdateCommand: "(\nset -euo pipefail\ndocker pull \"$IMG\"\ndr() { docker run --rm \"$@\"; }\n)"})
}

// fakeDocker puts a `docker` on PATH that logs its arguments and runs body; PATH holds nothing else,
// so the real one (if any) cannot answer instead. body "" = no docker on PATH at all. Returns the log.
func fakeDocker(t *testing.T, body string) string {
	t.Helper()
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "docker.calls")
	if body != "" {
		stub := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
			t.Fatalf("write docker stub: %v", err)
		}
	}
	t.Setenv("PATH", bin)
	return log
}

func dockerCalls(log string) []string {
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " | "))
}

// writeSession writes a logged-in session store (fresh author token + refresh token) and points the
// CLI at it. Returns the path and the access token.
func writeSession(t *testing.T, cp string) (string, string) {
	t.Helper()
	return writeSessionWith(t, cp, testJWT(t, "author", time.Now().Add(10*time.Minute), "SESSION-MARKER"))
}

// writeSessionWith is writeSession with a given access token (an expired one, say).
func writeSessionWith(t *testing.T, cp, tok string) (string, string) {
	t.Helper()
	sess := filepath.Join(t.TempDir(), "session.json")
	blob, _ := json.Marshal(onboard.SessionData{
		ControlPlane: cp,
		AccessToken:  tok,
		RefreshToken: "REFRESH-MARKER",
		Scope:        "author",
		ObtainedAt:   time.Now().Add(-time.Hour),
	})
	if err := os.WriteFile(sess, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(onboard.SessionEnvOverride, sess)
	return sess, tok
}

func checkByID(t *testing.T, rep doctor.Report, id string) doctor.Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("report has no check %q: %+v", id, rep)
	return doctor.Check{}
}

// THE TRAP, end to end: a bare access token in ARGUS_CP_AUTHOR_TOKEN that expired an hour ago. Before this
// command, that machine learned about it as a 401 from the first cloud call, with nothing naming the
// 15-minute life or the env var. The report must name both — and must not echo the token.
func TestDoctor_ExpiredBareTokenIsNamedAndNeverEchoed(t *testing.T) {
	noHats(t)
	const marker = "SECRET-MARKER-c0ffee"
	t.Setenv(cpTokenEnv, testJWT(t, "author", time.Now().Add(-time.Hour), marker))
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	cpCall := stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, rc, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", "https://argus-dev.onedroid.ai")

	if strings.Contains(out, marker) {
		t.Fatalf("the report echoes the credential:\n%s", out)
	}
	if rc != exitFailed {
		t.Fatalf("exit = %d, want exitFailed (%d)", rc, exitFailed)
	}
	cred := checkByID(t, rep, "control-plane-credential")
	if cred.Status != doctor.StatusFail {
		t.Fatalf("credential status = %q, want fail; detail: %s", cred.Status, cred.Detail)
	}
	if !strings.Contains(cred.Detail, "ARGUS_CP_AUTHOR_TOKEN trap") || !strings.Contains(cred.Detail, "15m") {
		t.Errorf("detail does not name the trap and the 15-minute life: %s", cred.Detail)
	}
	if !strings.Contains(cred.Fix, "cloud-login --control-plane https://argus-dev.onedroid.ai") {
		t.Errorf("fix is not the literal next command: %s", cred.Fix)
	}
	// The executor check did not run, and says so: an expired token is not presented, and not renewed.
	if strings.Join(rep.Failing, ",") != "control-plane-credential,executor-version" {
		t.Errorf("failing = %v, want the credential, then executor-version (not asked)", rep.Failing)
	}
	if ev := checkByID(t, rep, "executor-version"); ev.Status != doctor.StatusUnknown || !strings.Contains(ev.Detail, "expired") {
		t.Errorf("executor-version = %+v; want unknown, naming the expired credential", ev)
	}
	if cpCall.calls != 0 {
		t.Errorf("the control plane was asked %d time(s) with an expired credential", cpCall.calls)
	}
}

// A logged-in machine (session store, fresh author token, refresh token) with its own scenarios and
// no --config: nothing fails, and the one warn is the missing config.
func TestDoctor_LoggedInMachineWarnsOnlyAboutTheMissingConfig(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	sess, tok := writeSession(t, cp)
	cpCall := stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, rc, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

	for _, m := range []string{"SESSION-MARKER", "REFRESH-MARKER"} {
		if strings.Contains(out, m) {
			t.Fatalf("the report echoes %s:\n%s", m, out)
		}
	}
	if rc != exitOK {
		t.Fatalf("exit = %d, want 0; report: %s", rc, out)
	}
	if rep.Verdict != doctor.VerdictWarn || strings.Join(rep.Warning, ",") != "argus-config" || len(rep.Failing) != 0 {
		t.Fatalf("verdict %q warning %v failing %v; want warn on argus-config only", rep.Verdict, rep.Warning, rep.Failing)
	}
	if cred := checkByID(t, rep, "control-plane-credential"); cred.Status != doctor.StatusOK || !strings.Contains(cred.Subject, sess) {
		t.Errorf("credential check = %+v; want ok, subject naming the session file", cred)
	}
	if ev := checkByID(t, rep, "executor-version"); ev.Status != doctor.StatusOK || !strings.Contains(ev.Subject, "sut-demo 0.3.44") {
		t.Errorf("executor-version = %+v; want ok, naming the executor and its version", ev)
	}
	if cpCall.calls != 1 || cpCall.cpURL != cp || cpCall.gotToken != tok {
		t.Errorf("control plane asked %d time(s) at %q, with the session token = %v; want once, at %s, with it",
			cpCall.calls, cpCall.cpURL, cpCall.gotToken == tok, cp)
	}
}

// The mistake `run` cannot refuse at flag parsing: a config for one product, the default scenarios
// of another. Only the scenarios check may warn; the config check must be ok.
// REPLAY B:39 (tester the tester notes "`--scenarios` DEFAULTS to `examples/order-service/demo-packaging/
// scenarios-baked` … so it validated the wrong product's scenarios"; B:54 is its recurrence, not counted).
func TestDoctor_DefaultScenariosAgainstAnotherProductWarns(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	root := t.TempDir()
	t.Chdir(root) // defaultScenariosDir is relative, as it is for `run`
	if err := os.MkdirAll(defaultScenariosDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultScenariosDir, "demo.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "argus-config.yaml")
	if err := os.WriteFile(cfg, []byte("project:\n  name: memstore\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, rc, _ := runDoctor(t, "--config", cfg, "--control-plane", "https://argus-dev.onedroid.ai")

	if rc != exitOK {
		t.Fatalf("exit = %d, want 0 (a warn is not a failure): %+v", rc, rep)
	}
	// Not validated against the bundled demo: its refusals would blame the config for the scenarios mistake.
	if c := checkByID(t, rep, "argus-config"); c.Status != doctor.StatusOK || !strings.Contains(c.Detail, "not checked against scenarios") {
		t.Errorf("config check = %+v, want ok, not checked against the demo scenarios", c)
	}
	sc := checkByID(t, rep, "scenarios-dir")
	if sc.Status != doctor.StatusWarn || !strings.Contains(sc.Detail, `project "memstore"`) {
		t.Errorf("scenarios check = %+v; want warn naming the project", sc)
	}
	if strings.Join(rep.Warning, ",") != "scenarios-dir" {
		t.Errorf("warning = %v, want just scenarios-dir", rep.Warning)
	}
}

// #280 renamed ARGUS_AUTHOR_TOKEN -> ARGUS_EXECUTOR_SECRET for the local author hat, with the old name
// read as a fallback for one release. Doctor must reuse internal/envname's fallback (not a re-
// implementation), so a shell that still exports only the deprecated name is read exactly like one
// exporting the new name — and the report must name the NEW variable either way.
func TestDoctor_LocalHats_DeprecatedEnvNameStillWorks(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	t.Setenv("ARGUS_RUNNER_TOKEN", "RUNNER-HAT")
	t.Setenv("ARGUS_AUTHOR_TOKEN", "AUTHOR-HAT") // deprecated name only; ARGUS_EXECUTOR_SECRET unset
	t.Setenv("ARGUS_TOKEN", "AUTHOR-HAT")        // presents the author hat

	rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t))

	hats := checkByID(t, rep, "local-hats")
	if hats.Status != doctor.StatusOK {
		t.Fatalf("local-hats status = %q via deprecated ARGUS_AUTHOR_TOKEN, want ok; detail: %s", hats.Status, hats.Detail)
	}
	if !strings.Contains(hats.Detail, "author hat") {
		t.Errorf("deprecated ARGUS_AUTHOR_TOKEN was not read as the author hat: %+v", hats)
	}
	if strings.Contains(hats.Subject, "ARGUS_AUTHOR_TOKEN") {
		t.Errorf("subject names the deprecated variable instead of ARGUS_EXECUTOR_SECRET: %s", hats.Subject)
	}
}

// `doctor` owns its argv (ownsArgv), so `--help` reaches its own flagset — and asking is not a mistake.
func TestDoctor_HelpExitsZeroThroughDispatch(t *testing.T) {
	if rc := dispatch([]string{"doctor", "--help"}); rc != exitOK {
		t.Fatalf("doctor --help returned %d, want 0", rc)
	}
	if rc := dispatch([]string{"doctor", "--no-such-flag"}); rc != exitUsage {
		t.Fatalf("doctor --no-such-flag returned %d, want exitUsage (%d)", rc, exitUsage)
	}
}

// REPLAY B:38 end to end (the tester notes, 2026-09-23): a logged-in machine whose executor runs 0.3.36
// while the control plane recommends 0.3.32 and ranks 0.3.36 `current`. Before doctor, the downgrade
// was found by reading the ConfigMap by hand and carried as a warning in four runbooks. The report must
// name it without the operator knowing to look — and must not echo the session's tokens.
func TestDoctor_ReplayB38_DowngradeTheControlPlaneCannotShow(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	writeSession(t, cp)
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{sutReady(doctor.ExecutorStatus{InstanceID: "sut-demo", RunnerVersion: "0.3.36", VersionState: "current",
		MinSupported: "0.3.0", MinRecommended: "0.3.32", RecommendedImage: "argus-executor@sha256:d7aff596"})}, nil)

	rep, rc, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

	for _, m := range []string{"SESSION-MARKER", "REFRESH-MARKER"} {
		if strings.Contains(out, m) {
			t.Fatalf("the report echoes %s:\n%s", m, out)
		}
	}
	if rc != exitOK {
		t.Fatalf("exit = %d, want 0 (a warn is not a failure): %s", rc, out)
	}
	ev := checkByID(t, rep, "executor-version")
	if ev.Status != doctor.StatusWarn || !strings.Contains(ev.Detail, "DOWNGRADE") || !strings.Contains(ev.Detail, "#135") ||
		!strings.Contains(strings.ToLower(ev.Fix), "do not run") {
		t.Fatalf("executor-version = %+v; want warn naming the downgrade, the missing fixes, and not to run the block", ev)
	}
}

// No --control-plane and no ARGUS_CP_URL: GETTING-STARTED never tells a new user to set either, so the
// control plane the session was issued by is the one asked.
func TestDoctor_ExecutorVersionAsksTheSessionsControlPlane(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	writeSession(t, cp)
	cpCall := stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t))

	if cpCall.calls != 1 || cpCall.cpURL != cp {
		t.Fatalf("asked %d time(s) at %q; want once, at the session's %s", cpCall.calls, cpCall.cpURL, cp)
	}
	if ev := checkByID(t, rep, "executor-version"); ev.Status != doctor.StatusOK {
		t.Errorf("executor-version = %+v, want ok", ev)
	}
}

// A control plane that cannot be reached is unknown — "could not find out", never "fine".
func TestDoctor_ExecutorVersionUnreachableIsUnknown(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "", nil, errors.New("dial tcp: connection refused"))

	rep, rc, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", "https://argus-dev.onedroid.ai")

	if rc != exitFailed {
		t.Fatalf("exit = %d, want exitFailed: an unknown check blocks", rc)
	}
	if ev := checkByID(t, rep, "executor-version"); ev.Status != doctor.StatusUnknown || !strings.Contains(ev.Detail, "connection refused") {
		t.Errorf("executor-version = %+v; want unknown, carrying the error", ev)
	}
}

// REPLAY L0929:R4 (tester verify/2026-09-29-doctor-pr326-live/R4-wrong-token.out), live against argus-dev
// with PR #326's binary: a wrong odts_ value in ARGUS_CP_AUTHOR_TOKEN. The control plane answered 401, and doctor
// said the credential was ok and that the fix for the executor list was "re-run argus doctor". Re-running changes
// nothing; the credential is the fault, and the report must say so where the credential is checked.
func TestDoctor_RefusedCredentialFailsWhereTheCredentialIsChecked(t *testing.T) {
	const cp = "https://argus-dev.onedroid.ai"
	for _, tc := range []struct {
		name, answer, detail string
		status               int
	}{
		{"401: not accepted at all", "/api/instances answered HTTP 401: invalid credential", "refused it", http.StatusUnauthorized},
		{"403: a builder token", "/api/instances answered HTTP 403: the web is author-scope only", "author-scope only", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noHats(t)
			t.Setenv(cpTokenEnv, "odts_WRONG-TOKEN-MARKER")
			t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
			stubInstances(t, "", nil, &onboard.HTTPStatusError{Status: tc.status, Msg: tc.answer})

			rep, rc, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

			if rc != exitFailed {
				t.Errorf("exit = %d, want exitFailed", rc)
			}
			if cr := checkByID(t, rep, "control-plane-credential"); cr.Status != doctor.StatusFail || !strings.Contains(cr.Detail, tc.detail) ||
				!strings.Contains(cr.Fix, "cloud-login --control-plane "+cp+" --scope author") {
				t.Errorf("control-plane-credential = %+v; want fail, saying what the control plane answered, with a sign-in fix", cr)
			}
			ev := checkByID(t, rep, "executor-version")
			if ev.Status != doctor.StatusUnknown || !strings.Contains(ev.Detail, tc.answer) {
				t.Errorf("executor-version = %+v; want unknown, carrying the control plane's answer", ev)
			}
			if strings.Contains(ev.Fix, "re-run argus doctor; if it repeats") || !strings.Contains(ev.Fix, "control-plane-credential") {
				t.Errorf("executor-version fix = %q; want it to point at control-plane-credential, not at re-running", ev.Fix)
			}
			if strings.Contains(out, "WRONG-TOKEN-MARKER") {
				t.Error("the token value reached the report")
			}
		})
	}
	// Negative control: a control plane that fails to answer (503) says nothing about the credential.
	t.Run("503 is not a refusal", func(t *testing.T) {
		noHats(t)
		t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
		t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
		stubInstances(t, "", nil, &onboard.HTTPStatusError{Status: http.StatusServiceUnavailable, Msg: "/api/instances answered HTTP 503"})

		rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

		if cr := checkByID(t, rep, "control-plane-credential"); cr.Status != doctor.StatusOK {
			t.Errorf("control-plane-credential on a 503 = %+v; want ok — an outage is not a verdict on the credential", cr)
		}
		if ev := checkByID(t, rep, "executor-version"); !strings.Contains(ev.Fix, "re-run argus doctor") {
			t.Errorf("executor-version fix on a 503 = %q; want re-run", ev.Fix)
		}
	})
}

// THE REAL CALL, over HTTP (no stub): the session's token is presented as the bearer, the rows decode
// from the control plane's own field names, a refusal comes back with the control plane's reason — and
// the session file is byte-for-byte unchanged afterwards, because doctor never renews.
func TestDoctor_ExecutorVersionOverHTTPIsReadOnly(t *testing.T) {
	var gotAuth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/instances" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":"a workspace-bound token is required for the web"}`))
			return
		}
		_, _ = w.Write([]byte(`{"workspace":"ws-1","instances":[{"instance_id":"sut-demo","runner_version":"0.3.44",` +
			`"version_state":"current","min_supported_version":"0.3.0","min_recommended_version":"0.3.44","recommended_image":"x@sha256:d12a"}]}`))
	}))
	defer srv.Close()

	noHats(t)
	t.Setenv(cpTokenEnv, "")
	sess, tok := writeSession(t, srv.URL)
	before, _ := os.ReadFile(sess)

	rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", srv.URL)

	if gotAuth != "Bearer "+tok {
		t.Errorf("the session's access token was not the bearer")
	}
	if ev := checkByID(t, rep, "executor-version"); ev.Status != doctor.StatusOK || !strings.Contains(ev.Subject, "sut-demo 0.3.44") ||
		!strings.Contains(ev.Subject, "workspace ws-1") {
		t.Errorf("executor-version = %+v; want ok, naming workspace, executor and version", ev)
	}
	if after, _ := os.ReadFile(sess); !bytes.Equal(before, after) {
		t.Errorf("doctor changed the session file")
	}

	status = http.StatusForbidden
	rep, _, _ = runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", srv.URL)
	if ev := checkByID(t, rep, "executor-version"); ev.Status != doctor.StatusUnknown || !strings.Contains(ev.Detail, "HTTP 403") ||
		!strings.Contains(ev.Detail, "a workspace-bound token is required for the web") {
		t.Errorf("executor-version on a 403 = %+v; want unknown, carrying the status and the control plane's reason", ev)
	}
}

// THE COMMON CASE, over HTTP (no stub): a session whose 15-minute access token expired hours ago —
// what every signed-in machine holds most of the time (found live 2026-09-28: without renewal, doctor
// failed every such machine with "fix control-plane-credential first" while that check said "nothing to
// do"). Doctor renews it with one refresh_token grant, reads with the NEW token, and writes nothing.
func TestDoctor_ExpiredSessionIsRenewedInMemory(t *testing.T) {
	const refresh = "REFRESH-MARKER"
	fresh := testJWT(t, "author", time.Now().Add(15*time.Minute), "FRESH-MARKER")
	var grants int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			grants++
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != refresh {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			// The control plane keeps the refresh token (no rotation, oauth/endpoints.go grantRefresh).
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fresh, "refresh_token": refresh, "expires_in": 900})
		case r.Method == http.MethodGet && r.URL.Path == "/api/instances":
			if r.Header.Get("Authorization") != "Bearer "+fresh {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid credential"}`))
				return
			}
			_, _ = w.Write([]byte(`{"workspace":"ws-1","instances":[{"instance_id":"sut-demo","runner_version":"0.3.44",` +
				`"version_state":"current","min_supported_version":"0.3.0","min_recommended_version":"0.3.44"}]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	noHats(t)
	t.Setenv(cpTokenEnv, "")
	sess, _ := writeSessionWith(t, srv.URL, testJWT(t, "author", time.Now().Add(-3*time.Hour), "SESSION-MARKER"))
	before, _ := os.ReadFile(sess)

	rep, rc, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", srv.URL)

	for _, m := range []string{"SESSION-MARKER", "FRESH-MARKER", refresh} {
		if strings.Contains(out, m) {
			t.Fatalf("the report echoes %s:\n%s", m, out)
		}
	}
	ev := checkByID(t, rep, "executor-version")
	if ev.Status != doctor.StatusOK || !strings.Contains(ev.Subject, "renewed in memory") {
		t.Fatalf("executor-version = %+v (exit %d); want ok, saying the token was renewed in memory", ev, rc)
	}
	if grants != 1 {
		t.Errorf("refresh grants = %d, want exactly 1", grants)
	}
	if after, _ := os.ReadFile(sess); !bytes.Equal(before, after) {
		t.Errorf("doctor wrote the session file on a renewal the control plane did not rotate")
	}
}

// If the control plane ever ROTATES the refresh token, the old one stops working. Dropping the new one
// would sign the machine out, so it is saved — the one write doctor makes, and the report says so.
func TestDoctor_RotatedRefreshTokenIsSavedNotDropped(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	sess, _ := writeSessionWith(t, cp, testJWT(t, "author", time.Now().Add(-time.Hour), "SESSION-MARKER"))
	fresh := testJWT(t, "author", time.Now().Add(15*time.Minute), "FRESH-MARKER")
	prev := doctorRefresh
	doctorRefresh = func(context.Context, string, string) (*onboard.RefreshedTokens, error) {
		return &onboard.RefreshedTokens{AccessToken: fresh, RefreshToken: "ROTATED-MARKER"}, nil
	}
	t.Cleanup(func() { doctorRefresh = prev })
	cpCall := stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, _, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

	if strings.Contains(out, "ROTATED-MARKER") {
		t.Fatalf("the report echoes the rotated refresh token:\n%s", out)
	}
	d, err := onboard.LoadSessionFile(sess)
	if err != nil || d.RefreshToken != "ROTATED-MARKER" || d.AccessToken != fresh {
		t.Fatalf("the rotated pair was not saved (err %v)", err)
	}
	if cpCall.gotToken != fresh {
		t.Errorf("the read did not use the renewed access token")
	}
	if ev := checkByID(t, rep, "executor-version"); !strings.Contains(ev.Subject, "ROTATED") {
		t.Errorf("the report does not say the session file was written: %+v", ev)
	}
}

// After an in-memory renewal the read carried the RENEWED access token, not the one on disk: the credential
// check says the control plane accepted the session after a renewal, never "this credential" (#466 review nit).
func TestDoctor_AcceptedAfterARenewalSaysTheRenewedTokenWasAccepted(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	writeSessionWith(t, cp, testJWT(t, "author", time.Now().Add(-time.Hour), "SESSION-MARKER"))
	fresh := testJWT(t, "author", time.Now().Add(15*time.Minute), "FRESH-MARKER")
	prev := doctorRefresh
	doctorRefresh = func(context.Context, string, string) (*onboard.RefreshedTokens, error) {
		return &onboard.RefreshedTokens{AccessToken: fresh}, nil
	}
	t.Cleanup(func() { doctorRefresh = prev })
	cpCall := stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

	if cpCall.gotToken != fresh {
		t.Fatalf("fixture: the read did not carry the renewed token")
	}
	c := checkByID(t, rep, "control-plane-credential")
	if strings.Contains(c.Detail, "with this credential answered") {
		t.Errorf("says the on-disk credential was accepted, but the read carried the renewed token: %s", c.Detail)
	}
	for _, s := range []string{cp, "accepted", "renewed"} {
		if !strings.Contains(c.Detail, s) {
			t.Errorf("detail lacks %q: %s", s, c.Detail)
		}
	}
}

// A renewal the control plane refuses is unknown, and its fix is a sign-in, not "re-run".
func TestDoctor_FailedRenewalIsUnknownWithASignInFix(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	writeSessionWith(t, cp, testJWT(t, "author", time.Now().Add(-time.Hour), "SESSION-MARKER"))
	prev := doctorRefresh
	doctorRefresh = func(context.Context, string, string) (*onboard.RefreshedTokens, error) {
		return nil, &onboard.HTTPStatusError{Status: http.StatusBadRequest, Msg: "refresh: invalid_grant"}
	}
	t.Cleanup(func() { doctorRefresh = prev })
	cpCall := stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

	ev := checkByID(t, rep, "executor-version")
	if ev.Status != doctor.StatusUnknown || !strings.Contains(ev.Detail, "invalid_grant") || !strings.Contains(ev.Fix, "cloud-login --control-plane "+cp) {
		t.Fatalf("executor-version = %+v; want unknown, carrying the refusal, with a sign-in fix", ev)
	}
	// The credential check said "routine … Nothing to do" here until 2026-09-29: the refusal never reached it.
	if cr := checkByID(t, rep, "control-plane-credential"); cr.Status != doctor.StatusFail || !strings.Contains(cr.Detail, "refresh token") {
		t.Errorf("control-plane-credential = %+v; want fail: the control plane refused the refresh token", cr)
	}
	if cpCall.calls != 0 {
		t.Errorf("the executor list was read %d time(s) with a credential that could not be renewed", cpCall.calls)
	}
}

// REPLAY L0929:docker (tester verify/2026-09-29-312-fix-check/NOTE.md § Addendum 2). On a Coder
// workspace with the docker CLI and no daemon, the fix said "with the update block the control plane
// publishes" — a block that stops at its first `docker pull`. Doctor now asks the daemon, read-only.
func TestDoctor_NoDockerDaemonSaysTheUpdateBlockCannotRunHere(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	writeSession(t, cp)
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{behindWithDockerBlock()}, nil)
	log := fakeDocker(t, `echo "failed to connect to the docker API at unix:///var/run/docker.sock; dial unix /var/run/docker.sock: connect: no such file or directory" >&2; exit 1`)

	rep, rc, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

	if rc != exitOK {
		t.Fatalf("exit = %d, want 0 (a warn): %s", rc, out)
	}
	ev := checkByID(t, rep, "executor-version")
	for _, s := range []string{"cannot run on this host", "/var/run/docker.sock", "ask your control-plane operator"} {
		if !strings.Contains(ev.Fix, s) {
			t.Errorf("fix does not say %q:\n%s", s, ev.Fix)
		}
	}
	calls := dockerCalls(log)
	if len(calls) == 0 || calls[0] != "version" {
		t.Errorf("docker was called as %q; want one read-only `docker version`", calls)
	}
}

func TestDoctor_NoDockerOnPathSaysSo(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	writeSession(t, cp)
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{behindWithDockerBlock()}, nil)
	fakeDocker(t, "")

	rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)

	if ev := checkByID(t, rep, "executor-version"); !strings.Contains(ev.Fix, "cannot run on this host") || !strings.Contains(ev.Fix, "no docker command on PATH") {
		t.Errorf("fix = %s", ev.Fix)
	}
}

// Negative controls: a daemon that answers leaves the fix as it was, and doctor does not touch Docker
// at all when no executor's block needs it.
func TestDoctor_DockerIsAskedOnlyWhenABlockNeedsIt(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	writeSession(t, cp)

	t.Run("a daemon answers: no Docker sentence", func(t *testing.T) {
		stubInstances(t, "ws-1", []doctor.ExecutorStatus{behindWithDockerBlock()}, nil)
		log := fakeDocker(t, `echo 27.3.1; exit 0`)
		rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)
		ev := checkByID(t, rep, "executor-version")
		if strings.Contains(ev.Fix, "cannot run on this host") || !strings.Contains(ev.Fix, "update block") {
			t.Errorf("fix = %s", ev.Fix)
		}
		if len(dockerCalls(log)) == 0 {
			t.Errorf("docker was not asked, so this control proves nothing")
		}
	})
	t.Run("no block needs Docker: docker is never run", func(t *testing.T) {
		stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)
		log := fakeDocker(t, `exit 1`)
		runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)
		if calls := dockerCalls(log); len(calls) != 0 {
			t.Errorf("docker was run as %q with no block that needs it", calls)
		}
	})
}

// writeTokenFile writes a token file the way `cloud-login --token-out` and a Tokens-page copy leave one:
// the value and a trailing newline.
func writeTokenFile(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(p, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// REPLAY B:66 end to end (handoffs/2026-09-25-hub-runner-window-mixup.md:19-20): a Tokens-page author
// PAT went out in the wrap meant for the builder. Checked before the hand-off, the file says what it is —
// and neither prints the value nor presents it to anyone.
func TestDoctor_TokenFileNamesWhatItHoldsBeforeTheHandOff(t *testing.T) {
	noHats(t)
	const cp = "https://argus-dev.onedroid.ai"
	t.Setenv(cpTokenEnv, "")
	_, sessTok := writeSession(t, cp)
	cpCall := stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	t.Run("B:66 an odts_ PAT for the runner side warns", func(t *testing.T) {
		const marker = "odts_SECRET-MARKER-b66"
		path := writeTokenFile(t, marker)
		rep, rc, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp, "--token-file", path, "--token-for", "runner")
		if strings.Contains(out, "SECRET-MARKER-b66") {
			t.Fatalf("the report echoes the token file's value:\n%s", out)
		}
		if rc != exitOK {
			t.Errorf("exit = %d, want 0 (a warn)", rc)
		}
		tf := checkByID(t, rep, "token-file")
		if tf.Status != doctor.StatusWarn || !strings.Contains(tf.Subject, path) || !strings.Contains(tf.Detail, "not the runner access token") {
			t.Errorf("token-file = %+v; want warn, naming the file and that it is not the runner token", tf)
		}
		if !strings.Contains(strings.Join(rep.Warning, ","), "token-file") {
			t.Errorf("warning = %v, want token-file among them", rep.Warning)
		}
	})
	t.Run("control: runner-token.txt for the runner side is ok", func(t *testing.T) {
		const marker = "SECRET-MARKER-runner"
		tok := testJWT(t, "runner", time.Now().Add(12*time.Minute), marker)
		path := writeTokenFile(t, tok)
		rep, _, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp, "--token-file", path, "--token-for", "runner")
		for _, s := range append(strings.Split(tok, "."), marker) {
			if strings.Contains(out, s) {
				t.Fatalf("the report echoes part of the token file's value (%q):\n%s", s, out)
			}
		}
		if tf := checkByID(t, rep, "token-file"); tf.Status != doctor.StatusOK || !strings.Contains(tf.Detail, "runner__run accepts it") {
			t.Errorf("token-file = %+v; want ok, runner__run accepts it", tf)
		}
		if cpCall.gotToken != sessTok {
			t.Errorf("the control plane was presented something other than the session's token: the token file must never leave the machine")
		}
	})
	t.Run("two values in the file fail", func(t *testing.T) {
		rep, rc, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp, "--token-file", writeTokenFile(t, "odts_a\nodts_b"))
		if tf := checkByID(t, rep, "token-file"); tf.Status != doctor.StatusFail || !strings.Contains(tf.Detail, "2 values") {
			t.Errorf("token-file = %+v; want fail, 2 values", tf)
		}
		if rc != exitFailed {
			t.Errorf("exit = %d, want exitFailed (%d)", rc, exitFailed)
		}
	})
	t.Run("an unreadable file is unknown, never ok", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent.txt")
		rep, rc, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp, "--token-file", missing)
		if tf := checkByID(t, rep, "token-file"); tf.Status != doctor.StatusUnknown || !strings.Contains(tf.Detail, "no such file") {
			t.Errorf("token-file = %+v; want unknown, saying why", tf)
		}
		if rc != exitFailed {
			t.Errorf("exit = %d, want exitFailed (%d)", rc, exitFailed)
		}
	})
	t.Run("no --token-file: no token-file check", func(t *testing.T) {
		rep, _, _ := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", cp)
		for _, c := range rep.Checks {
			if c.ID == "token-file" {
				t.Errorf("a token-file check ran with no file named: %+v", c)
			}
		}
	})
}

// --token-for is a claim about a file; without one, or with a side that does not exist, it is a usage error.
func TestDoctor_TokenForIsRefusedWithoutAFileOrWithAnUnknownSide(t *testing.T) {
	noHats(t)
	for name, args := range map[string][]string{
		"no --token-file": {"--token-for", "runner"},
		"unknown side":    {"--token-file", writeTokenFile(t, "odts_x"), "--token-for", "builder"},
	} {
		t.Run(name, func(t *testing.T) {
			var rc int
			captureStdout(t, func() { rc = cmdDoctor(args) })
			if rc != exitUsage {
				t.Errorf("exit = %d, want exitUsage (%d)", rc, exitUsage)
			}
		})
	}
}

// writeConfig writes an argus-config.yaml into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// B:35 (a port-less http base_url is reached on :8080) and ${VAR}s unset here, end to end. A ${VAR}'s
// value is never printed, and a base_url that is still a ${VAR} is not judged.
func TestDoctor_ConfigNamesUnsetVarsAndPortlessURLs(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)
	t.Setenv("SYN_TOKEN", "")
	t.Setenv("SYN_URL", "http://SECRET-MARKER-url")
	t.Setenv("SYN_HOST", "SECRET-MARKER-host")
	cfg := writeConfig(t, `project:
  name: hub
targets:
  http:
    base_url: http://hub-api
  http_targets:
    known:
      base_url: ${SYN_URL}
    graph:
      base_url: http://hub-graph
    templated:
      base_url: http://${SYN_HOST}
    pathvar:
      base_url: http://hub-files/${SYN_HOST}
  mcp:
    base_url: http://hub-api:8080/mcp
    auth:
      type: bearer
      bearer_token: ${SYN_TOKEN}
`)
	rep, rc, out := runDoctor(t, "--config", cfg, "--scenarios", scenariosDir(t), "--control-plane", "https://argus-dev.onedroid.ai")

	if strings.Contains(out, "SECRET-MARKER") {
		t.Fatalf("the report echoes a ${VAR}'s value:\n%s", out)
	}
	if rc != exitOK {
		t.Errorf("exit = %d, want 0 (warn)", rc)
	}
	c := checkByID(t, rep, "argus-config")
	if c.Status != doctor.StatusWarn {
		t.Fatalf("config check = %+v; want warn", c)
	}
	for _, s := range []string{"SYN_TOKEN (targets.mcp.auth.bearer_token)", "targets.http.base_url, targets.http_targets.graph.base_url, targets.http_targets.pathvar.base_url names no port"} {
		if !strings.Contains(c.Detail, s) {
			t.Errorf("detail lacks %q: %s", s, c.Detail)
		}
	}
	for _, s := range []string{"SYN_URL (", "SYN_HOST (", "http_targets.known", "http_targets.templated"} {
		if strings.Contains(c.Detail, s) {
			t.Errorf("detail says %q, which is set or not literal: %s", s, c.Detail)
		}
	}
}

// validScenario is one the catalog import accepts: examples/argus-selftest/scenarios/DOGFOOD-001-… without its
// References section.
const validScenario = "# Scenario: The cloud MCP returns a clean TOOL error over Streamable HTTP (isError plane)\n\n" +
	"## Metadata\n- **ID**: DOGFOOD-001\n- **Layer**: HTTP Ingestion\n- **Tags**: mcp, functional-area:recursive-dogfood\n\n" +
	"## TRIGGER\nPOST `${MCP_URL}`\n\n```json\n" +
	`{"transport":"streamable-http","tool":"author__get_executor_status","args":{"instance_id":"onedroid-selftest-unknown"},"request_id":"${cid}"}` +
	"\n```\n\n## EXPECT\n\n### Runnable\n- result.isError == true\n\n## TIMEOUT\n30s\n\n## CLEANUP\nN/A — read-only.\n"

// oldFormatScenario is A:56's pack as it was: tester-upstream-notepad scenarios-v2/SYN-S0-002-liveness.md, lines
// 1-23 verbatim (its References section left out), written for executor 0.3.30. 0.3.31 made the format a strict
// contract and refuses it.
const oldFormatScenario = "# Scenario: Hub dev answers its liveness probe\n\n" +
	"## Metadata\n- **ID**: SYN-S0-002\n- **Layer**: HTTP Ingestion\n- **Priority**: High\n- **Tags**: functional-area:availability, anonymous\n\n" +
	"## TRIGGER\nGET `${INGESTION_URL}/api/health/live`\n\n" +
	"## VERIFY\nThe liveness route does no I/O and is the same endpoint the kubelet probes\n" +
	"(`k8s/base/services/mcp-server.yaml:111`), so a non-200 here means the process itself is not\n" +
	"serving. The body assertion pins that it answers with its liveness verdict rather than with any\n" +
	"200-shaped page an ingress might substitute.\n\n" +
	"## EXPECT\n- status=200\n- body has status containing \"status\":\"UP\"\n\n" +
	"## CLEANUP\nN/A — read-only.\n"

func writeScenarios(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// REPLAY A:9 and A:56 end to end. doctor puts every file the catalog import (cloud-seed-scenarios) would upload
// through the rules the control plane's author_write_scenario applies (toolcore.ValidateAll), and names each
// one it would refuse — before onboarding, not as a count after it.
func TestDoctor_ScenariosTheImportWouldRefuseAreNamed(t *testing.T) {
	noHats(t)
	t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
	t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
	stubInstances(t, "ws-1", []doctor.ExecutorStatus{currentExecutor()}, nil)

	t.Run("A:9 + A:56: an ID the rule refuses and an old-format file are named; the valid file is not", func(t *testing.T) {
		dir := writeScenarios(t, map[string]string{
			"DOGFOOD-001.md":             validScenario,
			"sub/SYN-S0-002-liveness.md": oldFormatScenario,
			"dotted.md":                  strings.Replace(validScenario, "DOGFOOD-001", "syn.mcp.001", 1),
		})
		rep, rc, _ := runDoctor(t, "--scenarios", dir, "--control-plane", "https://argus-dev.onedroid.ai")
		c := checkByID(t, rep, "scenarios-dir")
		if c.Status != doctor.StatusFail {
			t.Fatalf("scenarios check = %+v; want fail", c)
		}
		for _, s := range []string{"would refuse 2 of 3", "sub/SYN-S0-002-liveness.md", `(ID "SYN-S0-002")`, "7 errors", "### Runnable",
			"dotted.md", `"syn.mcp.001"`, "is not allowed"} {
			if !strings.Contains(c.Detail, s) {
				t.Errorf("detail does not say %q: %s", s, c.Detail)
			}
		}
		if strings.Contains(c.Detail, "DOGFOOD-001.md") {
			t.Errorf("detail names the file the import accepts: %s", c.Detail)
		}
		if rc != exitFailed {
			t.Errorf("exit = %d, want exitFailed (%d)", rc, exitFailed)
		}
	})
	t.Run("control: a directory the import accepts whole is ok", func(t *testing.T) {
		rep, _, _ := runDoctor(t, "--scenarios", writeScenarios(t, map[string]string{"DOGFOOD-001.md": validScenario}),
			"--control-plane", "https://argus-dev.onedroid.ai")
		if c := checkByID(t, rep, "scenarios-dir"); c.Status != doctor.StatusOK || !strings.Contains(c.Detail, "would accept all 1") {
			t.Errorf("scenarios check = %+v; want ok, all accepted", c)
		}
	})
	t.Run("a file the import cannot read is unknown: the import reads all before writing any", func(t *testing.T) {
		dir := writeScenarios(t, map[string]string{"DOGFOOD-001.md": validScenario})
		if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "gone.md")); err != nil {
			t.Fatal(err)
		}
		rep, _, _ := runDoctor(t, "--scenarios", dir, "--control-plane", "https://argus-dev.onedroid.ai")
		c := checkByID(t, rep, "scenarios-dir")
		if c.Status != doctor.StatusUnknown || !strings.Contains(c.Detail, "gone.md") || !strings.Contains(c.Detail, "imports nothing") {
			t.Errorf("scenarios check = %+v; want unknown, naming the file and that nothing is imported", c)
		}
	})
}

// , doctor's half: the report repeats the control plane in check subjects and in fix commands.
// A --control-plane given with userinfo, a query or a fragment comes back without them in every check, while
// the REQUEST still goes to the URL as given.
func TestDoctor_ReportNeverRepeatsTheControlPlaneURLsCredentials(t *testing.T) {
	const raw = "https://someuser:PWCANARY@argus-dev.onedroid.ai/base?token=QCANARY#FRAGCANARY"
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the control plane refuses the credential", &onboard.HTTPStatusError{Status: http.StatusUnauthorized, Msg: "/api/instances answered HTTP 401: invalid credential"}},
		{"the control plane cannot be reached", errors.New("dial tcp: connection refused")},
		{"no executor is registered", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noHats(t)
			t.Setenv(cpTokenEnv, "odts_a-long-lived-author-pat")
			t.Setenv(onboard.SessionEnvOverride, filepath.Join(t.TempDir(), "absent.json"))
			rec := stubInstances(t, "ws-1", nil, tc.err)
			// A token file with two values is refused with a mint command, which names the control plane too.
			tokenFile := filepath.Join(t.TempDir(), "handoff.token")
			if err := os.WriteFile(tokenFile, []byte("one two\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, _, out := runDoctor(t, "--scenarios", scenariosDir(t), "--control-plane", raw, "--token-file", tokenFile, "--token-for", doctor.SideAuthor)

			for _, canary := range []string{"PWCANARY", "QCANARY", "FRAGCANARY", "someuser"} {
				if strings.Contains(out, canary) {
					t.Errorf("the report repeats %q from the control-plane URL:\n%s", canary, out)
				}
			}
			if !strings.Contains(out, "https://argus-dev.onedroid.ai/base") {
				t.Errorf("the report no longer names the control plane at all:\n%s", out)
			}
			if rec.cpURL != raw {
				t.Errorf("the request went to %q; want the URL as given, only the REPORT is cleaned", rec.cpURL)
			}
		})
	}
}

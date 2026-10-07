package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/doctor"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// The help text must not promise more than the code keeps: doctor's one possible write is the rotated
// session token (renewInMemory), and the usage says so.
func TestDoctor_UsageSaysWhatItWrites(t *testing.T) {
	if !strings.Contains(doctorUsage, "writes nothing except a rotated session token") {
		t.Errorf("doctor usage does not say what doctor writes:\n%s", doctorUsage)
	}
	if strings.Contains(doctorUsage, "changes nothing") {
		t.Errorf("doctor usage still promises it changes nothing:\n%s", doctorUsage)
	}
}

// leakMarker is a fresh high-entropy string per run: a test that greps for a constant could be passed
// by a doctor that happens to print something else, and a constant can be grepped for in the source.
func leakMarker(t *testing.T, label string) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return label + "-" + hex.EncodeToString(b)
}

// leakSecret is one planted credential: the whole value, and the pieces a leak could print on its own
// (a JWT's three segments — its claims carry the marker only base64url-encoded, so the raw marker alone
// would miss a printed token — and the marker itself).
type leakSecret struct {
	label   string
	value   string
	needles []string
}

func newLeakSecret(label, value string, extra ...string) leakSecret {
	n := append([]string{value}, extra...)
	if parts := strings.Split(value, "."); len(parts) == 3 {
		n = append(n, parts...)
	}
	return leakSecret{label: label, value: value, needles: n}
}

// leakRig serves the control plane and records what the BUILT binary presented to it: the proof that a
// credential source was really read, not merely planted.
type leakRig struct {
	srv      *httptest.Server
	mu       sync.Mutex
	bearers  []string
	grants   []string // refresh tokens presented to /oauth/token
	freshJWT string   // what a refresh grant answers with
	refresh  string   // the refresh token a grant is answered with (no rotation)
}

func newLeakRig(t *testing.T, fresh, refresh string) *leakRig {
	t.Helper()
	rig := &leakRig{freshJWT: fresh, refresh: refresh}
	rig.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/token":
			_ = r.ParseForm()
			rig.grants = append(rig.grants, r.Form.Get("refresh_token"))
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": rig.freshJWT, "refresh_token": rig.refresh, "expires_in": 900})
		case r.Method == http.MethodGet && r.URL.Path == "/api/instances":
			rig.bearers = append(rig.bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			_, _ = w.Write([]byte(`{"workspace":"ws-1","instances":[{"instance_id":"sut-demo","runner_version":"0.3.44",` +
				`"version_state":"current","min_supported_version":"0.3.0","min_recommended_version":"0.3.44"}]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(rig.srv.Close)
	return rig
}

// builtArgus builds the real CLI into the calling test's temp dir. The leak test execs a binary, as a
// user does, so stdout, stderr, the process environment and the exit code are the real ones — not a
// function call with a captured os.Stdout.
func builtArgus(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "argus")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("could not build the argus binary: %v\n%s", err, out)
	}
	return bin
}

// leakRun execs `argus doctor` with ONLY the given environment (plus an empty PATH dir, so no docker can
// answer, and a temp HOME / config dir / session file, so nothing on this machine is read or written).
// It returns stdout and stderr separately, and the exit code.
func leakRun(t *testing.T, bin string, env map[string]string, args ...string) (stdout, stderr string, rc int) {
	t.Helper()
	home := t.TempDir()
	e := map[string]string{
		"PATH":               t.TempDir(),
		"HOME":               home,
		"XDG_CONFIG_HOME":    filepath.Join(home, "config"),
		"ARGUS_SESSION_FILE": filepath.Join(home, "no-session.json"),
	}
	for k, v := range env {
		e[k] = v
	}
	cmd := exec.Command(bin, append([]string{"doctor"}, args...)...)
	cmd.Dir = home
	for k, v := range e {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	rc = 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("could not run %s: %v", bin, err)
		}
		rc = ee.ExitCode()
	}
	return so.String(), se.String(), rc
}

// leakSession writes a session file holding the given access and refresh tokens and returns its path.
func leakSession(t *testing.T, cp, access, refresh string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.json")
	blob, _ := json.Marshal(onboard.SessionData{ControlPlane: cp, AccessToken: access, RefreshToken: refresh, Scope: "author", ObtainedAt: time.Now().Add(-time.Hour)})
	if err := os.WriteFile(p, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDoctor_NoCredentialLeak: a marker in every credential source the doctor reads, then a grep of the
// WHOLE stdout+stderr of the built binary. Each source is read only when the ones above it are unset,
// so each sub-test makes one source the one in use — and proves it was in use (the control plane saw it
// as the bearer / the refresh grant, or the report names the source and kind it found) — and the last
// sets everything at once.
func TestDoctor_NoCredentialLeak(t *testing.T) {
	bin := builtArgus(t)
	jwt := func(scope, label string, exp time.Time) leakSecret {
		m := leakMarker(t, label)
		return newLeakSecret(label, testJWT(t, scope, exp, m), m)
	}
	pat := func(label string) leakSecret {
		m := "odts_" + leakMarker(t, label)
		return newLeakSecret(label, m, m)
	}
	plain := func(label string) leakSecret {
		m := leakMarker(t, label)
		return newLeakSecret(label, m, m)
	}
	soon, past := time.Now().Add(10*time.Minute), time.Now().Add(-3*time.Hour)

	// check runs the doctor and asserts: no needle of any planted secret anywhere in stdout+stderr, a
	// JSON report on stdout, and a verdict exit (0 or 4). It returns the report for the source assertions.
	check := func(t *testing.T, planted []leakSecret, env map[string]string, args ...string) doctor.Report {
		t.Helper()
		stdout, stderr, rc := leakRun(t, bin, env, args...)
		all := stdout + "\n" + stderr
		for _, s := range planted {
			for _, n := range s.needles {
				if n != "" && strings.Contains(all, n) {
					t.Errorf("LEAK: %s (%.12s…) appears in doctor's stdout/stderr:\n%s", s.label, n, all)
				}
			}
		}
		if rc != exitOK && rc != exitFailed {
			t.Fatalf("doctor exited %d, want a verdict (0 or %d):\n%s", rc, exitFailed, all)
		}
		var rep doctor.Report
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("doctor did not print one JSON report: %v\n%s", err, all)
		}
		return rep
	}
	scen := scenariosDir(t)
	wantBearer := func(t *testing.T, rig *leakRig, want string) {
		t.Helper()
		rig.mu.Lock()
		defer rig.mu.Unlock()
		if len(rig.bearers) != 1 || rig.bearers[0] != want {
			t.Errorf("the control plane was presented %d bearer(s), want exactly the one under test — the source was not the one read", len(rig.bearers))
		}
	}

	t.Run("ARGUS_CP_AUTHOR_TOKEN (odts_ PAT)", func(t *testing.T) {
		s := pat("author-env")
		rig := newLeakRig(t, "", "")
		rep := check(t, []leakSecret{s}, map[string]string{"ARGUS_CP_AUTHOR_TOKEN": s.value}, "--scenarios", scen, "--control-plane", rig.srv.URL)
		if c := checkByID(t, rep, "control-plane-credential"); !strings.Contains(c.Subject, "ARGUS_CP_AUTHOR_TOKEN: an odts_ PAT") {
			t.Errorf("credential subject = %q; want it to name ARGUS_CP_AUTHOR_TOKEN and an odts_ PAT", c.Subject)
		}
		wantBearer(t, rig, s.value)
	})

	t.Run("ARGUS_CP_TOKEN (deprecated name, JWT)", func(t *testing.T) {
		s := jwt("author", "cp-token-env", soon)
		rig := newLeakRig(t, "", "")
		rep := check(t, []leakSecret{s}, map[string]string{"ARGUS_CP_TOKEN": s.value}, "--scenarios", scen, "--control-plane", rig.srv.URL)
		if c := checkByID(t, rep, "control-plane-credential"); !strings.Contains(c.Subject, "ARGUS_CP_TOKEN: a JWT, scope author") {
			t.Errorf("credential subject = %q; want it to name ARGUS_CP_TOKEN and a scope-author JWT", c.Subject)
		}
		wantBearer(t, rig, s.value)
	})

	t.Run("session file, access token in use", func(t *testing.T) {
		access, refresh := jwt("author", "session-access", soon), plain("session-refresh")
		rig := newLeakRig(t, "", "")
		sess := leakSession(t, rig.srv.URL, access.value, refresh.value)
		rep := check(t, []leakSecret{access, refresh}, map[string]string{"ARGUS_SESSION_FILE": sess}, "--scenarios", scen, "--control-plane", rig.srv.URL)
		if c := checkByID(t, rep, "control-plane-credential"); !strings.Contains(c.Subject, sess+": a JWT, scope author") {
			t.Errorf("credential subject = %q; want it to name the session file and a scope-author JWT", c.Subject)
		}
		wantBearer(t, rig, access.value)
	})

	t.Run("session file, expired access token: the refresh token is used", func(t *testing.T) {
		expired, refresh, fresh := jwt("author", "session-expired-access", past), plain("session-refresh"), jwt("author", "renewed-access", soon)
		rig := newLeakRig(t, fresh.value, refresh.value)
		sess := leakSession(t, rig.srv.URL, expired.value, refresh.value)
		rep := check(t, []leakSecret{expired, refresh, fresh}, map[string]string{"ARGUS_SESSION_FILE": sess}, "--scenarios", scen, "--control-plane", rig.srv.URL)
		if c := checkByID(t, rep, "control-plane-credential"); !strings.Contains(c.Subject, sess+": a JWT, scope author") {
			t.Errorf("credential subject = %q; want it to name the session file", c.Subject)
		}
		rig.mu.Lock()
		grants := append([]string(nil), rig.grants...)
		rig.mu.Unlock()
		if len(grants) != 1 || grants[0] != refresh.value {
			t.Errorf("refresh grants = %d, want exactly one presenting the session's refresh token", len(grants))
		}
		wantBearer(t, rig, fresh.value)
	})

	// The one write the usage owns up to: when the control plane ROTATES the refresh token, doctor saves
	// the new pair to the session file. Neither the old nor the new token may reach the output on the way.
	t.Run("session file, expired access token, refresh token rotated: saved, not printed", func(t *testing.T) {
		expired, refresh := jwt("author", "rot-expired-access", past), plain("rot-refresh")
		fresh, rotated := jwt("author", "rot-renewed-access", soon), plain("rot-rotated-refresh")
		rig := newLeakRig(t, fresh.value, rotated.value)
		sess := leakSession(t, rig.srv.URL, expired.value, refresh.value)
		check(t, []leakSecret{expired, refresh, fresh, rotated}, map[string]string{"ARGUS_SESSION_FILE": sess}, "--scenarios", scen, "--control-plane", rig.srv.URL)
		rig.mu.Lock()
		grants := append([]string(nil), rig.grants...)
		rig.mu.Unlock()
		if len(grants) != 1 || grants[0] != refresh.value {
			t.Errorf("refresh grants = %d, want exactly one presenting the session's refresh token", len(grants))
		}
		wantBearer(t, rig, fresh.value)
		saved, err := onboard.LoadSessionFile(sess)
		if err != nil {
			t.Fatalf("reading the session file back: %v", err)
		}
		if saved.RefreshToken != rotated.value || saved.AccessToken != fresh.value {
			t.Errorf("the session file does not hold the rotated pair — the one write doctor owns up to did not happen")
		}
	})

	for _, tc := range []struct {
		name, side string
		secret     func() leakSecret
		wantIn     string // what the token-file check must say it found
	}{
		{"--token-file runner, runner JWT", "runner", func() leakSecret { return jwt("runner", "tf-runner-jwt", soon) }, "runner__run accepts it"},
		{"--token-file runner, odts_ PAT", "runner", func() leakSecret { return pat("tf-runner-pat") }, "not the runner access token"},
		{"--token-file author, odts_ PAT", "author", func() leakSecret { return pat("tf-author-pat") }, "odts_ PAT"},
		{"--token-file author, author JWT", "author", func() leakSecret { return jwt("author", "tf-author-jwt", soon) }, "JWT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.secret()
			rig := newLeakRig(t, "", "")
			path := writeTokenFile(t, s.value)
			rep := check(t, []leakSecret{s}, nil, "--scenarios", scen, "--control-plane", rig.srv.URL, "--token-file", path, "--token-for", tc.side)
			tf := checkByID(t, rep, "token-file")
			if !strings.Contains(tf.Subject, path) || !strings.Contains(tf.Subject+" "+tf.Detail, tc.wantIn) {
				t.Errorf("token-file = %+v; want it to name %s and say %q — the file was not read", tf, path, tc.wantIn)
			}
			rig.mu.Lock()
			defer rig.mu.Unlock()
			if len(rig.bearers) != 0 {
				t.Errorf("the control plane was presented %d bearer(s); a token file is sent nowhere", len(rig.bearers))
			}
		})
	}

	t.Run("local hats, ARGUS_TOKEN matches the runner hat", func(t *testing.T) {
		runner, author := plain("hat-runner"), plain("hat-author")
		rep := check(t, []leakSecret{runner, author}, map[string]string{"ARGUS_TOKEN": runner.value, "ARGUS_RUNNER_TOKEN": runner.value, "ARGUS_EXECUTOR_SECRET": author.value}, "--scenarios", scen)
		if c := checkByID(t, rep, "local-hats"); c.Status != doctor.StatusOK || !strings.Contains(c.Detail, "runner hat") {
			t.Errorf("local-hats = %+v; want ok, the runner hat — the variables were not read", c)
		}
	})

	t.Run("local hats, ARGUS_TOKEN matches neither hat", func(t *testing.T) {
		presented, runner, author := plain("hat-presented"), plain("hat-runner"), plain("hat-author")
		rep := check(t, []leakSecret{presented, runner, author}, map[string]string{"ARGUS_TOKEN": presented.value, "ARGUS_RUNNER_TOKEN": runner.value, "ARGUS_EXECUTOR_SECRET": author.value}, "--scenarios", scen)
		if c := checkByID(t, rep, "local-hats"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "equals neither hat") {
			t.Errorf("local-hats = %+v; want fail, equals neither hat — the variables were not read", c)
		}
	})

	t.Run("local hats, the two hat variables collide", func(t *testing.T) {
		same := plain("hat-collision")
		rep := check(t, []leakSecret{same}, map[string]string{"ARGUS_TOKEN": same.value, "ARGUS_RUNNER_TOKEN": same.value, "ARGUS_EXECUTOR_SECRET": same.value}, "--scenarios", scen)
		if c := checkByID(t, rep, "local-hats"); c.Status != doctor.StatusFail || !strings.Contains(c.Detail, "distinct") {
			t.Errorf("local-hats = %+v; want fail, the two must be distinct — the variables were not read", c)
		}
	})

	t.Run("every source set at once", func(t *testing.T) {
		author := pat("all-author-env")
		legacy := jwt("author", "all-cp-token-env", soon)
		access, refresh := jwt("author", "all-session-access", soon), plain("all-session-refresh")
		tfRunner, tfAuthor := jwt("runner", "all-tf-runner", soon), pat("all-tf-author")
		tok, runner, exec_ := plain("all-hat-token"), plain("all-hat-runner"), plain("all-hat-author")
		rig := newLeakRig(t, "", "")
		sess := leakSession(t, rig.srv.URL, access.value, refresh.value)
		planted := []leakSecret{author, legacy, access, refresh, tfRunner, tfAuthor, tok, runner, exec_}
		env := map[string]string{
			"ARGUS_CP_AUTHOR_TOKEN": author.value, "ARGUS_CP_TOKEN": legacy.value, "ARGUS_SESSION_FILE": sess,
			"ARGUS_TOKEN": tok.value, "ARGUS_RUNNER_TOKEN": runner.value, "ARGUS_EXECUTOR_SECRET": exec_.value,
		}
		for _, tf := range []struct {
			side string
			s    leakSecret
		}{{"runner", tfRunner}, {"author", tfAuthor}} {
			path := writeTokenFile(t, tf.s.value)
			rep := check(t, planted, env, "--scenarios", scen, "--control-plane", rig.srv.URL, "--token-file", path, "--token-for", tf.side)
			if c := checkByID(t, rep, "control-plane-credential"); !strings.Contains(c.Subject, "ARGUS_CP_AUTHOR_TOKEN: an odts_ PAT") {
				t.Errorf("credential subject = %q; want the highest-precedence source, ARGUS_CP_AUTHOR_TOKEN", c.Subject)
			}
			if c := checkByID(t, rep, "local-hats"); !strings.Contains(c.Subject, "all set") || c.Status != doctor.StatusFail {
				t.Errorf("local-hats = %+v; want the three variables read (all set; presented matches neither hat)", c)
			}
			if c := checkByID(t, rep, "token-file"); !strings.Contains(c.Subject, path) {
				t.Errorf("token-file = %+v; want it to name %s — the file was not read", c, path)
			}
		}
		rig.mu.Lock()
		defer rig.mu.Unlock()
		for _, b := range rig.bearers {
			if b != author.value {
				t.Errorf("the control plane was presented a bearer that is not ARGUS_CP_AUTHOR_TOKEN")
			}
		}
		if len(rig.bearers) != 2 {
			t.Errorf("bearers presented = %d, want 2 (one per doctor run)", len(rig.bearers))
		}
	})
}

// TestDoctorTester_NoCredentialLeak: the same promise for `doctor --tester`, against the BUILT binary. The
// tester token lives in the env file (read at run time, as the headersHelper does); a second, different
// secret is planted in the environment. Neither may appear anywhere in stdout or stderr, on the passing
// path AND on the paths that print the most (a refused token, a token file that is too open).
func TestDoctorTester_NoCredentialLeak(t *testing.T) {
	bin := builtArgus(t)
	kdir := t.TempDir()
	kubectl := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"get --raw\"*) echo '{\"gitVersion\":\"v1.30.4\"}' ;;\n" +
		"  *namespace*) echo namespace/x ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(kdir, "kubectl"), []byte(kubectl), 0o755); err != nil {
		t.Fatal(err)
	}
	tok := leakMarker(t, "tester-file-token")
	envSecret := leakMarker(t, "env-author-token")
	run := func(t *testing.T, fileMode os.FileMode, cpToken string) (string, int, *testerCP) {
		t.Helper()
		cp := newTesterCP(t, cpToken, authorTools(4), "inst1", recentSeen())
		envFile := filepath.Join(t.TempDir(), "tester.env")
		if err := os.WriteFile(envFile, []byte("ARGUS_TESTER_TOKEN_MSGBUS="+tok+"\n"), fileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(envFile, fileMode); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, rc := leakRun(t, bin, map[string]string{"PATH": kdir, "ARGUS_CP_AUTHOR_TOKEN": envSecret},
			"--tester", "--env-file", envFile, "--control-plane", cp.srv.URL, "--instance", "inst1")
		all := stdout + "\n" + stderr
		for _, n := range []string{tok, envSecret} {
			if strings.Contains(all, n) {
				t.Errorf("LEAK: a planted secret appears in doctor --tester's output:\n%s", all)
			}
		}
		var rep doctor.Report
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("stdout is not one JSON report: %v\n%s", err, all)
		}
		if len(rep.Lines) == 0 || !strings.Contains(stderr, "tester-token") {
			t.Errorf("no per-phase lines:\n%s", all)
		}
		return all, rc, cp
	}
	t.Run("passing path: the file's token is the bearer, and is not printed", func(t *testing.T) {
		_, rc, cp := run(t, 0o600, tok)
		if rc != exitOK {
			t.Errorf("rc = %d", rc)
		}
		cp.mu.Lock()
		defer cp.mu.Unlock()
		if len(cp.bearers) == 0 || cp.bearers[0] != tok {
			t.Errorf("the token in the env file was not what the control plane was presented")
		}
		for _, b := range cp.bearers {
			if b == envSecret {
				t.Errorf("the environment's author token was presented; --tester must use the file's token")
			}
		}
	})
	t.Run("refused token", func(t *testing.T) {
		if _, rc, _ := run(t, 0o600, "a-different-token"); rc != exitFailed {
			t.Errorf("rc = %d, want %d", rc, exitFailed)
		}
	})
	t.Run("env file too open", func(t *testing.T) {
		if _, rc, _ := run(t, 0o644, tok); rc != exitFailed {
			t.Errorf("rc = %d, want %d", rc, exitFailed)
		}
	})
}

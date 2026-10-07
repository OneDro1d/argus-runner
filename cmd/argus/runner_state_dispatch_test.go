package main

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// VR10-U2 (V28-001) — `argus runner-state` ANSWERS WITHOUT A PRESENTED TOKEN, ON EVERY TIER.
//
// ── WHY THIS TEST DRIVES THE BINARY AND NOT THE FUNCTION ─────────────────────────────────────────
//
// runner_state_test.go calls cmdRunnerState directly, BELOW the token gate — and that is exactly the
// blindness that let V28-001 ship: the function was correct, the dispatch was on the wrong side of
// the gate, and no test ever crossed the JOIN. On compose the long-lived executor carries only
// ARGUS_RUNNER_TOKEN + ARGUS_AUTHOR_TOKEN (the SCOPE declarations) and no ARGUS_TOKEN (the
// PRESENTED credential), so `docker exec <c> argus runner-state` exited 3 ("denied: auth: a token
// is required") and update.sh read that as UNREACHABLE → refuse. k3d/managed passed only because the
// renderer injects ARGUS_TOKEN for an unrelated reason (CP-M3-III-30).
//
// So this test BUILDS the binary and RUNS it through main's command switch, with the compose
// executor's environment reproduced: scope tokens present, ARGUS_TOKEN deliberately absent (PO
// acceptance 5, 7, 9). Nothing here calls cmdRunnerState.
func TestRunnerState_DispatchAboveTheTokenGate(t *testing.T) {
	bin := buildArgusBinary(t)

	// A real key on disk, where the executor's identity actually lives — and a fake control plane
	// that answers /fed/state. The child is given ONLY these three, plus the two scope tokens.
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "identity.key")
	if err := os.WriteFile(keyPath, priv, 0o600); err != nil {
		t.Fatal(err)
	}

	// composeExecutorEnv reproduces deploy/compose/docker-compose.byo-m3.yml's long-lived executor:
	// the two scope tokens and NO ARGUS_TOKEN. Every inherited ARGUS_* is dropped first so a
	// developer shell that happens to export one cannot make this pass for the accidental k8s reason.
	composeExecutorEnv := func(cpURL string, extra ...string) []string {
		env := []string{}
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "ARGUS_") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env,
			"ARGUS_RUNNER_TOKEN=runner-scope-token-for-this-test",
			"ARGUS_AUTHOR_TOKEN=author-scope-token-for-this-test",
			"ARGUS_CP_URL="+cpURL,
			"ARGUS_INSTANCE_ID=inst-dispatch",
			"ARGUS_IDENTITY_PATH="+keyPath,
		)
		return append(env, extra...)
	}

	runBinary := func(t *testing.T, env []string, args ...string) (stdout string, rc int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		var out, errb strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", bin, err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run %s %v: %v\nstderr: %s", bin, args, err, errb.String())
				}
				rc = ee.ExitCode()
			}
		case <-time.After(60 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%s %v hung for 60s (cmdRunnerState's own timeout is 20s)", bin, args)
		}
		return out.String(), rc
	}

	// VR10-U2-2 / VR10-U2-8 / acceptance 2, 9: no ARGUS_TOKEN, no --token → rc 0 and the fence.
	t.Run("no ARGUS_TOKEN and no --token: rc 0 with running_run_id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"running_run_id":""}`))
		}))
		defer srv.Close()

		out, rc := runBinary(t, composeExecutorEnv(srv.URL), "runner-state")
		if rc != exitOK {
			t.Fatalf("`argus runner-state` with the compose executor's env exited %d, want 0.\n"+
				"stdout: %s\nThis is V28-001: the subcommand sits BELOW the token gate, so the "+
				"executor demands a credential its own container does not have, and update.sh reads "+
				"the refusal as UNREACHABLE and refuses the update.", rc, out)
		}
		if !strings.Contains(out, `"running_run_id"`) {
			t.Fatalf("rc 0 but no running_run_id on stdout — the kit cannot tell 'no run' from 'no "+
				"answer' without the field:\n%s", out)
		}
	})

	// VR10-U2-4: a failed check is a failed check. A control plane that cannot answer must exit
	// non-zero, must NOT exit 2 (that is the OLD_IMAGE "proceed" signal), and must print no fence.
	t.Run("a control plane failure is refused: non-zero, not 2, no fence", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		out, rc := runBinary(t, composeExecutorEnv(srv.URL), "runner-state")
		if rc == exitOK {
			t.Fatalf("a 500 from the control plane exited 0:\n%s", out)
		}
		if rc == exitUsage {
			t.Fatalf("a control plane failure exited %d — the code update.sh maps to OLD_IMAGE (proceed)", rc)
		}
		if strings.Contains(out, `"running_run_id"`) {
			t.Fatalf("a fence value was printed on the failure path:\n%s", out)
		}
	})

	// VR10-U2-5: the OLD_IMAGE discriminator through the real binary. An unrecognised subcommand exits
	// 2, and that is what update.sh maps to "this image predates runner-state; proceed with a note".
	// ⚠ The unknown-command arm sits BELOW the gate (only runner-state is hoisted — "nothing else"),
	// so this is driven with the token presented, as the k8s tiers do.
	t.Run("an unrecognised subcommand still exits 2 (the OLD_IMAGE signal)", func(t *testing.T) {
		out, rc := runBinary(t, composeExecutorEnv("http://127.0.0.1:1",
			"ARGUS_TOKEN=runner-scope-token-for-this-test"), "no-such-subcommand-for-runner-state")
		if rc != exitUsage {
			t.Fatalf("unknown command exited %d, want %d — update.sh's OLD_IMAGE arm depends on it:\n%s",
				rc, exitUsage, out)
		}
	})
}

// VR10-U2-1 — EXACTLY ONE DISPATCH SITE, AND IT IS ABOVE THE GATE. The process test above proves the
// behaviour; this pins the SHAPE the owner decided (reg:5335-5336): the case at the old site is deleted,
// not duplicated, so a future edit cannot quietly reintroduce a second, gated path.
func TestRunnerState_SingleDispatchSiteAboveTheGate(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	s := string(src)
	if n := strings.Count(s, `"runner-state"`); n != 1 {
		t.Fatalf("main.go names \"runner-state\" %d times, want exactly 1 dispatch site", n)
	}
	if strings.Contains(s, `case "runner-state":`) {
		t.Fatal("main.go still carries `case \"runner-state\":` in the gated switch — the hoist must MOVE " +
			"the dispatch, not add a second one")
	}
	gate := strings.Index(s, "authCfg.Verify(presented)")
	site := strings.Index(s, `"runner-state"`)
	if gate < 0 {
		t.Fatal("cannot find the token gate (authCfg.Verify(presented)) in main.go")
	}
	if site > gate {
		t.Fatalf("the runner-state dispatch (offset %d) sits BELOW the token gate (offset %d). On compose the "+
			"executor has no ARGUS_TOKEN, so the check is denied (rc 3) before it can run.", site, gate)
	}
}

// buildArgusBinary compiles THIS package into a throwaway directory so a test can drive main's
// command switch as a separate process — the only way to cross the token-gate JOIN.
//
// The output goes under GOTMPDIR when set: on this Windows host Smart App Control refuses to run a
// fresh binary from the default temp dir, and GOTMPDIR is where `go test` already runs its own.
func buildArgusBinary(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH; cannot build the binary under test")
	}
	dir, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "argus-dispatch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin := filepath.Join(dir, "argus")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

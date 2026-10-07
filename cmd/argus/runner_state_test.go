package main

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VR9-C1 — `argus runner-state`, THE SUBCOMMAND THE KIT EXECS INTO THE EXECUTOR.
//
// ── THE THREE HOPS, AND WHY THIS ONE EXISTS ───────────────────────────────────────────────────────
//
//	update.sh --exec--> the executor --mints its own JWT--> GET /fed/state --JSON--> back
//	          (B: transport)          (A: credential)       (the new endpoint)
//
// The PO framed A and B as "two mutually exclusive channels"; they are complementary. (B) is how the
// kit REACHES the executor — the same docker exec / kubectl exec `executor_version()` already uses.
// (A) is how the executor authenticates to the control plane — its own Ed25519 key, already in its
// environment. ⛔ NOTHING NEW IS PASSED IN, which is what keeps SEC-4 out of it: no credential on any
// command line, because the subcommand reads the key from the environment it is already running in.
//
// ⚠ AND update.sh IS POSIX SHELL. Minting an Ed25519 JWT in bash is not a thing, which is why this is
// a Go subcommand rather than a few lines in the kit.
func TestRunnerState(t *testing.T) {
	// A real key on disk, where the executor's identity actually lives.
	newEnv := func(t *testing.T, cpURL string) (dir string, pub ed25519.PublicKey) {
		t.Helper()
		dir = t.TempDir()
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		keyPath := filepath.Join(dir, "identity.key")
		if err := os.WriteFile(keyPath, priv, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ARGUS_CP_URL", cpURL)
		t.Setenv("ARGUS_INSTANCE_ID", "inst-rs")
		t.Setenv("ARGUS_IDENTITY_PATH", keyPath)
		return dir, pub
	}

	t.Run("it prints the fence as JSON and exits 0", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"running_run_id":"run-77"}`))
		}))
		defer srv.Close()
		newEnv(t, srv.URL)

		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRunnerState(nil) })
		if rc != exitOK {
			t.Fatalf("exit %d, want 0 — the kit reads this to decide whether an update is safe", rc)
		}
		var got struct {
			RunningRunID *string `json:"running_run_id"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("stdout is not JSON the kit can parse: %v\n%s", err, out)
		}
		if got.RunningRunID == nil || *got.RunningRunID != "run-77" {
			t.Fatalf("running_run_id = %v, want run-77", got.RunningRunID)
		}
	})

	t.Run("no run in flight prints an empty fence and exits 0", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"running_run_id":""}`))
		}))
		defer srv.Close()
		newEnv(t, srv.URL)

		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRunnerState(nil) })
		if rc != exitOK {
			t.Fatalf("exit %d for a healthy machine with no run — this is the ORDINARY case and the "+
				"one that permits an update", rc)
		}
		if !strings.Contains(string(out), `"running_run_id"`) {
			t.Fatalf("the field is absent from a successful answer: %s\n\nThe kit must be able to tell "+
				"'no run' from 'no answer', and an absent field is the second.", out)
		}
	})

	// ⛔ A FAILURE MUST NOT LOOK LIKE AN EMPTY FENCE. "" permits an update, so a control plane that
	// refuses or cannot be reached must exit non-zero AND must not print a running_run_id — a shell
	// parsing stdout would otherwise read the failure as permission to proceed.
	t.Run("a refusing control plane exits non-zero and prints no fence", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"reason":"bad_signature"}`))
		}))
		defer srv.Close()
		newEnv(t, srv.URL)

		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRunnerState(nil) })
		if rc == exitOK {
			t.Fatal("exit 0 on a 401 — the kit would read that as a successful check")
		}
		if strings.Contains(string(out), `"running_run_id"`) {
			t.Fatalf("stdout carries a fence value on the failure path: %s", out)
		}
	})

	// ⛔ AND THE FAILURE CODE MUST NOT BE THE *UNKNOWN SUBCOMMAND* CODE. This is SA §0.10's whole
	// discriminator: an image that predates this subcommand exits exitUsage (main.go:500, "unknown
	// command"), and the kit must PROCEED on that — otherwise the entire existing estate would need
	// --force to reach the build that fixes V27-001, and VR9-C1 acceptance 1 fails on every machine at
	// rollout. A dead container or a refusing CP must REFUSE. Collapsing the two re-creates the false
	// emergency update.sh's own comment warns about.
	t.Run("a runtime failure is distinguishable from an unrecognised subcommand", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		newEnv(t, srv.URL)

		var rc int
		_ = captureEmitRaw(t, func() { rc = cmdRunnerState(nil) })
		if rc == exitUsage {
			t.Fatalf("a runtime failure exited %d, which is the code an image WITHOUT this subcommand "+
				"returns. The kit cannot then tell 'this check failed' from 'this image is too old', "+
				"and one of those must refuse while the other must proceed.", rc)
		}
		if rc == exitOK {
			t.Fatal("a 500 exited 0")
		}
	})

	// Missing configuration is a failure, not a silent success.
	t.Run("no control plane configured exits non-zero", func(t *testing.T) {
		newEnv(t, "")
		t.Setenv("ARGUS_CP_URL", "")
		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRunnerState(nil) })
		if rc == exitOK {
			t.Fatal("exit 0 with no control plane to ask")
		}
		if strings.Contains(string(out), `"running_run_id"`) {
			t.Errorf("a fence value was printed with no control plane: %s", out)
		}
	})

	t.Run("an unreadable identity exits non-zero", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"running_run_id":""}`))
		}))
		defer srv.Close()
		dir, _ := newEnv(t, srv.URL)
		t.Setenv("ARGUS_IDENTITY_PATH", filepath.Join(dir, "nope.key"))

		var rc int
		out := captureEmitRaw(t, func() { rc = cmdRunnerState(nil) })
		if rc == exitOK {
			t.Fatal("exit 0 with no identity to sign with")
		}
		if strings.Contains(string(out), `"running_run_id"`) {
			t.Errorf("a fence value was printed with no identity: %s", out)
		}
	})
}

// ⛔ THE DISCRIMINATOR ITSELF, PINNED. SA §0.10 splits three cases, and the split depends on an
// unrecognised subcommand exiting a DIFFERENT code from a failing one. If main's unknown-command exit
// ever changes, update.sh's "PROCEED on an old image" arm silently becomes "REFUSE on an old image" —
// which is the state where every machine in the estate needs --force to take the build that fixes
// V27-001.
func TestUnknownCommand_ExitCodeIsTheOldImageSignal(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), `emitErr(exitUsage, "unknown command %q", cmd)`) {
		t.Fatal("main's unknown-command path no longer exits exitUsage. update.sh distinguishes 'this " +
			"image predates runner-state' (PROCEED) from 'the check failed' (REFUSE) by that code, and " +
			"collapsing them turns every older image into a false emergency.")
	}
	if exitUsage == exitErr || exitUsage == exitOK {
		t.Fatalf("exitUsage (%d) collides with exitErr (%d) / exitOK (%d) — the three cases are no "+
			"longer distinguishable", exitUsage, exitErr, exitOK)
	}
	// VR10-U2-7 (V28-001): exitDenied is ENUMERATED TOO. It is the code the token gate returns, and
	// this check used to list exitUsage / exitErr / exitOK and never it — which is how a rc 3 on
	// compose went unnoticed. update.sh's classify_run_state maps it by its "anything else" arm to
	// UNREACHABLE (REFUSE), and that bucket is chosen on purpose: a denied check is a failed check and
	// must never read as OLD_IMAGE (proceed) or CLEAR (proceed). With runner-state above the gate that
	// subcommand can no longer produce the code, but the bucket it lands in stays pinned.
	if exitDenied == exitUsage || exitDenied == exitOK {
		t.Fatalf("exitDenied (%d) collides with exitUsage (%d) / exitOK (%d) — a denied check would be "+
			"read as an old image (proceed) or a clear fence (proceed)", exitDenied, exitUsage, exitOK)
	}
}

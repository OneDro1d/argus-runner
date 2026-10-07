//go:build !windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a mutex-guarded bytes.Buffer: exec.Cmd's own internal io.Copy goroutines write to it
// for as long as the child is alive, while this test's main goroutine may read it (for a failure
// message) BEFORE the child has exited — a plain bytes.Buffer would race on exactly that overlap.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// up_signal_forward_test.go — #46(signal): `upFirstRun`'s --json mode starts onboard.sh with
// `Setsid: true` (#47 — detaching it from a controlling terminal so its own tier/secrets/etc.
// questions cannot block on one), which ALSO puts onboard.sh in a brand-new process group. A
// SIGINT/SIGTERM delivered to `up` itself (Ctrl-C, a CI cancellation, an agent giving up) no longer
// reaches a child in a DIFFERENT group — without forwarding, killing `up` left onboard.sh running on,
// orphaned, still pulling images or registering an instance nobody is waiting on anymore.
//
// ⛔ THIS NEEDS A SEPARATE PROCESS, same reasoning as up_pty_detach_test.go's own note: proving the
// signal reaches onboard.sh's process group means signalling a REAL OS process running `up` itself,
// not the in-process cmdUp() call upRun (up_json_protocol_test.go) uses everywhere else in this
// package — a signal sent to the test binary's own pid would not exercise upFirstRun's forwarding at
// all. ARGUS_UP_SIGNAL_HELPER selects the same re-exec-self helper-process mode
// (os/exec_test.go's TestHelperProcess pattern) up_pty_detach_test.go's ARGUS_UP_PTY_HELPER already
// uses: args travel through an env var, never argv, so the re-exec'd binary's own `-test.*` flag
// parsing never sees them.
func TestUp_JSON_SIGTERM_ForwardedToOnboardProcessGroup(t *testing.T) {
	if os.Getenv("ARGUS_UP_SIGNAL_HELPER") == "1" {
		args := strings.Split(os.Getenv("ARGUS_UP_SIGNAL_HELPER_ARGS"), "\x1f")
		os.Exit(cmdUp(args))
		return
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if strings.ContainsAny(self, "'\n") {
		t.Skip("test binary path is not safely shell-quotable")
	}

	sand := t.TempDir()
	prod := filepath.Join(sand, "product")
	scen := filepath.Join(sand, "scenarios")
	if err := os.MkdirAll(prod, 0o755); err != nil {
		t.Fatalf("mkdir product: %v", err)
	}
	if err := os.MkdirAll(scen, 0o755); err != nil {
		t.Fatalf("mkdir scenarios: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prod, "argus-config.yaml"), []byte("project:\n  name: probe\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// A MINIMAL fake onboard.sh — this test is about signal delivery to its process group, not about
	// anything a real onboard.sh does. It reports its OWN pid (bash's, the process `up` actually
	// starts and Setsid makes the group leader) and then sleeps, standing in for a long-running step
	// (an image pull, a cluster wait) a real onboard.sh could be blocked on when `up` is killed.
	kit := t.TempDir()
	if err := os.MkdirAll(filepath.Join(kit, "onboarding"), 0o755); err != nil {
		t.Fatalf("mkdir onboarding: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "onboard.pid")
	fakeScript := fmt.Sprintf("#!/usr/bin/env bash\necho $$ > %q\nsleep 60\n", pidFile)
	if err := os.WriteFile(filepath.Join(kit, "onboarding", "onboard.sh"), []byte(fakeScript), 0o755); err != nil {
		t.Fatalf("write fake onboard.sh: %v", err)
	}

	upArgsForHelper := []string{
		"--config", filepath.Join(prod, "argus-config.yaml"),
		"--scenarios-dir", scen,
		"--runner-id", "probe-sigterm",
		"--json", "--yes",
	}

	cmd := exec.Command(self, "-test.run=^TestUp_JSON_SIGTERM_ForwardedToOnboardProcessGroup$", "-test.v=false")
	cmd.Dir = kit
	cmd.Env = append(os.Environ(),
		"ARGUS_UP_SIGNAL_HELPER=1",
		"ARGUS_UP_SIGNAL_HELPER_ARGS="+strings.Join(upArgsForHelper, "\x1f"),
		"ARGUS_ROUTER_STATE="+filepath.ToSlash(filepath.Join(t.TempDir(), "argus", "router")),
	)
	outBuf := &syncBuffer{}
	cmd.Stdout = outBuf
	cmd.Stderr = outBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper `up` process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for the fake onboard.sh to report its own pid — proof `up` actually reached cmd.Start()
	// for it before this test signals `up` itself.
	var onboardPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && onboardPID == 0 {
		b, rerr := os.ReadFile(pidFile)
		if rerr == nil {
			if n, aerr := strconv.Atoi(strings.TrimSpace(string(b))); aerr == nil && n > 0 {
				onboardPID = n
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if onboardPID == 0 {
		t.Fatalf("fake onboard.sh never wrote its pid to %s within the deadline; helper output:\n%s",
			pidFile, outBuf.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the helper `up` process: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case <-waitDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("helper `up` process did not exit after SIGTERM; helper output:\n%s", outBuf.String())
	}

	// The fake onboard.sh must be gone within a few seconds of `up` itself exiting — if the signal was
	// never forwarded to its process group, it is still sleeping right now.
	gone := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if killErr := syscall.Kill(onboardPID, 0); killErr != nil {
			gone = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gone {
		_ = syscall.Kill(onboardPID, syscall.SIGKILL) // don't leak a live process out of a failed test
		t.Fatalf("fake onboard.sh (pid %d) was still alive after `up` exited from SIGTERM — the signal "+
			"was not forwarded to its process group; helper output:\n%s", onboardPID, outBuf.String())
	}
}

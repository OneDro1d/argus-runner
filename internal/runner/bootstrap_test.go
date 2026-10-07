package runner

// bootstrap_test.go — Bootstrap actually STARTS when a control plane is configured.
//
// ── WHY THIS FILE EXISTS ──────────────────────────────────────────────────────────────────────────
//
// VR-V5 added one line to Bootstrap:
//
//	s.exec.Floors = NewFloorStore(cfg.Fed.IdentityPath)
//
// twenty-six lines BEFORE `s.exec = &Executor{…}`. `exec` is a *Executor, so it was nil, and every
// executor with a CP configured died on its first start:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	  internal/runner.Bootstrap … runner.go:87
//
// It shipped in 0.3.19 and was found by the FIRST REAL ONBOARD — the executor crash-looped 11 times
// and onboarding reported "the executor did not REGISTER within ~60s", which points at the network.
//
// ── WHAT LET IT THROUGH, WHICH IS THE ACTUAL DEFECT ───────────────────────────────────────────────
//
// Every §V unit test passed. FloorStore was tested directly, FloorState was tested directly, 28 Go
// packages were green, both publish gates passed, and `argus version` answered on both
// architectures — because NONE of that executes Bootstrap's CP branch. The whole §V feature was
// tested at the unit level and never once WIRED.
//
// That is this round's own lesson landing on this round's own code: a compile is not a render, and a
// green unit suite is not a start. So the test here is deliberately shallow and deliberately real —
// it calls the actual constructor by the actual path a container takes.

import (
	"context"
	"path/filepath"
	"testing"
)

// The CP branch is the one that runs in production and the one that was never executed by a test.
// It does not need a reachable control plane: the panic was in CONSTRUCTION, long before any request.
func TestBootstrap_WithControlPlane_DoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr: "127.0.0.1:0", // ask the OS for a port; a fixed one makes this flaky under -race
		Fed: FedConfig{
			CPURL:        "http://127.0.0.1:1", // never dialled during Bootstrap
			InstanceID:   "boot-test",
			IdentityPath: filepath.Join(dir, "identity.key"),
			Exec:         ExecConfig{ResultsRoot: dir},
		},
	}

	srv, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Bootstrap returned an error: %v", err)
	}
	if srv == nil {
		t.Fatal("Bootstrap returned no server and no error")
	}
	// Server exposes no Close; Run owns the listener's lifetime and this test never calls Run. The
	// bound socket is released when the test binary exits — acceptable for two short cases, and
	// preferable to inventing a shutdown path just to satisfy a test.
	t.Cleanup(func() { _ = srv.ln })

	// The executor must EXIST — the nil it used to be is what was dereferenced.
	if srv.exec == nil {
		t.Fatal("the executor was not constructed, so nothing downstream can work")
	}
	// And VR-V5's store must be wired to it, which is the thing the broken line was trying to do.
	if srv.exec.Floors == nil {
		t.Error("Floors is nil — the executor cannot rank itself against the published floors, " +
			"so VR-V2/VR-V4 silently do nothing")
	}
}

// The no-CP path is the local fast loop. It must stay constructible too, and it must NOT build a
// federation executor — asserted so a future fix for the above does not "helpfully" build one here.
func TestBootstrap_WithoutControlPlane_BuildsNoExecutor(t *testing.T) {
	srv, err := Bootstrap(context.Background(), Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Bootstrap (no CP) returned an error: %v", err)
	}
	if srv.exec != nil {
		t.Error("a federation executor was built with no control plane configured")
	}
}

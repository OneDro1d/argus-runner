package router

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// ── #607: THE ROUTER SAYS WHICH PORT IT BOUND ──────────────────────────────────────
//
// Native onboarding used to learn the router's port by reading state.json and asking /ready there. Both
// halves answer for whoever else is around: state.json is shared by every router serving that state dir,
// and /ready answers for whatever holds the port. Measured 2026-10-07: a router started by hand from an
// older kit held 9765, onboarding's own router took 9766, and onboarding reported "started natively on
// port 9765 (pid <the 9766 process>)" because the OTHER router answered /ready.
//
// The one process that knows which port it bound is the process that bound it, and only after the bind
// succeeded. So `router serve` writes {pid, port} to the file BoundFileEnv names, AFTER net.Listen
// returned. Onboarding sets the variable for the process it starts and accepts the port only when the
// pid in the file is the pid it started. The variable is opt-in: a router nobody asked (compose, a
// by-hand start) writes nothing, so it cannot overwrite the answer onboarding is waiting for.

// BoundFileEnv names the file `router serve` writes after binding. Unset = nothing is written.
const BoundFileEnv = "ARGUS_ROUTER_BOUND_FILE"

// Bound is the content of that file.
type Bound struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// ListenerPort is the TCP port a listener actually holds (the bound one, not the one asked for: ":0"
// asks for any).
func ListenerPort(ln net.Listener) (int, error) {
	if a, ok := ln.Addr().(*net.TCPAddr); ok && a.Port > 0 {
		return a.Port, nil
	}
	return 0, fmt.Errorf("router: the listener's address %q is not a TCP port", ln.Addr().String())
}

// WriteBoundFile writes {pid, port} to path atomically (temp file + rename in the same directory), so a
// reader polling for it sees either the old content or the whole new one, never half a write.
func WriteBoundFile(path string, pid, port int) error {
	if path == "" {
		return fmt.Errorf("router: no bound-port file named")
	}
	blob, err := json.Marshal(Bound{PID: pid, Port: port})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

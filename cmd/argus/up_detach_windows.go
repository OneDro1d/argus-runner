//go:build windows

package main

import "os/exec"

// detachFromTerminal is a no-op on Windows: there is no controlling terminal to detach from in the
// POSIX sense, and onboarding runs under bash there with its own console handling.
func detachFromTerminal(cmd *exec.Cmd) {}

// forwardSignals is a no-op on Windows: a console break already reaches every process attached to
// the console, and there is no process group to signal by negative pid.
func forwardSignals(cmd *exec.Cmd) func() { return func() {} }

//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// detachFromTerminal starts the child in a new session with no controlling terminal, so every
// question it would otherwise ask on /dev/tty takes its non-interactive path instead.
func detachFromTerminal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// forwardSignals relays SIGINT and SIGTERM delivered to this process to the child's whole process
// group (the child is its group leader after detachFromTerminal), so a cancelled `up` does not leave
// onboarding running on its own. The returned func stops the relay; call it once the child has exited.
func forwardSignals(cmd *exec.Cmd) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				if s, ok := sig.(syscall.Signal); ok && cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, s)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}

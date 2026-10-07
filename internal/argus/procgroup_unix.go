//go:build unix

package argus

import (
	"os/exec"
	"syscall"
)

// killWholeGroup makes cmd the leader of its OWN process group and makes the context's cancellation (the
// backstop / cleanup deadline) SIGKILL that whole group.
//
// `jmeter` is a shell wrapper and the JVM is its CHILD: exec.CommandContext signals only the wrapper, so the
// java grandchild used to survive the backstop and went on publishing for 30+ s after "stopped after 1m51s".
// Killing -pgid takes the wrapper, the JVM and anything either forked. cmd.WaitDelay (set by the caller) is
// untouched: it still bounds how long Wait blocks on a pipe something else holds open.
//
// ⛔ Deleting the CALL to this function is the wiring mutation TestLocalRunner_BackstopKillsTheWholeProcessGroup
// is built to catch.
func killWholeGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill() // the group is already gone or unreachable: at least the leader
		}
		return nil
	}
}

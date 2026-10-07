//go:build !unix

package argus

import "os/exec"

// killWholeGroup is a no-op where process groups do not exist: exec.CommandContext's own kill of the
// direct child stands. The executor image is linux; this keeps the package building elsewhere.
func killWholeGroup(cmd *exec.Cmd) {}

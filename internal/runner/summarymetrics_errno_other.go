//go:build !windows

package runner

import "syscall"

// The socket errors of a refused and of a reset connection (Linux, macOS and the other Unix systems: the native numbers).
// Windows has its own list (summarymetrics_errno_windows.go).
var (
	connRefusedErrnos = []syscall.Errno{syscall.ECONNREFUSED}
	connResetErrnos   = []syscall.Errno{syscall.ECONNRESET, syscall.EPIPE, syscall.ECONNABORTED}
)

//go:build windows

package runner

import "syscall"

// On Windows a socket failure is a WSA number. syscall.ECONNREFUSED and syscall.ECONNRESET are numbers Go invented there
// (APPLICATION_ERROR + n, syscall/zerrors_windows.go) and syscall.Errno.Is maps neither (syscall_windows.go), so they
// never match a real failure: measured with Go 1.26.5 on Windows (2026-10-17) — a refused connection is 10061
// under "connectex", a reset one 10054 under "wsarecv". WSAECONNREFUSED (10061) has no name in package syscall.
var (
	connRefusedErrnos = []syscall.Errno{syscall.ECONNREFUSED, syscall.Errno(10061)}
	connResetErrnos   = []syscall.Errno{syscall.ECONNRESET, syscall.EPIPE, syscall.ECONNABORTED,
		syscall.WSAECONNRESET, syscall.WSAECONNABORTED}
)

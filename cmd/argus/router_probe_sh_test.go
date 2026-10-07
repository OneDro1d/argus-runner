package main

import (
	"strings"
)

// toBashPath renders a Windows path the way Git Bash reads it (C:\x → /c/x); a POSIX path is returned as is.
func toBashPath(p string) string {
	if len(p) > 2 && p[1] == ':' {
		return "/" + strings.ToLower(p[:1]) + strings.ReplaceAll(p[2:], `\`, "/")
	}
	return p
}

package scenario

import (
	"fmt"
	"regexp"
	"strings"
)

// Issue #709: A ```bash CLEANUP BLOCK RUNS UNDER `sh -c`.
//
// Both executors run a bash cleanup as `sh -c <body>`, and on the executor image /bin/sh is dash.
// Bash-only syntax therefore fails at once, and because a cleanup's output is deliberately discarded
// the report could only say "did not complete". The decision is to KEEP `sh -c` —
// existing blocks must behave exactly as today — and make the trap visible instead: this warning at
// authoring time, one sentence in the docs, and a clearer report line (internal/argus/cleanup.go).
//
// ⚠ A WARNING, NEVER AN ERROR: the write still succeeds. The detector is a deliberate approximation:
// it blanks comment lines and single-quoted strings (so `# [[ x ]]` and `echo '<<<'` stay quiet) but
// it does not parse shell, so a construct inside a heredoc body may still warn. A false warning is
// acceptable here; a missed one only means the author finds out from the report line.

var bashismChecks = []struct {
	name string
	re   *regexp.Regexp
}{
	{"here-string `<<<`", regexp.MustCompile(`<<<`)},
	{"`[[ … ]]`", regexp.MustCompile(`(^|[\s;&|(])\[\[(\s|$)`)},
	{"array assignment `name=(`", regexp.MustCompile(`(^|[\s;&|])[A-Za-z_][A-Za-z0-9_]*\+?=\(`)},
	{"array expansion `${name[`", regexp.MustCompile(`\$\{[#!]?[A-Za-z_][A-Za-z0-9_]*\[`)},
	{"pattern substitution `${var/…}`", regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*//?`)},
	{"`set -o pipefail`", regexp.MustCompile(`\bpipefail\b`)},
	{"the `function` keyword", regexp.MustCompile(`(^|[;&|{(]\s*|\s)function\s+[A-Za-z_]`)},
	{"process substitution `<(` / `>(`", regexp.MustCompile(`[<>]\(`)},
	{"`==` inside `[ ]`", regexp.MustCompile(`(^|[\s;&|(])\[\s[^\]]*==[^\]]*\]`)},
}

// blankShellNoise returns body with comment lines removed and single-quoted string contents blanked.
// ansiC reports a `$'…'` string seen outside any quote (it is itself a bash-only construct).
func blankShellNoise(body string) (code string, ansiC bool) {
	var kept []string
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		kept = append(kept, ln)
	}
	src := strings.Join(kept, "\n")
	var b strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
				b.WriteByte(c)
			} else if c == '\n' {
				b.WriteByte(c)
			} else {
				b.WriteByte(' ')
			}
		case inDouble:
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(c)
				i++
				b.WriteByte(src[i])
				continue
			}
			if c == '"' {
				inDouble = false
			}
			b.WriteByte(c)
		default:
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(c)
				i++
				b.WriteByte(src[i])
				continue
			}
			switch c {
			case '"':
				inDouble = true
			case '\'':
				if i > 0 && src[i-1] == '$' {
					ansiC = true
				}
				inSingle = true
			}
			b.WriteByte(c)
		}
	}
	return b.String(), ansiC
}

// bashismsIn lists the bash-only constructs found in a cleanup body.
func bashismsIn(body string) []string {
	code, ansiC := blankShellNoise(body)
	var found []string
	for _, ch := range bashismChecks {
		if ch.re.MatchString(code) {
			found = append(found, ch.name)
		}
	}
	if ansiC {
		found = append(found, "`$'…'` strings")
	}
	return found
}

// cleanupShellWarnings is one warning per ```bash CLEANUP block that uses bash-only syntax.
func cleanupShellWarnings(s *Scenario) []string {
	var w []string
	for i, b := range s.Cleanup.Blocks {
		if b.Form != CleanupBash {
			continue
		}
		found := bashismsIn(b.Body)
		if len(found) == 0 {
			continue
		}
		w = append(w, fmt.Sprintf("## CLEANUP block %d (bash) uses bash-only syntax: %s. The block runs under POSIX `sh -c` "+
			"(dash on the executor image), not bash, so it will fail with a syntax error. Write POSIX sh "+
			"", i+1, strings.Join(found, ", ")))
	}
	return w
}

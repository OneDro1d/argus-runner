// Package envname is the ONE place that knows about the two env-var renames landing in this
// release:
//
//	ARGUS_AUTHOR_TOKEN -> ARGUS_EXECUTOR_SECRET   (the in-env test-scope token: the executor's
//	                                                local MCP gate, and the control plane's static
//	                                                fallback bearer — internal/auth.Config.AuthorToken)
//	ARGUS_CP_TOKEN     -> ARGUS_CP_AUTHOR_TOKEN    (a PERSON's author session token presented to the
//	                                                control plane: cloud-* commands, runner-id,
//	                                                certificate get, hub discovery, up, preflight,
//	                                                onboard.sh/teardown.sh, the runner client)
//
// Neither ARGUS_RUNNER_TOKEN nor ARGUS_TOKEN is touched by this package or this release.
//
// Every non-test Go read of the old names goes through Lookup so the fallback-with-one-warning
// behaviour lives in exactly one function, rather than being re-implemented (and re-drifted) at
// each of the dozen or so call sites that used to call os.Getenv directly.
package envname

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

const (
	// ExecutorSecret is the new name for the in-env test-scope token.
	ExecutorSecret = "ARGUS_EXECUTOR_SECRET"
	// ExecutorSecretDeprecated is the old name for ExecutorSecret. Still read as a fallback this
	// release; removal is a later release's decision.
	ExecutorSecretDeprecated = "ARGUS_AUTHOR_TOKEN"

	// CPAuthorToken is the new name for a person's author session token.
	CPAuthorToken = "ARGUS_CP_AUTHOR_TOKEN"
	// CPAuthorTokenDeprecated is the old name for CPAuthorToken. Still read as a fallback this
	// release; removal is a later release's decision.
	CPAuthorTokenDeprecated = "ARGUS_CP_TOKEN"
)

var (
	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

// deprecationMessage (item 9, msgbus tester 2026-09-28: "the executor's startup line 'ARGUS_AUTHOR_
// TOKEN is deprecated' must name the fix... and say when the alias ends") is the FULL warning line
// for a deprecated var that has one — the concrete remedy and a FIXED removal floor, printed INSTEAD
// OF the generic "will be removed in a later release" line, which told the reader nothing they could
// act on or plan around. Only ExecutorSecretDeprecated has an entry: it is the one pair with a
// server-side migration command (another builder is adding `argus secrets migrate`, item 3's gap);
// CPAuthorTokenDeprecated is a person's own session token, nothing to migrate server-side, so it
// keeps the generic wording (empty entry ⇒ warnOnce falls back to it).
var deprecationMessage = map[string]string{
	ExecutorSecretDeprecated: "warning: %[1]s is deprecated; use %[2]s instead. Run `argus secrets migrate` " +
		"to rewrite the rendered Secret under the new name (the value is never printed). The alias %[1]s " +
		"is read on every 0.3.x release and will not be removed before v0.4.0.\n",
}

// Lookup resolves an aliased environment variable pair: newName always wins when it is set to a
// non-empty value — including when oldName is ALSO set, and silently, with no warning, because
// that is the caller doing exactly what the deprecation asks. Only when newName is unset or empty
// does Lookup fall back to oldName, and the FIRST time that happens for a given oldName in this
// process it writes one deprecation line to stderr naming both vars and warning that the old name
// will be removed in a later release. Every later call with the same oldName returns the fallback
// value again but stays silent — the warning is a per-name-per-process event, not a per-call one.
//
// The warning is written to STDERR ONLY, never stdout: several CLI commands in this repo emit one
// JSON object per line on stdout (see cmd/argus/up.go's --json protocol, and every emitErr/emit
// call in cmd/argus), and a warning line interleaved there would corrupt that wire format for
// whatever is parsing it.
//
// The warning never contains the VALUE of either variable — only the two names — so it is safe to
// print even when the old variable holds a live credential.
func Lookup(newName, oldName string) string {
	if v := os.Getenv(newName); v != "" {
		return v
	}
	v, isSet := os.LookupEnv(oldName)
	if !isSet || v == "" {
		return ""
	}
	warnOnce(newName, oldName)
	return v
}

// WarnedEnv is the environment variable that carries "already warned" across processes: a
// comma-separated list of OLD names (never values). The first process to warn for an old name appends
// it, and every child inherits it, so one onboarding — which starts several argus processes, some in
// containers — prints each deprecation once, not once per process. onboarding's
// shell (lib/envname.sh) writes the same marker.
const WarnedEnv = "ARGUS_DEPRECATION_WARNED"

func warnedByAncestor(oldName string) bool {
	for _, n := range strings.Split(os.Getenv(WarnedEnv), ",") {
		if strings.TrimSpace(n) == oldName {
			return true
		}
	}
	return false
}

func warnOnce(newName, oldName string) {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if warned[oldName] {
		return
	}
	warned[oldName] = true
	if warnedByAncestor(oldName) {
		return
	}
	if cur := os.Getenv(WarnedEnv); cur == "" {
		_ = os.Setenv(WarnedEnv, oldName)
	} else {
		_ = os.Setenv(WarnedEnv, cur+","+oldName)
	}
	format := deprecationMessage[oldName]
	if format == "" {
		format = "warning: %s is deprecated and will be removed in a later release; use %s instead\n"
	}
	fmt.Fprintf(os.Stderr, format, oldName, newName)
}

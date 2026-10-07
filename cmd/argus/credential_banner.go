package main

// credential_banner.go — F-CRED-1: a tester (msgbus, 2026-09-27) acted in the wrong Argus workspace
// without noticing. `argus cloud-*` commands (and runner-id / certificate get, which authenticate the
// same way) read --token, then ARGUS_CP_AUTHOR_TOKEN / the deprecated ARGUS_CP_TOKEN
// (internal/envname.Lookup), and otherwise fall back SILENTLY to ~/.config/argus/session.json from
// `argus cloud-login` — author scope over whatever workspace that session last used. Nothing ever
// told the caller which of those three it got.
//
// credentialBanner prints ONE line to STDERR, before the control-plane call, naming the credential
// source and the workspace it acts in — never the token value. stdout stays pure JSON (the CLI's
// wire format); this is stderr only, exactly like the deprecation warning envname.Lookup already
// prints there.
//
// It does not change which credential is used or refuse anything — onboard.sh/teardown.sh
// deliberately export `ARGUS_CP_TOKEN="${ARGUS_CP_TOKEN:-}"` (empty) and rely on the session-file
// fallback (onboarding/onboard.sh:723-729, teardown.sh similarly); refusing on an empty var would
// break onboarding. The one exception is a LOUD warning line — still no refusal — when a token var is
// SET but EMPTY and the command is about to fall back to the session file, so that shape is visible
// rather than silently "working".

import (
	"fmt"
	"os"

	"github.com/OneDro1d/argus-runner/internal/envname"
	"github.com/OneDro1d/argus-runner/internal/onboard"
)

// unknownWorkspace is what the banner says when the credential's workspace cannot be determined
// locally — an opaque `odts_` author PAT carries no decodable claims, so guessing would be worse
// than saying so (PROMISE #1: "say so honestly ... rather than guessing").
const unknownWorkspace = "bound to the token, not known locally"

// describeEnvToken mirrors envname.Lookup's OWN precedence (the new name wins whenever it holds a
// non-empty value; only then does the deprecated name get a look) so the reported source can never
// disagree with the token Lookup actually resolves. It never emits Lookup's deprecation warning
// itself — Lookup owns that side effect and is still called, separately, for the real value — and it
// never returns anything that gets printed as-is; workspaceOf decodes claims out of the token, the
// token string itself is discarded by every caller here.
func describeEnvToken() (token, source string) {
	if v := os.Getenv(envname.CPAuthorToken); v != "" {
		return v, envname.CPAuthorToken
	}
	if v, ok := os.LookupEnv(envname.CPAuthorTokenDeprecated); ok && v != "" {
		return v, envname.CPAuthorTokenDeprecated + " (deprecated, use " + envname.CPAuthorToken + ")"
	}
	return "", ""
}

// explicitlyEmptyTokenVar names a CP-author-token env var that is SET but EMPTY — the shape
// onboard.sh/teardown.sh deliberately produce when they want the session-file fallback to run
// (`ARGUS_CP_TOKEN="${ARGUS_CP_TOKEN:-}"`). "" when neither var is set at all (a plain unset is not
// this case — nothing to warn about). Callers must only ask this once describeEnvToken has already
// found no usable value, or a var that resolves via the OTHER name would be reported as "empty".
func explicitlyEmptyTokenVar() string {
	if v, ok := os.LookupEnv(envname.CPAuthorToken); ok && v == "" {
		return envname.CPAuthorToken
	}
	if v, ok := os.LookupEnv(envname.CPAuthorTokenDeprecated); ok && v == "" {
		return envname.CPAuthorTokenDeprecated
	}
	return ""
}

// workspaceOf decodes the OAuth "workspace" claim out of tok (onboard.WorkspaceFromToken), honestly
// reporting unknownWorkspace when tok is not a JWT (an opaque `odts_` author PAT) or carries no such
// claim — never a guess, and never the token itself.
func workspaceOf(tok string) string {
	if ws := onboard.WorkspaceFromToken(tok); ws != "" {
		return ws
	}
	return unknownWorkspace
}

// credentialBanner prints, on stderr, which credential a control-plane-authenticating command is
// about to present and which workspace it acts in — one line, before the call, the token VALUE never
// appearing anywhere. cpURL is the resolved control-plane base (e.g. client.BaseURL); flagToken is
// whatever --token already held (empty when the flag was not given).
func credentialBanner(cpURL, flagToken string) {
	// cpURL is caller-supplied and can carry a credential (userinfo, ?token=,
	// #fragment). Every line below prints cp, never cpURL. Plain display, not shell-quoted: the banner
	// is read, not pasted.
	cp := displayControlPlane(cpURL)
	if flagToken != "" {
		fmt.Fprintf(os.Stderr, "argus: control plane %s · credential: --token flag · workspace: %s\n",
			cp, workspaceOf(flagToken))
		return
	}
	if envTok, envSrc := describeEnvToken(); envTok != "" {
		fmt.Fprintf(os.Stderr, "argus: control plane %s · credential: %s · workspace: %s\n",
			cp, envSrc, workspaceOf(envTok))
		return
	}
	// Nothing explicit — about to fall back to the session file `argus cloud-login` wrote. A token
	// var that is SET but EMPTY (the onboard.sh/teardown.sh shape) gets a LOUD warning naming it —
	// never a refusal (PROMISE #1).
	if empty := explicitlyEmptyTokenVar(); empty != "" {
		fmt.Fprintf(os.Stderr, "argus: WARNING: %s is set but empty — falling back to the session file\n", empty)
	}
	path, perr := onboard.DefaultSessionPath()
	if v := os.Getenv(onboard.SessionEnvOverride); v != "" {
		path, perr = v, nil
	}
	if perr != nil {
		fmt.Fprintf(os.Stderr, "argus: control plane %s · credential: none found (no --token/%s, and the session path could not be resolved: %v)\n",
			cp, envname.CPAuthorToken, perr)
		return
	}
	d, lerr := onboard.LoadSessionFile(path)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "argus: control plane %s · credential: none found (no --token/%s, and no session at %s — run `argus cloud-login`)\n",
			cp, envname.CPAuthorToken, path)
		return
	}
	ws := d.Workspace
	if ws == "" {
		ws = workspaceOf(d.AccessToken)
	}
	fmt.Fprintf(os.Stderr, "argus: control plane %s · credential: session file %s (author) · workspace: %s\n",
		cp, path, ws)
}

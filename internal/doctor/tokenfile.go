package doctor

import (
	"fmt"
	"path/filepath"
	"time"
)

// Sides a token file can be meant for (`argus doctor --token-for`).
const (
	SideAuthor = "author" // the operator's side: the author__* tools
	SideRunner = "runner" // the builder's side of a hub pairing: runner__run
)

// TokenFileInput is what the caller read from a token file about to be handed to someone else — wrapped
// in Vault for a hub connection, say. The value is not a field: classify it, pass the facts.
type TokenFileInput struct {
	Path string
	// Err is why the file could not be read. The check is then unknown: nothing was examined.
	Err string
	// Values is how many whitespace-separated values the file holds. Facts means something only when it is 1.
	Values int
	Facts  TokenFacts
	// For is the side the token is going to, as the operator said: SideAuthor, SideRunner, or "" (not said).
	For             string
	ControlPlaneURL string
	Now             time.Time
}

// CheckTokenFile names what a token file holds BEFORE it leaves the machine. On 2026-09-25 a Hub
// test window was lost because a freshly minted author PAT went out in the Vault wrap meant for the
// builder instead of runner-token.txt, and the hub's connection check said "verified" for both: it
// proves a token authenticates, not its kind (incidents B:66, B:67, B:71, B:78). The difference was in
// the files all along — a JWT with scope runner, an odts_ PAT — and nothing read them.
//
// What it cannot see: an odts_ value is an author PAT or, since #259, a builder token (a runner-scope
// PAT bound to one workspace). The prefix is shared and only the control plane's scope column tells
// them apart (store/tokens.go), so it says so rather than guess. It leans on the default only this far:
// every mint is author scope unless the Tokens page was asked for a builder token, so an odts_ file
// headed for the runner side is a warn, and one headed for the author side is not.
func CheckTokenFile(in TokenFileInput) Check {
	c := Check{
		ID:      "token-file",
		What:    "the token file you are about to hand off holds the kind of credential its receiver needs, and will still be valid when it arrives",
		Subject: in.Path,
	}
	switch {
	case in.Err != "":
		c.Status = StatusUnknown
		c.Detail = "it could not be read: " + in.Err + ". Nothing is known about what it holds."
		c.Fix = "check the path and its permissions, then: argus doctor --token-file " + in.Path
		return c
	case in.Values == 0:
		c.Subject = in.Path + ": empty"
		c.Status = StatusFail
		c.Detail = "the file is empty: whoever receives it has nothing to present."
		c.Fix = mintFor(in.For, in.ControlPlaneURL, in.Path, false)
		return c
	case in.Values > 1:
		c.Subject = fmt.Sprintf("%s: %d values", in.Path, in.Values)
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("the file holds %d values, not one token: a receiver that presents the whole file presents none of them, "+
			"and every value in it goes to whoever receives it.", in.Values)
		c.Fix = "put the one token the receiver needs in a file of its own, then: argus doctor --token-file <that file>"
		return c
	}

	f := in.Facts
	switch f.Kind {
	case KindAuthorPAT:
		c.Subject = in.Path + ": an odts_ PAT"
		c.Status = StatusOK
		pat := "Long-lived (90 days by default). An author PAT, the default kind, drives the author__* tools; a builder token (#259: runner " +
			"scope, bound to one workspace) has the same odts_ prefix and drives runner__run instead. Only the control plane's Tokens page " +
			"shows which this is, and a hub's connection check will say \"verified\" either way."
		switch in.For {
		case SideRunner:
			c.Status = StatusWarn
			c.Detail = "you said it is for the runner side, and this is not the runner access token `cloud-login --scope runner --token-out` " +
				"writes. Unless you minted it on the Tokens page as a builder token, it is an author PAT and runner__run will refuse it. " + pat
			c.Fix = runnerFix(in.ControlPlaneURL, keepAside(in.Path, runnerTokenFile)) + " Or, if it is a builder token, confirm on the Tokens page that its scope is runner."
		case SideAuthor:
			c.Detail = "most likely an author PAT, which the author__* tools accept — unless it was minted on the Tokens page as a builder token. " + pat
		default:
			c.Detail = pat
		}
		return c
	case KindUnrecognised:
		c.Subject = in.Path + ": neither an odts_ PAT nor a JWT"
		c.Status = StatusWarn
		c.Detail = "the control plane accepts odts_ PATs and its own access tokens (JWTs); this is neither, so the receiver will most likely " +
			"get 401. A wrong file (a workspace id, a URL, a note) looks exactly like this."
		c.Fix = mintFor(in.For, in.ControlPlaneURL, in.Path, true)
		return c
	case KindJWT:
	default:
		c.Status = StatusUnknown
		c.Detail = "what the file holds could not be classified."
		c.Fix = mintFor(in.For, in.ControlPlaneURL, in.Path, true)
		return c
	}
	if f.Malformed != "" {
		c.Subject = in.Path + ": a JWT whose claims could not be read"
		c.Status = StatusUnknown
		c.Detail = "its claims could not be read: " + f.Malformed + ". Nothing can be said about its scope or expiry."
		c.Fix = mintFor(in.For, in.ControlPlaneURL, in.Path, true)
		return c
	}

	switch f.Scope {
	case SideRunner:
		c.Subject = in.Path + ": a runner-scope access token (JWT)"
		if in.For == SideAuthor {
			c.Status = StatusFail
			c.Detail = "a RUNNER-scope token, and you said it is for the author side: the author__* tools refuse it."
			c.Fix = authorFix(in.ControlPlaneURL, keepAside(in.Path, authorTokenFile))
			return c
		}
		c.Detail = "what the builder presents: runner__run accepts it; the author__* tools refuse it."
	case SideAuthor:
		c.Subject = in.Path + ": an author-scope access token (JWT)"
		if in.For == SideRunner {
			c.Status = StatusFail
			c.Detail = "an AUTHOR-scope token, and you said it is for the runner side: runner__run refuses it."
			c.Fix = runnerFix(in.ControlPlaneURL, keepAside(in.Path, runnerTokenFile))
			return c
		}
		c.Detail = fmt.Sprintf("the author__* tools accept it. It lives %s; for a hand-off, an author PAT from the Tokens page lasts.", human(accessTokenLife))
	default:
		c.Subject = in.Path + ": a JWT with no scope claim"
		c.Status = StatusWarn
		c.Detail = "it has no scope claim, so this doctor cannot say which side accepts it; the control plane's own tokens carry one."
		c.Fix = mintFor(in.For, in.ControlPlaneURL, in.Path, true)
		return c
	}

	// A bare access token: once it leaves the session store, nothing renews it. Its replacement may be
	// written over it — it is the same kind, and past (or near) its use.
	fix := runnerFix(in.ControlPlaneURL, in.Path)
	if f.Scope == SideAuthor {
		fix = authorFix(in.ControlPlaneURL, in.Path)
	}
	if !f.HasExpiry {
		c.Status = StatusOK
		c.Detail += " It has no exp claim, so this doctor cannot say when it expires."
		return c
	}
	left := f.ExpiresAt.Sub(in.Now)
	switch {
	case left <= 0:
		c.Status = StatusFail
		c.Detail += fmt.Sprintf(" It expired %s ago: the receiver will get 401.", human(-left))
		c.Fix = fix
	case left < expiryWarning:
		c.Status = StatusWarn
		c.Detail += fmt.Sprintf(" It expires in %s and nothing renews it: a hand-off that takes longer (a Vault wrap, a message, "+
			"the receiver installing it) delivers a dead token.", human(left))
		c.Fix = fix
	default:
		c.Status = StatusOK
		c.Detail += fmt.Sprintf(" It expires at %s (in %s), and nothing renews it once it leaves this machine: the receiver must "+
			"install it before then, and a Vault wrap must not outlive it.", f.ExpiresAt.UTC().Format("15:04 UTC"), human(left))
	}
	return c
}

// runnerFix mints the file the runner side needs, in a session file of its own so the operator's author
// session is not replaced — the order handoffs/2026-09-25-hub-runner-window-mixup.md settled on.
func runnerFix(cp, path string) string {
	return "ARGUS_SESSION_FILE=$HOME/.config/argus/session-runner.json argus cloud-login --control-plane " + orURL(cp) +
		" --scope runner --token-out " + path + " (a session of its own, so your author session is not replaced), then check it: " +
		"argus doctor --token-file " + path + " --token-for runner. For a runner credential that lasts, mint a builder token on the " +
		"control plane's Tokens page instead."
}

// authorFix: a hand-off PAT comes from the Tokens page, which shows the value once. `cloud-mint-token` is
// not the route: it writes its PAT into this machine's router state, not into a file to hand over.
func authorFix(cp, path string) string {
	return "on the control plane's Tokens page (" + orURL(cp) + "), create an author token, save the odts_ value it shows once to " +
		path + ", then check it: argus doctor --token-file " + path + " --token-for author"
}

// mintFor is the fix for the side the token is going to (both, when it was not said). keep says the file
// may hold something worth keeping, so the new token goes beside it rather than over it.
func mintFor(side, cp, path string, keep bool) string {
	runnerOut, authorOut := path, path
	if keep {
		runnerOut, authorOut = keepAside(path, runnerTokenFile), keepAside(path, authorTokenFile)
	}
	switch side {
	case SideRunner:
		return runnerFix(cp, runnerOut)
	case SideAuthor:
		return authorFix(cp, authorOut)
	}
	return "For the runner side: " + runnerFix(cp, runnerOut) + " For the author side: " + authorFix(cp, authorOut)
}

// File names a new token goes to when the checked file is kept (keepAside).
const (
	runnerTokenFile = "runner-token.txt"
	authorTokenFile = "author-token.txt"
)

// keepAside is where a new token goes when the checked file may hold something worth keeping: a PAT the
// Tokens page showed once, or a value this doctor does not recognise. A fix that wrote over it would
// destroy what it had just found.
func keepAside(path, name string) string {
	p := filepath.Join(filepath.Dir(path), name)
	if p == filepath.Clean(path) {
		p += ".new"
	}
	return p
}

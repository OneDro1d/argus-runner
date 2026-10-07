// AC-D24: one human `argus cloud-login` good for 30 days of later cloud-* calls and enrollments.
//
// The control plane already issues a refresh token on an approved device-code poll (endpoints.go
// grantDevice → issueTokens(..., withRefresh=true), internal/control/oauth/endpoints.go:327) — that
// part needed no server change. What was missing was entirely on this side: PollToken threw the
// refresh token away, cloud-login never wrote it anywhere, and every cloud-* command demanded a live
// access token or refused outright.
//
// This file is the client-side half: a private session store OUTSIDE any git tree
// (~/.config/argus/session.json by default, honoring $XDG_CONFIG_HOME via os.UserConfigDir, or
// $ARGUS_SESSION_FILE), and a Session that refreshes PROACTIVELY (the access token's own exp claim,
// ~60s margin) and REACTIVELY (a 401 from the control plane), exactly once per call, never looping.
//
// An earlier draft of this file stored the refresh token beside --token-out as `<token-out>.refresh`
// — a plaintext sidecar with no directory-permission story, easy to leave inside a git working tree.
// That shape is gone; SaveSession refuses outright to write anywhere under a `.git` tree.
package onboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoRefresh is wrapped into the error a Session returns when its access token needs refreshing
// (expired, or the control plane answered 401) but there is nothing to refresh FROM — either
// `cloud-login` was never run, or the token in hand came from an explicit --token / ARGUS_CP_TOKEN,
// which by construction carries no refresh material (REQUIRED #2). Callers match on this with
// errors.Is to decide whether to print the "run cloud-login" remedy.
var ErrNoRefresh = errors.New("onboard: no refresh token available")

// RefreshMargin is the AC-D24 "~60s margin": an access token whose exp claim falls within this
// window of "now" is refreshed PROACTIVELY, before it is handed to a call that would likely 401.
const RefreshMargin = 60 * time.Second

// SessionEnvOverride is the escape hatch REQUIRED #1 asks for: tests and operators who need the
// session to live somewhere other than the OS default point this at an explicit file.
const SessionEnvOverride = "ARGUS_SESSION_FILE"

// jwtExp decodes the UNVERIFIED "exp" claim (unix seconds) of a JWT. ok is false when the token is
// not a 3-part JWT or carries no exp at all — callers must not treat that as "expired"; the 401 retry
// is what actually guarantees correctness, this is only ever a courtesy that saves one round trip.
func jwtExp(tok string) (time.Time, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	blob, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(blob, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// nearExpiry reports whether tok's exp claim is within margin of now (or already past it). An
// unparsable/claimless token answers false — see jwtExp.
func nearExpiry(tok string, now time.Time, margin time.Duration) bool {
	exp, ok := jwtExp(tok)
	if !ok {
		return false
	}
	return !now.Add(margin).Before(exp)
}

// HTTPStatusError carries the HTTP status a control-plane call answered with, so Session.Do can tell
// "this credential needs refreshing" (401) from every other failure without parsing error text. The
// CloudClient methods a Session wraps return one of these on a non-2xx response; a transport failure
// (never reached the network) does NOT carry one — StatusOf answers 0 for it, same as for any error
// from outside this package.
type HTTPStatusError struct {
	Status int
	Msg    string
}

func (e *HTTPStatusError) Error() string { return e.Msg }

// StatusOf returns the HTTP status wrapped anywhere in err's chain, or 0 when none is present.
func StatusOf(err error) int {
	var se *HTTPStatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// ── the on-disk session ──────────────────────────────────────────────────────────────────────────

// SessionData is the on-disk shape of session.json (REQUIRED #1): the control plane it was obtained
// from, the live access token, the long-lived refresh token, and enough context (scope, workspace) to
// explain the session without decoding the JWT again.
type SessionData struct {
	ControlPlane string    `json:"control_plane"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scope        string    `json:"scope,omitempty"`
	Workspace    string    `json:"workspace,omitempty"`
	ObtainedAt   time.Time `json:"obtained_at,omitempty"`
}

// DefaultSessionPath resolves where cloud-login writes and every cloud-* command reads the session:
// $ARGUS_SESSION_FILE when set (tests, and operators who want it elsewhere), else
// <os.UserConfigDir()>/argus/session.json — os.UserConfigDir honors $XDG_CONFIG_HOME on Linux.
func DefaultSessionPath() (string, error) {
	if v := os.Getenv(SessionEnvOverride); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("onboard: could not determine a user config dir (set %s instead): %w", SessionEnvOverride, err)
	}
	return filepath.Join(base, "argus", "session.json"), nil
}

// refuseIfGitTree walks up from path's directory looking for a `.git` entry (a directory for an
// ordinary clone, a FILE for a worktree/submodule — Lstat catches both without following it).
// REQUIRED #1: the refresh token must never land somewhere a repo could commit it, so this is
// enforced here, at the one function every write goes through, not left to callers to remember.
func refuseIfGitTree(path string) error {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("onboard: resolve %s: %w", path, err)
	}
	cur := dir
	for {
		gitPath := filepath.Join(cur, ".git")
		if _, statErr := os.Lstat(gitPath); statErr == nil {
			return fmt.Errorf("onboard: refusing to write the session file at %s — %s is inside a git working tree (found %s); the OAuth refresh token must never be written where a repo could commit it. Point %s (or an explicit --token-out) somewhere outside any git tree", path, dir, gitPath, SessionEnvOverride)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}

// ensureDir0700 creates dir at 0700 when absent, and TIGHTENS an existing dir to 0700 when it is
// looser (REQUIRED #1: "chmod it to 0700 if it is looser").
func ensureDir0700(dir string) error {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("onboard: %s exists and is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// SaveSession writes d to path ATOMICALLY: a temp file created 0600 in the same directory, then
// renamed over path (REQUIRED #1). It refuses inside a git working tree and creates/tightens the
// containing directory to 0700 first. Never logs or returns the token values in its own error text.
func SaveSession(path string, d SessionData) error {
	if err := refuseIfGitTree(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDir0700(dir); err != nil {
		return fmt.Errorf("onboard: session dir %s: %w", dir, err)
	}
	blob, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("onboard: encode session: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("onboard: create temp session file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return fmt.Errorf("onboard: write session: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("onboard: chmod session temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("onboard: close session temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("onboard: rename session file into place: %w", err)
	}
	ok = true
	return nil
}

// SaveTokenOutFile writes token as PLAIN TEXT to path, atomically, mode 0600 — the back-compat
// --token-out format (the access token alone, no JSON, no refresh token). It refuses inside a git
// working tree, same as SaveSession, but does NOT touch the containing directory's permissions:
// --token-out paths land in directories other tooling already owns (e.g. onboard.sh's
// deploy/compose/), and forcing them to 0700 would be a surprising side effect this call has no
// business causing — unlike the session store's OWN directory, which nothing else uses.
func SaveTokenOutFile(path, token string) error {
	if err := refuseIfGitTree(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("onboard: create temp token file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		return fmt.Errorf("onboard: write token: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("onboard: chmod token temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("onboard: close token temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("onboard: rename token file into place: %w", err)
	}
	ok = true
	return nil
}

// LoadSessionFile reads and parses path. Returns the raw *os.PathError (os.IsNotExist-able) on a
// missing file so callers can tell "never logged in" from "the file is corrupt".
func LoadSessionFile(path string) (*SessionData, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d SessionData
	if err := json.Unmarshal(blob, &d); err != nil {
		return nil, fmt.Errorf("onboard: parse session file %s: %w", path, err)
	}
	return &d, nil
}

// DeleteSession removes path. existed=false with a nil error is the idempotent "nothing to log out
// of" case cloud-logout relies on (REQUIRED #3).
func DeleteSession(path string) (existed bool, err error) {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// PersistLogin builds SessionData from a fresh Login() result and saves it to path (DefaultSessionPath
// when path=="") — the write cloud-login performs after a successful device-code approval.
func PersistLogin(path, controlPlane, scope, access, refresh string) (string, error) {
	if path == "" {
		p, err := DefaultSessionPath()
		if err != nil {
			return "", err
		}
		path = p
	}
	d := SessionData{
		ControlPlane: controlPlane,
		AccessToken:  access,
		RefreshToken: refresh,
		Scope:        scope,
		Workspace:    WorkspaceFromToken(access),
		ObtainedAt:   time.Now().UTC(),
	}
	if err := SaveSession(path, d); err != nil {
		return "", err
	}
	return path, nil
}

// ── the in-memory session a command runs its calls through ─────────────────────────────────────────

// Session is one cloud-* command's authenticated session: the access token in hand, where it (and its
// refresh sibling) live on disk, and the client to refresh through. main.go builds exactly one per
// invocation; every authenticating cloud-* call is expected to run through Session.Do.
type Session struct {
	Client *CloudClient
	// Path is "" for a STATIC session (an explicit --token / ARGUS_CP_AUTHOR_TOKEN, formerly
	// ARGUS_CP_TOKEN — REQUIRED #2): no file, no refresh capability, no persistence. Non-"" is what
	// makes refreshing able to rewrite the session file in place.
	Path string

	data       SessionData
	refreshedN int // how many refreshes THIS Session performed — read by RefreshCount for evidence/tests.
}

// NewEmptySession builds a Session with no credential at all — the intentionally-unauthenticated case
// some cloud-* commands legitimately allow (an anonymous availability check, a workspace list before
// login). It never attempts to refresh; Do simply calls fn with an empty token.
func NewEmptySession(client *CloudClient) *Session { return &Session{Client: client} }

// NewStaticSession wraps an explicit --token / ARGUS_CP_AUTHOR_TOKEN (formerly ARGUS_CP_TOKEN,
// REQUIRED #2): it takes precedence over any session file, works exactly as before, and carries no
// refresh capability — refreshOnce's error says so by name.
func NewStaticSession(client *CloudClient, token string) *Session {
	return &Session{Client: client, data: SessionData{AccessToken: token}}
}

// LoadSession reads the session file at path (DefaultSessionPath() when path=="") and returns a
// refresh-capable Session. A missing or empty file is a clear "run cloud-login" error, not a panic or
// a silent empty session.
func LoadSession(client *CloudClient, path string) (*Session, error) {
	if path == "" {
		p, err := DefaultSessionPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	d, err := LoadSessionFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("onboard: no session at %s — run `argus cloud-login` first (or pass --token / set ARGUS_CP_AUTHOR_TOKEN, formerly ARGUS_CP_TOKEN)", path)
		}
		return nil, fmt.Errorf("onboard: read session %s: %w — run `argus cloud-login` again", path, err)
	}
	if strings.TrimSpace(d.AccessToken) == "" {
		return nil, fmt.Errorf("onboard: session at %s has no access token — run `argus cloud-login`", path)
	}
	return &Session{Client: client, Path: path, data: *d}, nil
}

// Token returns the current access token (post any refresh already performed this session).
func (s *Session) Token() string { return s.data.AccessToken }

// RefreshCount reports how many refresh_token grants this Session has performed — used by tests/
// evidence to prove "exactly one refresh".
func (s *Session) RefreshCount() int { return s.refreshedN }

// EnsureFresh runs the PROACTIVE refresh check on demand, for a caller that needs a valid Token()
// in hand BEFORE it can build the rest of its request (cloud-mint-token derives the record owner
// from the token's `sub` claim ahead of the mint call itself). Most callers should prefer Do, which
// also covers the REACTIVE 401 case; EnsureFresh alone does not retry on a 401.
func (s *Session) EnsureFresh(ctx context.Context) error { return s.ensureFresh(ctx, time.Now()) }

// ensureFresh is the PROACTIVE half of AC-D24: refresh when the access token is within RefreshMargin
// of its exp claim. A Session with no credential at all (NewEmptySession) skips this — not its job.
func (s *Session) ensureFresh(ctx context.Context, now time.Time) error {
	if s.data.AccessToken == "" && s.data.RefreshToken == "" {
		return nil
	}
	if s.data.AccessToken != "" && !nearExpiry(s.data.AccessToken, now, RefreshMargin) {
		return nil
	}
	return s.refreshOnce(ctx)
}

// refreshOnce performs exactly ONE refresh_token grant. On success it persists the rotated pair
// (atomic, 0600) via SaveSession — including the refresh token itself when the control plane rotated
// it (grantRefresh does not today, "no rotation in M3 — simplest" per endpoints.go:314, but a Session
// that assumed it never will is exactly the kind of assumption that breaks silently the day it does).
// On failure it returns the failure untouched — the caller (Do, or a command's own refusal path)
// decides what "clear message, no second attempt" means; this function itself never retries and never
// prints a token value.
func (s *Session) refreshOnce(ctx context.Context) error {
	if s.data.RefreshToken == "" {
		if s.Path == "" {
			return fmt.Errorf("%w: this token came from --token or ARGUS_CP_AUTHOR_TOKEN (formerly ARGUS_CP_TOKEN), which carries no refresh material — run `argus cloud-login` (and drop --token/ARGUS_CP_AUTHOR_TOKEN) to enable auto-refresh", ErrNoRefresh)
		}
		return fmt.Errorf("%w — run `argus cloud-login`", ErrNoRefresh)
	}
	rt, err := s.Client.Refresh(ctx, s.data.RefreshToken)
	if err != nil {
		return fmt.Errorf("refresh failed: %w — run `argus cloud-login`", err)
	}
	s.data.AccessToken = rt.AccessToken
	if rt.RefreshToken != "" {
		s.data.RefreshToken = rt.RefreshToken
	}
	s.refreshedN++
	if s.Path != "" {
		s.data.ObtainedAt = time.Now().UTC()
		if werr := SaveSession(s.Path, s.data); werr != nil {
			return fmt.Errorf("refresh: persist rotated session: %w", werr)
		}
	}
	return nil
}

// Do runs fn with the session's current access token, refreshing PROACTIVELY first (ensureFresh) and
// REACTIVELY — exactly once, never looping — when fn's error carries HTTP 401 (StatusOf). fn reports
// its own outcome's status via a *HTTPStatusError on failure; a nil error, or an error with no status
// (a transport failure, or any other class of error), is returned to the caller untouched.
//
// AT MOST ONE refresh happens per Do call: if ensureFresh already refreshed (the proactive path) and
// fn STILL answers 401, that is treated as the refreshed credential being refused too, and Do returns
// the error rather than refreshing a second time — REQUIRED #2's "never loop".
func (s *Session) Do(ctx context.Context, fn func(token string) error) error {
	before := s.refreshedN
	if err := s.ensureFresh(ctx, time.Now()); err != nil {
		return err
	}
	err := fn(s.data.AccessToken)
	if StatusOf(err) != http.StatusUnauthorized {
		return err
	}
	if s.refreshedN > before {
		return err // already refreshed once for this call; do not refresh again
	}
	if s.data.AccessToken == "" && s.data.RefreshToken == "" {
		// An empty session (NewEmptySession) went out anonymous and has nothing to refresh. Say that no
		// credential was presented, keeping the 401 for callers that branch on it. refreshOnce would say
		// "this token came from --token", naming a token nobody set (the 2026-09-28 fresh-user trial).
		return fmt.Errorf("%w — no credential was presented (no --token, no ARGUS_CP_AUTHOR_TOKEN, no session file): run `argus cloud-login`", err)
	}
	if s.data.RefreshToken == "" {
		return s.refusedWithNothingToRefresh(err)
	}
	if rerr := s.refreshOnce(ctx); rerr != nil {
		return rerr
	}
	return fn(s.data.AccessToken)
}

// refusedWithNothingToRefresh is Do's answer when the control plane answered 401 and there is no refresh
// token to try: the control plane REFUSED the credential. That is the fact, and it leads; "nothing to
// refresh with" is only why there is no retry. The 401 (StatusOf) and ErrNoRefresh both stay in the chain.
//
// ⛔ REPLAY 2026-09-29 live doctor test, R4: a mistyped PAT in ARGUS_CP_AUTHOR_TOKEN, refused with 401, used to
// come back from refreshOnce as "no refresh token available: … carries no refresh material — run argus
// cloud-login … to enable auto-refresh", with the 401 dropped. A PAT never needs refreshing, so that sent the
// reader after an expiry that was not the problem. refreshOnce keeps that text for the PROACTIVE path
// (ensureFresh: a JWT about to expire), where it is true.
func (s *Session) refusedWithNothingToRefresh(err error) error {
	cp := "<control-plane-url>"
	if s.Client != nil {
		cp = ControlPlaneForCommand(s.Client.BaseURL)
	}
	if s.Path == "" {
		// The cure is a sequence: while either name is exported it outranks the session file (sessionForCommand,
		// envname.Lookup), so a new login is ignored and the refused token is presented again. A PAT is NOT minted
		// with `argus cloud-mint-token`: that command mints this machine's author token (minted during onboarding), needs --router-state
		// (without it the control plane answers 400 "router_id is required for an auto token"), and the onboarding
		// scripts send a reader to the control plane's API Tokens page for a PAT.
		return fmt.Errorf("the control plane refused this token (%w): it was revoked, has expired, was mistyped or cut short, "+
			"or was issued by another control plane. It came from --token or ARGUS_CP_AUTHOR_TOKEN (formerly ARGUS_CP_TOKEN), "+
			"which outranks the session file and cannot be refreshed (%w). Replace it: drop --token, "+
			"`unset ARGUS_CP_AUTHOR_TOKEN ARGUS_CP_TOKEN`, then `argus cloud-login --control-plane %s --scope author`. "+
			"After that login, if you need a PAT again, generate an author token on the control plane's API Tokens page (%s)",
			err, ErrNoRefresh, cp, cp)
	}
	return fmt.Errorf("the control plane refused this session's access token (%w), and the session file holds no refresh "+
		"token to renew it (%w) — sign in again: `argus cloud-login --control-plane %s --scope author`", err, ErrNoRefresh, cp)
}

// ControlPlaneForCommand renders a control-plane URL for a command printed in an error, which ends up in stdout
// JSON: scheme, host and path only, since a URL can carry a credential (user:password@, ?token=), and
// single-quoted when the shell would otherwise split or glob it. A URL that does not parse as scheme://host is
// not repeated at all (scheme AND host are both required); the placeholder takes its place. This is the ONE
// implementation: cmd/argus's `diagnose` pointer (#357) calls it too, so the `error` and `diagnose` lines of one
// refusal print the URL identically for every input.
func ControlPlaneForCommand(raw string) string {
	out, ok := controlPlaneWithoutCredentials(raw)
	if !ok {
		return controlPlanePlaceholder
	}
	if strings.Trim(out, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") != "" {
		return "'" + strings.ReplaceAll(out, "'", `'\''`) + "'"
	}
	return out
}

// ControlPlaneForDisplay renders a control-plane URL for a message that is READ, not pasted (the credential
// banner, a check's subject): the same cleaning as ControlPlaneForCommand and the same placeholder, but never
// shell-quoted, since quote characters around it would look like part of the URL.
func ControlPlaneForDisplay(raw string) string {
	out, ok := controlPlaneWithoutCredentials(raw)
	if !ok {
		return controlPlanePlaceholder
	}
	return out
}

const controlPlanePlaceholder = "<control-plane-url>"

// controlPlaneWithoutCredentials is the shared core of the two renderers: scheme, host and path only. ok is
// false when raw does not parse as scheme://host; the raw text is then not repeated at all, since it may hold
// a secret.
func controlPlaneWithoutCredentials(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	shown := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	return shown.String(), true
}

// ── logout ────────────────────────────────────────────────────────────────────────────────────────

// Logout implements `argus cloud-logout` (REQUIRED #3): best-effort revoke the refresh token at the
// CP (POST /oauth/revoke, RFC 7009 semantics — internal/control/oauth/endpoints.go handleRevoke), then
// always delete the session file. Idempotent: no session file at all is success (hadSession=false),
// not an error — a second `cloud-logout` must exit 0.
//
// The file is removed even when the CP revoke could not be confirmed (client==nil, or the call
// failed): a local logout must not get stuck behind a control plane that is briefly unreachable. In
// that case revoked=false and err carries the revoke failure so the caller can still report it.
func Logout(ctx context.Context, client *CloudClient, path string) (hadSession, revoked bool, err error) {
	if path == "" {
		p, perr := DefaultSessionPath()
		if perr != nil {
			return false, false, perr
		}
		path = p
	}
	d, rerr := LoadSessionFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("onboard: read session %s: %w", path, rerr)
	}
	var revokeErr error
	if d.RefreshToken != "" && client != nil {
		if revokeErr = client.RevokeRefresh(ctx, d.RefreshToken); revokeErr == nil {
			revoked = true
		}
	}
	if _, derr := DeleteSession(path); derr != nil {
		if revokeErr != nil {
			return true, false, fmt.Errorf("revoke: %v (and could not delete %s: %w)", revokeErr, path, derr)
		}
		return true, false, fmt.Errorf("onboard: delete session %s: %w", path, derr)
	}
	if revokeErr != nil {
		return true, false, fmt.Errorf("%w — the session file was removed locally, but the refresh token was NOT revoked at the control plane and stays valid until it expires (at most 30 days)", revokeErr)
	}
	return true, revoked, nil
}

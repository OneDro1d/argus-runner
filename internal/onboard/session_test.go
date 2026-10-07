package onboard

// AC-D24 session store + auto-refresh + logout tests (RED-GREEN, see the mutant test at the bottom).
//
// The refusal test, the perms test and the refresh-rotation tests are the direct evidence for
// REQUIRED #1/#2/#3 in the promise: the session file never lands in a git tree, is 0600 in a 0700
// dir, refreshes proactively before expiry AND reactively on one 401, never loops, and cloud-logout
// revokes + deletes idempotently.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJWT builds an unsigned 3-part JWT carrying only an exp claim — jwtExp/nearExpiry read it
// without verifying a signature, exactly like the real Session does against a real CP-signed token.
func fakeJWT(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + itoa(exp.Unix()) + `,"sub":"u1","workspace":"ws1"}`))
	return header + "." + payload + ".sig"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// refreshCP is a minimal fake control plane: POST /oauth/token (grant_type=refresh_token) rotates the
// refresh token by default, POST /oauth/revoke records the revoked token. It refuses a refresh_token
// that does not match its CURRENT expectation, which is what proves "the rotated refresh token is
// persisted: the next refresh uses it, not the old one."
type refreshCP struct {
	*httptest.Server
	mu             sync.Mutex
	currentRefresh string
	refreshCalls   int32
	revokedTokens  []string
	// refuseRefresh, when true, makes every refresh_token grant answer 400 invalid_grant — the
	// "refresh failure" test.
	refuseRefresh bool
	// noRotate, when true, answers a refresh with an access token ONLY and keeps the same refresh
	// token valid — what the real control plane does today (grantRefresh, withRefresh=false).
	noRotate bool
	// newAccessTTL controls the exp claim of every access token this mints.
	newAccessTTL time.Duration
}

func newRefreshCP(t *testing.T, initialRefresh string, ttl time.Duration) *refreshCP {
	t.Helper()
	f := &refreshCP{currentRefresh: initialRefresh, newAccessTTL: ttl}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.refreshCalls, 1)
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.refuseRefresh {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "refresh token invalid, expired, or revoked"})
			return
		}
		presented := r.Form.Get("refresh_token")
		if presented != f.currentRefresh {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "stale refresh token presented"})
			return
		}
		newAccess := fakeJWT(time.Now().Add(f.newAccessTTL))
		if f.noRotate {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": newAccess, "token_type": "Bearer", "expires_in": int(f.newAccessTTL.Seconds()),
			})
			return
		}
		newRefresh := "rt_" + itoa(time.Now().UnixNano())
		f.currentRefresh = newRefresh
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": newAccess, "refresh_token": newRefresh, "token_type": "Bearer",
			"expires_in": int(f.newAccessTTL.Seconds()),
		})
	})
	mux.HandleFunc("/oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revokedTokens = append(f.revokedTokens, r.Form.Get("token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *refreshCP) calls() int {
	return int(atomic.LoadInt32(&f.refreshCalls))
}

func (f *refreshCP) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.currentRefresh
}

// ── SaveSession: modes + dir tightening ─────────────────────────────────────────────────────────

func TestSaveSession_ModeAndDirTightening(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "argus")
	// pre-create the dir LOOSE (0755) — cloud-login must tighten it to 0700, not merely create it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// MkdirAll's mode is filtered by the umask (a 077 umask would already yield 0700 and hide a missing
	// tightening), so set the loose mode explicitly.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.json")
	if err := SaveSession(path, SessionData{ControlPlane: "https://cp.example", AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat session file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat session dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("session dir mode = %o, want 0700 (a pre-existing 0755 dir must be TIGHTENED)", di.Mode().Perm())
	}
}

// ── the git-tree refusal ─────────────────────────────────────────────────────────────────────────

func TestSaveSession_RefusesInsideGitWorkingTree(t *testing.T) {
	base := t.TempDir()
	cmd := exec.Command("git", "init", "-q", base)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git not available to set up the fixture: %v: %s", err, out)
	}
	path := filepath.Join(base, "argus", "session.json")
	err := SaveSession(path, SessionData{AccessToken: "a", RefreshToken: "r"})
	if err == nil {
		t.Fatal("SaveSession succeeded inside a git working tree — REQUIRED #1's refusal did not fire")
	}
	if !strings.Contains(err.Error(), "git working tree") {
		t.Errorf("error = %q, want it to name the git working tree", err.Error())
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("the session file was written anyway despite the refusal")
	}
}

func TestSaveSession_OutsideGitTreeSucceeds(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "argus", "session.json")
	if err := SaveSession(path, SessionData{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("SaveSession outside a git tree should succeed: %v", err)
	}
}

// ── proactive refresh (before expiry), and the rotated token is what the NEXT refresh uses ────────

func TestSession_ProactiveRefreshBeforeExpiry_PersistsRotatedToken(t *testing.T) {
	cp := newRefreshCP(t, "rt_initial", time.Hour)
	client := NewCloudClient(cp.URL)
	sessPath := filepath.Join(t.TempDir(), "session.json")
	// An access token INSIDE the 60s margin — must be refreshed BEFORE the call, not after a 401.
	seed := SessionData{ControlPlane: cp.URL, AccessToken: fakeJWT(time.Now().Add(30 * time.Second)), RefreshToken: "rt_initial"}
	if err := SaveSession(sessPath, seed); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	sess, err := LoadSession(client, sessPath)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	calledWith := ""
	err = sess.Do(context.Background(), func(token string) error {
		calledWith = token
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sess.RefreshCount() != 1 {
		t.Fatalf("RefreshCount = %d, want exactly 1 (a token inside the margin gets exactly one refresh)", sess.RefreshCount())
	}
	if cp.calls() != 1 {
		t.Fatalf("the control plane saw %d refresh calls, want 1", cp.calls())
	}
	if calledWith != sess.Token() || calledWith == seed.AccessToken {
		t.Fatalf("fn was not handed the FRESH token")
	}
	onDisk, err := LoadSessionFile(sessPath)
	if err != nil {
		t.Fatalf("read persisted session: %v", err)
	}
	if onDisk.AccessToken != sess.Token() {
		t.Errorf("the rotated access token was not persisted")
	}
	if onDisk.RefreshToken == "rt_initial" || onDisk.RefreshToken != cp.current() {
		t.Fatalf("the ROTATED refresh token was not persisted: on disk %q, CP's current %q", onDisk.RefreshToken, cp.current())
	}

	// The NEXT refresh must present the ROTATED token, not "rt_initial" — reload from disk exactly
	// as a fresh `argus cloud-*` invocation would, force it near expiry again, and refresh once more.
	sess2, err := LoadSession(client, sessPath)
	if err != nil {
		t.Fatalf("LoadSession (2nd): %v", err)
	}
	sess2.data.AccessToken = fakeJWT(time.Now().Add(10 * time.Second)) // simulate time passing
	err = sess2.Do(context.Background(), func(token string) error { return nil })
	if err != nil {
		t.Fatalf("Do (2nd): %v — the rotated refresh token from the first round was not what got presented", err)
	}
	if cp.calls() != 2 {
		t.Fatalf("the control plane saw %d total refresh calls, want 2", cp.calls())
	}
}

// ── the real control plane does NOT rotate: the old refresh token must survive every refresh ───────

func TestSession_NonRotatingCP_KeepsRefreshTokenAcrossRefreshes(t *testing.T) {
	cp := newRefreshCP(t, "rt_keep", time.Hour)
	cp.noRotate = true
	client := NewCloudClient(cp.URL)
	sessPath := filepath.Join(t.TempDir(), "session.json")
	seed := SessionData{ControlPlane: cp.URL, AccessToken: fakeJWT(time.Now().Add(30 * time.Second)), RefreshToken: "rt_keep"}
	if err := SaveSession(sessPath, seed); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	for round := 1; round <= 2; round++ {
		sess, err := LoadSession(client, sessPath)
		if err != nil {
			t.Fatalf("round %d LoadSession: %v", round, err)
		}
		sess.data.AccessToken = fakeJWT(time.Now().Add(10 * time.Second)) // near expiry again
		if err := sess.Do(context.Background(), func(string) error { return nil }); err != nil {
			t.Fatalf("round %d Do: %v", round, err)
		}
		onDisk, err := LoadSessionFile(sessPath)
		if err != nil {
			t.Fatalf("round %d read session: %v", round, err)
		}
		if onDisk.RefreshToken != "rt_keep" {
			t.Fatalf("round %d: refresh token on disk = %q, want rt_keep kept (the CP sent none back; dropping it ends the 30-day session after one refresh)", round, onDisk.RefreshToken)
		}
	}
	if cp.calls() != 2 {
		t.Fatalf("the control plane saw %d refresh calls, want 2", cp.calls())
	}
}

// ── a proactive refresh followed by a 401 is an error: never a second refresh in one call ─────────

func TestSession_ProactiveRefreshThen401_NoSecondRefresh(t *testing.T) {
	cp := newRefreshCP(t, "rt_1", time.Hour)
	client := NewCloudClient(cp.URL)
	sess := &Session{Client: client, data: SessionData{
		AccessToken: fakeJWT(time.Now().Add(10 * time.Second)), RefreshToken: "rt_1", // inside the margin
	}}
	calls := 0
	err := sess.Do(context.Background(), func(string) error {
		calls++
		return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "401"}
	})
	if err == nil {
		t.Fatal("expected an error when the freshly refreshed credential is refused")
	}
	if calls != 1 {
		t.Fatalf("fn was called %d times, want 1 (the proactive refresh already used this call's one refresh)", calls)
	}
	if cp.calls() != 1 {
		t.Fatalf("the control plane saw %d refresh calls, want exactly 1", cp.calls())
	}
}

// ── refresh failure: clear error naming cloud-login, exactly one attempt, no token text ───────────

func TestSession_RefreshFailure_ClearErrorNoLoop(t *testing.T) {
	cp := newRefreshCP(t, "rt_secret_value", time.Hour)
	cp.refuseRefresh = true
	client := NewCloudClient(cp.URL)
	sess := &Session{Client: client, data: SessionData{
		AccessToken:  fakeJWT(time.Now().Add(-time.Minute)), // already expired
		RefreshToken: "rt_secret_value",
	}}
	err := sess.Do(context.Background(), func(token string) error {
		t.Fatal("fn must not be called when the proactive refresh itself fails")
		return nil
	})
	if err == nil {
		t.Fatal("expected an error when the CP refuses the refresh")
	}
	if !strings.Contains(err.Error(), "cloud-login") {
		t.Errorf("error = %q, want it to name `argus cloud-login`", err.Error())
	}
	if strings.Contains(err.Error(), "rt_secret_value") {
		t.Errorf("error leaked the refresh token value: %q", err.Error())
	}
	if cp.calls() != 1 {
		t.Fatalf("the control plane saw %d refresh attempts, want exactly 1 (no loop)", cp.calls())
	}
}

// ── reactive: a 401 gives exactly one refresh + one retry; a second 401 is an error, not a loop ───

func TestSession_401_ExactlyOneRefreshThenRetry(t *testing.T) {
	cp := newRefreshCP(t, "rt_1", time.Hour)
	client := NewCloudClient(cp.URL)
	// A token that is NOT near expiry, so the PROACTIVE path does nothing — only the reactive 401
	// path should trigger the refresh here.
	sess := &Session{Client: client, data: SessionData{
		AccessToken: fakeJWT(time.Now().Add(time.Hour)), RefreshToken: "rt_1",
	}}
	calls := 0
	err := sess.Do(context.Background(), func(token string) error {
		calls++
		if calls == 1 {
			return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "401"}
		}
		return nil // the retry, with the refreshed token, succeeds
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 2 {
		t.Fatalf("fn was called %d times, want 2 (the 401, then one retry)", calls)
	}
	if sess.RefreshCount() != 1 {
		t.Fatalf("RefreshCount = %d, want exactly 1", sess.RefreshCount())
	}
	if cp.calls() != 1 {
		t.Fatalf("the control plane saw %d refresh calls, want 1", cp.calls())
	}
}

func TestSession_SecondConsecutive401_IsAnErrorNotALoop(t *testing.T) {
	cp := newRefreshCP(t, "rt_1", time.Hour)
	client := NewCloudClient(cp.URL)
	sess := &Session{Client: client, data: SessionData{
		AccessToken: fakeJWT(time.Now().Add(time.Hour)), RefreshToken: "rt_1",
	}}
	calls := 0
	err := sess.Do(context.Background(), func(token string) error {
		calls++
		return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "401"} // ALWAYS 401, even refreshed
	})
	if err == nil {
		t.Fatal("expected an error when the refreshed credential is STILL refused")
	}
	if calls != 2 {
		t.Fatalf("fn was called %d times, want 2 (the original 401 + exactly one retry, never more)", calls)
	}
	if sess.RefreshCount() != 1 {
		t.Fatalf("RefreshCount = %d, want exactly 1 (never a second refresh for the same call)", sess.RefreshCount())
	}
	if cp.calls() != 1 {
		t.Fatalf("the control plane saw %d refresh calls, want exactly 1 (no loop)", cp.calls())
	}
}

// ── static session (--token / ARGUS_CP_TOKEN): no refresh capability, clear error ─────────────────

func TestNewStaticSession_NeverRefreshesNamesTheFlags(t *testing.T) {
	cp := newRefreshCP(t, "", time.Hour)
	client := NewCloudClient(cp.URL)
	sess := NewStaticSession(client, fakeJWT(time.Now().Add(-time.Minute))) // expired, no refresh token
	err := sess.Do(context.Background(), func(token string) error { return nil })
	if err == nil {
		t.Fatal("a static session with an expired token and no refresh material must fail, not silently proceed")
	}
	if !strings.Contains(err.Error(), "--token") || !strings.Contains(err.Error(), "ARGUS_CP_TOKEN") {
		t.Errorf("error = %q, want it to name --token/ARGUS_CP_TOKEN", err.Error())
	}
	if strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %q: this path sent no request, so nothing was refused — the refusal wording belongs to Do's 401 branch only", err.Error())
	}
	if cp.calls() != 0 {
		t.Errorf("a static session must never call the refresh endpoint, saw %d calls", cp.calls())
	}
}

// ── a token the control plane REFUSED: the 401 is the fact, "no refresh material" only why there is no retry ──

// REPLAY 2026-09-29 live doctor test, R4: a synthetic odts_ value in ARGUS_CP_AUTHOR_TOKEN, and argus-dev answered
// 401 invalid credential. Every cloud-* command then said "no refresh token available: this token came from --token
// or ARGUS_CP_AUTHOR_TOKEN, which carries no refresh material — run argus cloud-login … to enable auto-refresh".
// Nothing in that says the control plane refused the token, and a PAT never needed refreshing: the symptom was
// reported in place of the cause, and the remedy was for a different problem.
func TestNewStaticSession_401_SaysTheControlPlaneRefusedIt(t *testing.T) {
	cp := newRefreshCP(t, "", time.Hour)
	const pat = "odts_synthetic_refused_value"
	sess := NewStaticSession(NewCloudClient(cp.URL), pat)
	calls := 0
	err := sess.Do(context.Background(), func(token string) error {
		calls++
		return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "instances lookup failed: status 401"}
	})
	if err == nil {
		t.Fatal("a 401 on a static token must be an error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "the control plane refused this token") {
		t.Errorf("error = %q, want it to LEAD with the refusal, not with the missing refresh material", msg)
	}
	for _, want := range []string{
		"ARGUS_CP_AUTHOR_TOKEN",
		"argus cloud-login --control-plane " + cp.URL + " --scope author",
		"API Tokens page (" + cp.URL + ")",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "to enable auto-refresh") {
		t.Errorf("error = %q still offers auto-refresh as the remedy; the token was refused, not expired", msg)
	}
	// The cure is a SEQUENCE. sessionForCommand presents ARGUS_CP_AUTHOR_TOKEN (then ARGUS_CP_TOKEN, envname.Lookup)
	// ahead of the session file, and cloud-mint-token goes through it too: while either is still exported, a fresh
	// login is ignored and the refused token is presented again. So: unset both, then log in.
	if !strings.Contains(msg, "unset ARGUS_CP_AUTHOR_TOKEN ARGUS_CP_TOKEN") {
		t.Errorf("error = %q, want the literal unset of both names; either one left exported outranks a new login", msg)
	}
	// Printed without --router-state, `argus cloud-mint-token` sends an empty router id (HTTP 400 "router_id is
	// required for an auto token") and mints the machine's onboarding token, not a general PAT: never offer it here.
	if strings.Contains(msg, "cloud-mint-token") {
		t.Errorf("error = %q offers `argus cloud-mint-token`, which cannot mint a PAT; a PAT comes from the API Tokens page", msg)
	}
	if strings.Contains(msg, pat) {
		t.Errorf("error = %q contains the token value", msg)
	}
	if StatusOf(err) != http.StatusUnauthorized {
		t.Errorf("StatusOf(err) = %d, want 401 kept in the chain for callers that branch on it", StatusOf(err))
	}
	if !errors.Is(err, ErrNoRefresh) {
		t.Errorf("errors.Is(err, ErrNoRefresh) = false, want it kept for callers that branch on it")
	}
	if calls != 1 || cp.calls() != 0 {
		t.Errorf("fn called %d times, refresh endpoint %d times; want 1 and 0 (nothing to refresh, so no retry)", calls, cp.calls())
	}
}

// Only a 401 is "the control plane refused this token". A 403 (not your workspace) or a 500 on a session with nothing
// to refresh is returned UNCHANGED: the same error value, no refusal wording, no ErrNoRefresh in its chain.
func TestSession_Non401OnStaticToken_ReturnedUnchanged(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cp := newRefreshCP(t, "", time.Hour)
			sess := NewStaticSession(NewCloudClient(cp.URL), fakeJWT(time.Now().Add(time.Hour)))
			want := &HTTPStatusError{Status: status, Msg: "workspaces lookup failed"}
			calls := 0
			got := sess.Do(context.Background(), func(string) error {
				calls++
				return want
			})
			if got != error(want) {
				t.Fatalf("Do returned %v (%T), want the SAME error value back untouched", got, got)
			}
			if strings.Contains(got.Error(), "refused this token") {
				t.Errorf("error = %q claims the token was refused; a %d is not that verdict", got.Error(), status)
			}
			if errors.Is(got, ErrNoRefresh) {
				t.Errorf("errors.Is(err, ErrNoRefresh) = true for a %d; the credential was not the problem", status)
			}
			if calls != 1 || cp.calls() != 0 {
				t.Errorf("fn called %d times, refresh endpoint %d times; want 1 and 0", calls, cp.calls())
			}
		})
	}
}

// A session that DOES hold a refresh token and gets a 401 still refreshes exactly once and retries with the new token.
func TestSession_WithRefreshToken_401_RefreshesOnceAndRetries(t *testing.T) {
	cp := newRefreshCP(t, "rt_1", time.Hour)
	sess := &Session{Client: NewCloudClient(cp.URL), data: SessionData{
		AccessToken: fakeJWT(time.Now().Add(2 * time.Hour)), RefreshToken: "rt_1", // the refreshed one expires in 1h: a different value
	}}
	var tokens []string
	err := sess.Do(context.Background(), func(token string) error {
		tokens = append(tokens, token)
		if len(tokens) == 1 {
			return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "401"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("fn was called %d times, want 2 calls with a DIFFERENT token on the retry", len(tokens))
	}
	if sess.RefreshCount() != 1 || cp.calls() != 1 {
		t.Errorf("RefreshCount = %d, refresh endpoint calls = %d; want exactly 1 and 1", sess.RefreshCount(), cp.calls())
	}
}

// ControlPlaneForCommand is the ONE renderer of a control-plane URL for a command printed in a refusal (the `error`
// text here and cmd/argus's `diagnose` pointer both call it). FAKE credentials only.
func TestControlPlaneForCommand_Table(t *testing.T) {
	const ph = "<control-plane-url>"
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ph},
		{"plain https", "https://cp.example", "https://cp.example"},
		{"with a port", "https://cp.example:8443", "https://cp.example:8443"},
		{"with a path", "https://cp.example/base", "https://cp.example/base"},
		{"userinfo dropped", "https://fakeuser:fakepw-123@cp.example", "https://cp.example"},
		{"query dropped", "https://cp.example?token=fake-tok-456", "https://cp.example"},
		{"fragment dropped", "https://cp.example/#fake-frag-789", "https://cp.example/"},
		{"all three dropped", "https://fakeuser:fakepw-123@cp.example:8443/base?token=fake-tok-456#fake-frag-789", "https://cp.example:8443/base"},
		{"schemeless //host/base", "//host.example/base", ph},
		{"bare hostname", "cp.example", ph},
		{"scheme without host", "https:///base", ph},
		{"opaque scheme", "mailto:someone@cp.example", ph},
		{"space in the path is escaped, so safe", "https://cp.example/a b", "https://cp.example/a%20b"},
		{"space in the host does not parse", "https://cp .example", ph},
		{"shell metacharacters are quoted", "https://cp.example/a b;$(touch x)", "'https://cp.example/a%20b;$%28touch%20x%29'"},
		{"single quote is quoted", "https://cp.example/it's", `'https://cp.example/it'\''s'`},
		{"unparseable", "http://fakeuser:fakepw-123@[::1", ph},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ControlPlaneForCommand(tc.in)
			if got != tc.want {
				t.Fatalf("ControlPlaneForCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, secret := range []string{"fakeuser", "fakepw", "fake-tok", "fake-frag"} {
				if strings.Contains(got, secret) {
					t.Fatalf("ControlPlaneForCommand(%q) echoes %q: %q", tc.in, secret, got)
				}
			}
		})
	}
}

// The cure is a SEQUENCE and the PAT advice is CONDITIONAL on it: a reworded message must not offer the PAT as an
// alternative to the login. So the pieces appear in this order — unset, login, then the PAT advice — and the PAT
// advice says it applies "after that login". No single punctuation literal is pinned.
func TestNewStaticSession_401_CureOrderAndConditionality(t *testing.T) {
	cp := newRefreshCP(t, "", time.Hour)
	sess := NewStaticSession(NewCloudClient(cp.URL), "odts_synthetic_refused_value")
	err := sess.Do(context.Background(), func(string) error {
		return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "401"}
	})
	if err == nil {
		t.Fatal("a 401 on a static token must be an error")
	}
	msg := err.Error()
	unset := strings.Index(msg, "unset ARGUS_CP_AUTHOR_TOKEN ARGUS_CP_TOKEN")
	login := strings.Index(msg, "argus cloud-login")
	if unset < 0 || login < 0 {
		t.Fatalf("error = %q, want both the unset step and the login step", msg)
	}
	if unset > login {
		t.Errorf("error = %q, want the unset BEFORE the login: an exported name outranks the new session", msg)
	}
	pat := strings.Index(msg, "API Tokens page")
	if pat < 0 {
		t.Fatalf("error = %q, want the PAT advice to name the API Tokens page", msg)
	}
	if pat < login {
		t.Errorf("error = %q, want the PAT advice AFTER the login, not offered ahead of or instead of it", msg)
	}
	tied := strings.Index(strings.ToLower(msg), "after that login")
	if tied < login || tied > pat {
		t.Errorf("error = %q, want the PAT advice tied to \"after that login\" (between the login step and the page)", msg)
	}
	if strings.Count(msg, "API Tokens page") != 1 {
		t.Errorf("error = %q, want the PAT advice stated once, after the login", msg)
	}
}

// The same refusal from a session FILE that holds no refresh token: the cure is signing in again.
func TestFileSessionWithoutRefreshToken_401_SaysRefusedAndSignIn(t *testing.T) {
	cp := newRefreshCP(t, "", time.Hour)
	sess := &Session{Client: NewCloudClient(cp.URL), Path: filepath.Join(t.TempDir(), "session.json"),
		data: SessionData{AccessToken: fakeJWT(time.Now().Add(time.Hour))}} // live-looking, so no proactive refresh
	err := sess.Do(context.Background(), func(string) error {
		return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "list workspaces failed: status 401"}
	})
	if err == nil {
		t.Fatal("a 401 with nothing to refresh from must be an error")
	}
	msg := err.Error()
	for _, want := range []string{"the control plane refused", "argus cloud-login --control-plane " + cp.URL} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "--token or ARGUS_CP_AUTHOR_TOKEN") {
		t.Errorf("error = %q blames --token/ARGUS_CP_AUTHOR_TOKEN for a session-file credential", msg)
	}
	if StatusOf(err) != http.StatusUnauthorized || !errors.Is(err, ErrNoRefresh) {
		t.Errorf("StatusOf = %d, errors.Is(ErrNoRefresh) = %v; want 401 and true", StatusOf(err), errors.Is(err, ErrNoRefresh))
	}
	if cp.calls() != 0 {
		t.Errorf("refresh endpoint called %d times with no refresh token to send", cp.calls())
	}
}

// A Session built without a client, or with a client that has no base URL, still answers a refusal with a
// command that shows where the URL goes, never an empty "--control-plane " and never a nil dereference.
func TestStatic401_WithoutAKnownControlPlane_SaysWhereTheURLGoes(t *testing.T) {
	for name, client := range map[string]*CloudClient{"nil client": nil, "empty base URL": NewCloudClient("")} {
		t.Run(name, func(t *testing.T) {
			sess := NewStaticSession(client, "odts_synthetic_refused_value")
			err := sess.Do(context.Background(), func(string) error {
				return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "status 401"}
			})
			if err == nil {
				t.Fatal("a 401 must be an error")
			}
			if !strings.Contains(err.Error(), "argus cloud-login --control-plane <control-plane-url> --scope author") {
				t.Errorf("error = %q, want the <control-plane-url> placeholder in the cure", err.Error())
			}
		})
	}
}

// The cure repeats the control-plane URL so it runs as printed, and the error lands in stdout JSON (emitErr). A URL
// can carry a credential (user:password@, ?token=), so only scheme, host and path are repeated, and a URL that the
// shell would split (&, spaces) is single-quoted. Hop's review of #357, point 1, applied to this message too.
func TestRefused401_RepeatsTheControlPlaneURLWithoutItsCredentials(t *testing.T) {
	cases := []struct {
		name, baseURL, want string
		secrets             []string
	}{
		{"userinfo, query and fragment dropped", "https://svc:pw-synthetic@cp.example.test/base?token=q-synthetic#f-synthetic",
			"--control-plane https://cp.example.test/base --scope author", []string{"pw-synthetic", "q-synthetic", "f-synthetic", "svc:"}},
		{"shell metacharacters quoted", "https://cp.example.test/a&b",
			"--control-plane 'https://cp.example.test/a&b' --scope author", nil},
		{"IPv6 host quoted (brackets are shell glob characters)", "http://[::1]:18501",
			"--control-plane 'http://[::1]:18501' --scope author", nil},
		{"unparseable URL not repeated", "https://pw-synthetic@[::1",
			"--control-plane <control-plane-url> --scope author", []string{"pw-synthetic"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"", filepath.Join(t.TempDir(), "session.json")} {
				sess := &Session{Client: NewCloudClient(tc.baseURL), Path: path, data: SessionData{AccessToken: "odts_synthetic_refused_value"}}
				err := sess.Do(context.Background(), func(string) error {
					return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "status 401"}
				})
				if err == nil {
					t.Fatal("a 401 must be an error")
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("path=%q: error = %q, want %q", path, err.Error(), tc.want)
				}
				for _, secret := range tc.secrets {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("path=%q: error = %q repeats %q from the URL", path, err.Error(), secret)
					}
				}
			}
		})
	}
}

// ── empty session (no credential at all): a 401 names the missing credential ────────────────────────

// REPLAY 2026-09-28 fresh-user trial, error 1: with no sign-in, `cloud-list-workspaces` went out anonymous,
// the control plane answered 401, and the error said "this token came from --token or ARGUS_CP_AUTHOR_TOKEN".
// There was no token. The session had nothing to refresh, so the 401 must say that nothing was presented,
// and not blame a flag nobody used.
func TestNewEmptySession_401_SaysNoCredentialWasPresented(t *testing.T) {
	cp := newRefreshCP(t, "", time.Hour)
	sess := NewEmptySession(NewCloudClient(cp.URL))
	calls := 0
	err := sess.Do(context.Background(), func(token string) error {
		calls++
		if token != "" {
			t.Errorf("an empty session presented a token")
		}
		return &HTTPStatusError{Status: http.StatusUnauthorized, Msg: "/api/workspaces answered HTTP 401"}
	})
	if err == nil {
		t.Fatal("a 401 on an empty session must be an error")
	}
	for _, s := range []string{"no credential", "argus cloud-login"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error = %q, want it to say %q", err.Error(), s)
		}
	}
	if strings.Contains(err.Error(), "came from --token") {
		t.Errorf("error = %q blames --token / ARGUS_CP_AUTHOR_TOKEN, and none was set", err.Error())
	}
	if StatusOf(err) != http.StatusUnauthorized {
		t.Errorf("StatusOf(err) = %d, want 401 kept for callers that branch on it", StatusOf(err))
	}
	if calls != 1 || cp.calls() != 0 {
		t.Errorf("fn called %d times, refresh endpoint %d times; want 1 and 0 (nothing to refresh, so no retry)", calls, cp.calls())
	}
}

// ── cloud-logout: revokes, deletes, idempotent ──────────────────────────────────────────────────

func TestLogout_RevokesDeletesAndIsIdempotent(t *testing.T) {
	cp := newRefreshCP(t, "rt_to_revoke", time.Hour)
	client := NewCloudClient(cp.URL)
	path := filepath.Join(t.TempDir(), "session.json")
	if err := SaveSession(path, SessionData{ControlPlane: cp.URL, AccessToken: fakeJWT(time.Now().Add(time.Hour)), RefreshToken: "rt_to_revoke"}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	hadSession, revoked, err := Logout(context.Background(), client, path)
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if !hadSession || !revoked {
		t.Fatalf("hadSession=%v revoked=%v, want true/true", hadSession, revoked)
	}
	cp.mu.Lock()
	sawRevoke := len(cp.revokedTokens) == 1 && cp.revokedTokens[0] == "rt_to_revoke"
	cp.mu.Unlock()
	if !sawRevoke {
		t.Errorf("the control plane did not observe the revoke: %v", cp.revokedTokens)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("session file still exists after logout: %v", statErr)
	}
	// Idempotent: a second logout with nothing to log out of is success, not an error.
	hadSession2, revoked2, err2 := Logout(context.Background(), client, path)
	if err2 != nil {
		t.Fatalf("second Logout: %v", err2)
	}
	if hadSession2 || revoked2 {
		t.Errorf("second Logout: hadSession=%v revoked=%v, want false/false", hadSession2, revoked2)
	}
}

// ── MUTANT: remove the proactive refresh and the before-expiry test goes RED ───────────────────────
//
// This is TestSession_ProactiveRefreshBeforeExpiry_PersistsRotatedToken's own assertion, isolated:
// with ensureFresh's "is it near expiry" check short-circuited to "never", Do hands fn the OLD,
// about-to-expire token and never calls the refresh endpoint at all. Evidence for this is pasted in
// the AC-D24 evidence report by literally toggling ensureFresh (see session.go) and re-running
// TestSession_ProactiveRefreshBeforeExpiry_PersistsRotatedToken — this function documents WHAT that
// mutation breaks so the two don't drift apart.
func TestSession_MutantDocumentation_ProactiveRefreshIsWhatMakesThisPass(t *testing.T) {
	cp := newRefreshCP(t, "rt_initial", time.Hour)
	client := NewCloudClient(cp.URL)
	sess := &Session{Client: client, data: SessionData{
		AccessToken: fakeJWT(time.Now().Add(30 * time.Second)), RefreshToken: "rt_initial",
	}}
	if err := sess.Do(context.Background(), func(token string) error { return nil }); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sess.RefreshCount() != 1 {
		t.Fatal("MUTANT CHECK: with the proactive refresh intact this must be 1 — if ensureFresh's " +
			"nearExpiry check is disabled (the mutation), this assertion is what goes RED, because " +
			"the near-expiry access token is handed to fn untouched and the refresh endpoint is never called")
	}
}

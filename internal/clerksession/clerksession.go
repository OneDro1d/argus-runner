// Package clerksession is the executor's `clerk_session` auth mode: a short-lived Clerk
// session token minted from the Clerk BACKEND API and sent as the SUT's `Authorization: Bearer` header,
// instead of one static token somebody pasted into a Secret.
//
// What it does, in the only order Clerk allows:
//
//	POST {api}/v1/sessions                       {"user_id": "user_…"}   -> {"id": "sess_…"}
//	POST {api}/v1/sessions/{id}/tokens           (or …/tokens/{template}) -> {"jwt": "eyJ…"}
//	POST {api}/v1/sessions/{id}/revoke                                   (run end)
//
// all three with `Authorization: Bearer <Clerk SECRET key>`. The secret key never leaves this package
// except as that header on a request to the declared api_url.
//
// ⛔ NOTHING HERE PRINTS A SECRET. Every error is built from a fixed sentence plus an HTTP status or a
// transport error with the request stripped off (a *url.Error carries the URL, never a header, and the URL
// is the api_url, which is not a secret). The response body is never echoed: it is the one place a Clerk
// error could repeat what we sent. Callers may log Prefix(token) (six characters) and nothing longer.
//
// ⛔ THE TOKEN IS SHORT-LIVED. A Clerk session token lives 60 seconds unless a JWT template says otherwise,
// so Token() re-mints when less than max(15 s, 25 % of its life) is left. The check runs on every call and
// one refresh is in flight at a time: concurrent callers wait on the same mutex and find the fresh token.
// The session itself is created once per Manager and re-used for every refresh.
package clerksession

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIURL is Clerk's Backend API. A test (or a self-hosted proxy) points api_url elsewhere.
	DefaultAPIURL = "https://api.clerk.com"

	// MinRemaining and MinFraction: a token with less than MinRemaining left, or less than MinFraction of
	// its life left, is re-minted before it is used. 60 s tokens hit both at 15 s; an hour-long template
	// token refreshes with 15 minutes left.
	MinRemaining = 15 * time.Second
	MinFraction  = 0.25

	// mintTimeout bounds each mint call; revokeTimeout bounds the run-end revoke so a dead Clerk cannot
	// hold a finished run open.
	mintTimeout   = 15 * time.Second
	revokeTimeout = 5 * time.Second

	maxBody = 1 << 20
)

// Params is one Clerk identity: which instance's API, which secret key, which user, which template.
type Params struct {
	APIURL    string // "" = DefaultAPIURL
	SecretKey string // sk_… — the VALUE, read from the environment by the caller
	UserID    string // user_…
	Template  string // optional JWT template name; "" = Clerk's default session token
}

// Error is a mint/refresh/revoke failure. Its text names the stage and the HTTP status, never a secret.
type Error struct {
	Stage  string // "create a session" | "mint a session token" | "revoke the session"
	Detail string // "HTTP 401" | a transport reason | a shape problem
}

func (e *Error) Error() string { return "clerk_session: could not " + e.Stage + ": " + e.Detail }

// Manager owns one Clerk session and its current token.
type Manager struct {
	p       Params
	now     func() time.Time
	hc      *http.Client
	revokeT time.Duration

	mu        sync.Mutex
	sessionID string
	createErr error // sticky: a refused session create is the config's fault and is not retried per scenario
	token     string
	exp       time.Time
	life      time.Duration
	revoked   bool
}

// Option tunes a Manager (tests only; production uses the defaults).
type Option func(*Manager)

// WithClock replaces time.Now — the refresh tests move it.
func WithClock(f func() time.Time) Option { return func(m *Manager) { m.now = f } }

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(m *Manager) { m.hc = c } }

// WithRevokeTimeout replaces the revoke bound.
func WithRevokeTimeout(d time.Duration) Option { return func(m *Manager) { m.revokeT = d } }

// New builds a Manager. It does no I/O: the session is created by the first Token().
func New(p Params, opts ...Option) *Manager {
	if strings.TrimSpace(p.APIURL) == "" {
		p.APIURL = DefaultAPIURL
	}
	p.APIURL = strings.TrimRight(strings.TrimSpace(p.APIURL), "/")
	m := &Manager{p: p, now: time.Now, hc: &http.Client{Timeout: mintTimeout}, revokeT: revokeTimeout}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Token returns a session token with enough life left, minting or refreshing it first when needed.
func (m *Manager) Token() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return "", m.createErr
	}
	if m.token != "" && !m.stale() {
		return m.token, nil
	}
	if m.sessionID == "" {
		id, err := m.createSession()
		if err != nil {
			m.createErr = err
			return "", err
		}
		m.sessionID = id
	}
	tok, err := m.mintToken()
	if err != nil {
		return "", err
	}
	return tok, nil
}

// stale reports whether the cached token is inside its refresh window. Caller holds mu.
func (m *Manager) stale() bool {
	left := m.exp.Sub(m.now())
	if left < MinRemaining {
		return true
	}
	return float64(left) < MinFraction*float64(m.life)
}

// Revoke ends the session. It is a no-op when none was created or it was already revoked, and it is
// bounded by the revoke timeout. The caller logs a failure; it never changes a verdict.
func (m *Manager) Revoke() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionID == "" || m.revoked {
		return nil
	}
	m.revoked = true // one attempt: a failed revoke is logged, not retried against a dead API
	ctx, cancel := context.WithTimeout(context.Background(), m.revokeT)
	defer cancel()
	_, err := m.post(ctx, "/v1/sessions/"+url.PathEscape(m.sessionID)+"/revoke", nil, "revoke the session")
	if err != nil {
		return err
	}
	m.token = ""
	return nil
}

// Values are the strings a scrubber must remove from anything it keeps: the secret key and the live
// token. A session id is not a credential.
func (m *Manager) Values() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{m.p.SecretKey}
	if m.token != "" {
		out = append(out, m.token)
	}
	return out
}

// Prefix is the only part of a token that may be logged: its first six characters.
func Prefix(token string) string {
	if len(token) <= 6 {
		return token
	}
	return token[:6]
}

func (m *Manager) createSession() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"user_id": m.p.UserID})
	raw, err := m.post(ctx, "/v1/sessions", body, "create a session")
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &out) != nil || out.ID == "" {
		return "", &Error{"create a session", "the response carried no session id"}
	}
	return out.ID, nil
}

func (m *Manager) mintToken() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mintTimeout)
	defer cancel()
	path := "/v1/sessions/" + url.PathEscape(m.sessionID) + "/tokens"
	if m.p.Template != "" {
		path += "/" + url.PathEscape(m.p.Template)
	}
	raw, err := m.post(ctx, path, nil, "mint a session token")
	if err != nil {
		return "", err
	}
	var out struct {
		JWT string `json:"jwt"`
	}
	if json.Unmarshal(raw, &out) != nil || out.JWT == "" {
		return "", &Error{"mint a session token", "the response carried no jwt"}
	}
	exp, iat, ok := claims(out.JWT)
	if !ok {
		return "", &Error{"mint a session token", "the jwt carried no exp claim"}
	}
	m.token, m.exp = out.JWT, exp
	m.life = exp.Sub(iat)
	if iat.IsZero() || m.life <= 0 {
		m.life = exp.Sub(m.now())
	}
	return m.token, nil
}

// post sends one Backend API call. The error never contains the response body or a header.
func (m *Manager) post(ctx context.Context, path string, body []byte, stage string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.p.APIURL+path, rd)
	if err != nil {
		return nil, &Error{stage, "the api_url is not a usable URL"}
	}
	req.Header.Set("Authorization", "Bearer "+m.p.SecretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drops the method + URL wrapper; the transport reason stays
		}
		return nil, &Error{stage, scrub(err.Error(), m.p.SecretKey)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &Error{stage, fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return raw, nil
}

func scrub(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}

// claims reads exp and iat from a JWT's payload. The signature is not checked: the SUT does that, and a
// token Clerk just handed us over TLS is not something the client second-guesses.
func claims(jwt string) (exp, iat time.Time, ok bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return exp, iat, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return exp, iat, false
	}
	var c struct {
		Exp float64 `json:"exp"`
		Iat float64 `json:"iat"`
	}
	if json.Unmarshal(b, &c) != nil || c.Exp <= 0 {
		return exp, iat, false
	}
	exp = time.Unix(int64(c.Exp), 0)
	if c.Iat > 0 {
		iat = time.Unix(int64(c.Iat), 0)
	}
	return exp, iat, true
}

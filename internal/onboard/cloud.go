// Package onboard also holds the M3 CLOUD-onboarding client (D-ONBOARD.7): the OAuth device-code login
// (RFC 8628) + the small post-login queries (instance-name availability) + the instance-name charset
// rule. Kept in Go (not bash) so onboard.sh's cloud steps are unit/integration-testable — the same
// pattern as the existing secrets-scan / render-obs / validate-config subcommands.
package onboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OneDro1d/argus-runner/internal/retry"
)

// CloudClient talks to the M3 control plane during onboarding.
type CloudClient struct {
	BaseURL string
	HC      *http.Client
	// Retry runs one control-plane call under VR-F2b's policy. nil means retry.Do — the real thing.
	// Tests set retry.DoNoWait so that asserting a 503 IS retried costs no wall clock; see do().
	Retry func(context.Context, func(context.Context) error) error
}

// NewCloudClient builds a client for the control plane at baseURL.
func NewCloudClient(baseURL string) *CloudClient {
	return &CloudClient{BaseURL: strings.TrimRight(baseURL, "/"), HC: &http.Client{Timeout: 30 * time.Second}}
}

// RegisterClient does dynamic client registration (DCR) for a PUBLIC device-code client, returning the
// client_id (no secret — token_endpoint_auth_method=none).
func (c *CloudClient) RegisterClient(ctx context.Context, name string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                name,
		"grant_types":                []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
		"token_endpoint_auth_method": "none",
	})
	var out struct {
		ClientID string `json:"client_id"`
	}
	code, err := c.postJSON(ctx, "/oauth/register", body, "", &out)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK && code != http.StatusCreated || out.ClientID == "" {
		return "", fmt.Errorf("dcr failed: status %d", code)
	}
	return out.ClientID, nil
}

// DeviceAuthResp is the device_authorization response (RFC 8628).
type DeviceAuthResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// DeviceAuth starts the device-code flow.
func (c *CloudClient) DeviceAuth(ctx context.Context, clientID, scope string) (*DeviceAuthResp, error) {
	form := url.Values{"client_id": {clientID}, "scope": {scope}}
	var d DeviceAuthResp
	code, err := c.postForm(ctx, "/oauth/device_authorization", form, &d)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK || d.DeviceCode == "" {
		return nil, fmt.Errorf("device_authorization failed: status %d", code)
	}
	return &d, nil
}

// PollTokens polls the token endpoint until the user approves (honoring authorization_pending /
// slow_down) or the deadline passes, returning both the access token and — when the control plane
// minted one (AC-D24: it does, unconditionally, on an approved device-code poll; see
// internal/control/oauth/endpoints.go grantDevice) — the refresh token. PollToken below is the
// pre-AC-D24 single-value form, kept for callers that only ever wanted the access token.
func (c *CloudClient) PollTokens(ctx context.Context, clientID, deviceCode string, interval int, deadline time.Time) (access, refresh string, err error) {
	if interval <= 0 {
		interval = 5
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode}, "client_id": {clientID},
	}
	for {
		var tok struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
		}
		if _, err := c.postForm(ctx, "/oauth/token", form, &tok); err != nil {
			return "", "", err
		}
		if tok.AccessToken != "" {
			return tok.AccessToken, tok.RefreshToken, nil
		}
		if tok.Error != "" && tok.Error != "authorization_pending" && tok.Error != "slow_down" {
			return "", "", fmt.Errorf("device login denied: %s", tok.Error)
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("device login timed out (never approved)")
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

// PollToken is PollTokens without the refresh token — kept for the existing callers (internal/control
// tests) that only ever needed the access token.
func (c *CloudClient) PollToken(ctx context.Context, clientID, deviceCode string, interval int, deadline time.Time) (string, error) {
	access, _, err := c.PollTokens(ctx, clientID, deviceCode, interval, deadline)
	return access, err
}

// Refresh performs one refresh_token grant against /oauth/token. It never retries and never loops —
// Session.refreshOnce (session.go) is the single caller allowed to decide what happens after a
// failure. The refresh token in the response may be empty: the control plane does not rotate it today
// ("no rotation in M3 — simplest", endpoints.go grantRefresh), and the caller must keep using the old
// one in that case — see Session.refreshOnce.
type RefreshedTokens struct {
	AccessToken  string
	RefreshToken string // "" when the control plane did not rotate it
	ExpiresIn    int
}

func (c *CloudClient) Refresh(ctx context.Context, refreshToken string) (*RefreshedTokens, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	code, err := c.postForm(ctx, "/oauth/token", form, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK || out.AccessToken == "" {
		msg := out.ErrorDesc
		if msg == "" {
			msg = out.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("status %d", code)
		}
		return nil, &HTTPStatusError{Status: code, Msg: "refresh: " + msg}
	}
	return &RefreshedTokens{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, ExpiresIn: out.ExpiresIn}, nil
}

// RevokeRefresh revokes a refresh token at the control plane (RFC 7009, POST /oauth/revoke —
// internal/control/oauth/endpoints.go handleRevoke) so it can no longer mint an access token even
// though its 30-day RefreshTTL has not elapsed. `argus cloud-logout` is the only caller (AC-D24
// REQUIRED #3). Per RFC 7009 §2.2 the endpoint answers 200 whether or not the token existed, so a
// non-200 here means the request itself was malformed or the control plane could not be reached —
// never "the token was already gone".
func (c *CloudClient) RevokeRefresh(ctx context.Context, refreshToken string) error {
	form := url.Values{"token": {refreshToken}, "token_type_hint": {"refresh_token"}}
	code, err := c.postForm(ctx, "/oauth/revoke", form, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return &HTTPStatusError{Status: code, Msg: fmt.Sprintf("revoke: status %d", code)}
	}
	return nil
}

// EnrollmentResult is the AC-D25 `POST /api/enrollments` response (internal/control/web.go
// handleMintEnrollment): a single-use, 15-min-TTL, workspace-bound executor enrollment credential.
type EnrollmentResult struct {
	Token        string `json:"enrollment_token"`
	EnrollmentID string `json:"enrollment_id"`
	Workspace    string `json:"workspace"`
	ExpiresIn    int    `json:"expires_in"`
}

// MintEnrollment mints one enrollment credential for the caller's workspace (AC-D25). The token is
// returned ONCE, exactly as the control plane returns it — cloud-enroll writes it straight to
// DIR/<instance>.enrollment and never logs it.
func (c *CloudClient) MintEnrollment(ctx context.Context, token string) (*EnrollmentResult, error) {
	var out EnrollmentResult
	code, err := c.postJSON(ctx, "/api/enrollments", []byte("{}"), token, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK || out.Token == "" {
		return nil, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("mint enrollment failed: status %d", code)}
	}
	return &out, nil
}

// RevokeEnrollment retires an enrollment credential (`cloud-enroll --revoke <id>`, AC-D25) via
// DELETE /api/enrollments/{id}. `revoked:false` is an ordinary outcome (the credential may already
// have been consumed or expired) — only a transport/status failure is an error.
func (c *CloudClient) RevokeEnrollment(ctx context.Context, token, enrollmentID string) (bool, error) {
	var out struct {
		Revoked bool `json:"revoked"`
	}
	code, err := c.deleteJSON(ctx, "/api/enrollments/"+url.PathEscape(enrollmentID), token, &out)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("revoke enrollment failed: status %d", code)}
	}
	return out.Revoked, nil
}

// Login runs the full device-code login: DCR → device_authorization → print the human instruction to
// `out` → poll for approval → the access token (author scope, workspace-bound) + the refresh token
// AC-D24 persists so cloud-login is the only sign-in a human ever has to do.
func (c *CloudClient) Login(ctx context.Context, scope string, out io.Writer) (access, refresh string, err error) {
	clientID, err := c.RegisterClient(ctx, "argus-onboarding")
	if err != nil {
		return "", "", err
	}
	d, err := c.DeviceAuth(ctx, clientID, scope)
	if err != nil {
		return "", "", err
	}
	// R6 (owner-approved text, 2026-07-17): the SINGLE merged sign-in banner. onboard.sh no longer prints
	// a PART-1 preamble — this is the only place with the device URL/code. The code-carrying line
	// (verification_uri_complete) stays greppable: the onboard.sh host-side watcher opens the first URL
	// matching oauth/device?...user_code= in a browser. The real device-code case is preserved.
	fmt.Fprintf(out, `
Signing into your Argus account.

A browser window is opening; if it didn't, open this link yourself:
   %s

Create your OneDroid account (first time here) or log in.
That approves your device, saves your token, and continues onboarding automatically.

Waiting for you to sign in…

`, d.VerificationURIComplete)
	ttl := d.ExpiresIn
	if ttl < 300 {
		ttl = 300
	}
	return c.PollTokens(ctx, clientID, d.DeviceCode, d.Interval, time.Now().Add(time.Duration(ttl)*time.Second))
}

// InstanceAvailable reports whether an instance_id is free to claim (globally unique per CP). Advisory —
// registration is the authoritative claim (UC121/UC124).
func (c *CloudClient) InstanceAvailable(ctx context.Context, token, instanceID string) (bool, error) {
	var out struct {
		Available bool `json:"available"`
	}
	code, err := c.getJSON(ctx, "/api/instances/available?instance_id="+url.QueryEscape(instanceID), token, &out)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("availability check failed: status %d", code)}
	}
	return out.Available, nil
}

// InstanceCheck is the availability-check response (UC121 + UC149): free to claim, and — when taken —
// whether it is the caller's OWN registration and whether that registration looks dead (stale).
type InstanceCheck struct {
	Available  bool `json:"available"`
	OwnedByYou bool `json:"owned_by_you"`
	Stale      bool `json:"stale"`
}

// CheckInstance runs the availability check and returns the full claim info (UC121/UC149) — powering the
// onboarding collision assist (propose a rename, or offer to purge YOUR dead registration).
func (c *CloudClient) CheckInstance(ctx context.Context, token, instanceID string) (*InstanceCheck, error) {
	var out InstanceCheck
	code, err := c.getJSON(ctx, "/api/instances/available?instance_id="+url.QueryEscape(instanceID), token, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("availability check failed: status %d", code)}
	}
	return &out, nil
}

// Workspace is one of the user's workspaces as the web API returns it (UC119 onboarding list).
type Workspace struct {
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	InstanceCount int                 `json:"instance_count"`
	Active        bool                `json:"active"`
	Instances     []WorkspaceInstance `json:"instances"` // B2: the registered SUTs + environments per workspace
}

// WorkspaceInstance is one registered instance in a workspace as the picker shows it (B2).
type WorkspaceInstance struct {
	InstanceID string `json:"instance_id"`
	SUTName    string `json:"sut_name"`
	Tier       string `json:"tier"`
}

// WorkspaceList is the GET /api/workspaces response. SameSUT is populated only when a sut was queried
// (UC120): the owner's OTHER workspaces already holding that SUT name.
type WorkspaceList struct {
	Workspaces []Workspace `json:"workspaces"`
	Active     string      `json:"active"`
	SameSUT    []Workspace `json:"same_sut"`
}

// ListWorkspaces lists the user's workspaces (UC119). When sut is non-empty the response's SameSUT holds
// the owner's OTHER workspaces (excluding the caller's active one) already holding that SUT (UC120 — the
// same-SUT warning). Only the owner's own workspaces are ever consulted.
func (c *CloudClient) ListWorkspaces(ctx context.Context, token, sut string) (*WorkspaceList, error) {
	path := "/api/workspaces"
	if sut != "" {
		path += "?sut=" + url.QueryEscape(sut)
	}
	var out WorkspaceList
	code, err := c.getJSON(ctx, path, token, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("list workspaces failed: status %d", code)}
	}
	return &out, nil
}

// CreateWorkspace creates a NEW workspace owned by the login user (UC002 over the login session — the B2
// onboarding picker's "create new" branch). Returns the new workspace id.
func (c *CloudClient) CreateWorkspace(ctx context.Context, sessionToken, name string) (string, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	code, err := c.postJSON(ctx, "/api/workspaces/create", body, sessionToken, &out)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK || out.ID == "" {
		if out.Error != "" {
			return "", &HTTPStatusError{Status: code, Msg: "create workspace: " + out.Error}
		}
		return "", &HTTPStatusError{Status: code, Msg: fmt.Sprintf("create workspace failed: status %d", code)}
	}
	return out.ID, nil
}

// SwitchWorkspace re-binds the login session to another OWNED workspace (UC004 over the login session —
// the B2 picker). The CP ownership-checks and mints a NEW session token bound to that workspace; every
// later onboarding step (mint/seed/register) must use the returned token.
func (c *CloudClient) SwitchWorkspace(ctx context.Context, sessionToken, workspaceID string) (string, error) {
	body, _ := json.Marshal(map[string]string{"workspace": workspaceID})
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	code, err := c.postJSON(ctx, "/api/workspaces/switch", body, sessionToken, &out)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK || out.Token == "" {
		if out.Error != "" {
			return "", &HTTPStatusError{Status: code, Msg: "switch workspace: " + out.Error}
		}
		return "", &HTTPStatusError{Status: code, Msg: fmt.Sprintf("switch workspace failed: status %d", code)}
	}
	return out.Token, nil
}

// MintedToken is the response of minting an author token (POST /api/tokens). S7 (CP-M3-122): the
// endpoints carry NO embedded token — the PAT travels header-only (Authorization: Bearer …).
type MintedToken struct {
	ID          string `json:"id"`
	Token       string `json:"token"`
	Endpoint    string `json:"endpoint"`     // <origin>/mcp — pair with the Authorization header
	SSEEndpoint string `json:"sse_endpoint"` // <origin>/sse — legacy face, same header
	// V27-009 redesign: the expiry (RFC3339) and, for an auto mint, the id of the row this mint retired ("" on a
	// first mint) — the router keeps the expiry beside the token; nothing refuses on it.
	ExpiresAt       string `json:"expires_at,omitempty"`
	ReplacedTokenID string `json:"replaced_token_id,omitempty"`
}

// ErrAutoTokenExists reports the control plane's 409: this account already holds a live
// author token minted during onboarding (VR-B3). The caller reuses the credential it already has rather than
// treating it as a failure — see MintOrReuseAuthorToken.
var ErrAutoTokenExists = errors.New("an author token (minted during onboarding) already exists for this account and machine")

// MintAuthorToken mints an author token via the web token API using the login session (UC131/UC011 —
// the onboarding auto-mint; the user never handles a token on the direct path).
//
// auto marks it minted BY ONBOARDING (VR-B8). That label is what the Tokens page reads to say "auto"
// versus "manual", and writing it also opts the token into the one-per-owner rule — so a second
// onboard gets ErrAutoTokenExists rather than a second token.
func (c *CloudClient) MintAuthorToken(ctx context.Context, sessionToken, name string, auto bool, routerID string) (*MintedToken, error) {
	// V27-009 redesign: an auto mint names the MACHINE (this router's id); the control plane refuses one without.
	body, _ := json.Marshal(map[string]any{"name": name, "auto": auto, "router_id": routerID})
	var out MintedToken
	code, err := c.postJSON(ctx, "/api/tokens", body, sessionToken, &out)
	if err != nil {
		return nil, err
	}
	if code == http.StatusConflict {
		return nil, ErrAutoTokenExists
	}
	if code != http.StatusOK || out.Token == "" {
		return nil, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("mint token failed: status %d", code)}
	}
	return &out, nil
}

// MintOrReuseAuthorToken is VR-B3 as onboarding actually has to implement it: at most ONE
// auto-generated token per user, REUSED across onboards, minted only when none exists.
//
// WHY REUSE MEANS THE LOCAL FILE. VR-B7 shows a token exactly once and stores only its hash, so the
// control plane genuinely cannot hand back an existing token's plaintext — not as a policy, as a
// fact. "Reuse it" can therefore only mean: the credential this machine already holds, if it still
// authenticates. Anything else would be a fresh token wearing the old one's name.
//
// The order is deliberate. VALIDATE the local token first and mint only when it is missing or dead,
// so a re-onboard on a working machine performs no write at all. The 409 path exists for the case
// the local file is gone but the server-side token is alive — a fresh checkout, or a wiped kit —
// where the honest answer is that this is recoverable but not automatically: silently revoking the
// live one would break whichever machine is still using it.
func (c *CloudClient) MintOrReuseAuthorToken(ctx context.Context, sessionToken, name, existing, routerID string) (*MintedToken, bool, error) {
	if strings.TrimSpace(existing) != "" && c.TokenWorks(ctx, existing) {
		return &MintedToken{Token: existing, Endpoint: strings.TrimRight(c.BaseURL, "/") + "/mcp",
			SSEEndpoint: strings.TrimRight(c.BaseURL, "/") + "/sse"}, true, nil
	}
	mt, err := c.MintAuthorToken(ctx, sessionToken, name, true, routerID)
	if err != nil {
		return nil, false, err
	}
	return mt, false, nil
}

// TokenWorks reports whether a token still authenticates — the cheapest authenticated call there is,
// used to decide reuse. A network failure answers NO: treating "I could not check" as "it works"
// would hand onboarding a dead credential and blame the next step for it.
//
// THE ENDPOINT IS OWNER-SCOPED, AND THAT IS THE WHOLE POINT (V15-015, found live 2026-08-12).
//
// This probed /api/workspaces until the second onboard on a real estate failed. The token being
// probed is an AUTHOR PAT, which the B-block (0.3.18) made USER-GLOBAL: it carries no workspace, and
// web.go:210 refuses every workspace-scoped web endpoint with 403 "a workspace-bound token is
// required for the web". So this returned false for a perfectly live credential, MintOrReuseAuthorToken
// skipped reuse, the mint hit VR-B3's one-auto-token-per-owner rule, and the operator was told the
// file "does not hold" a token it was holding. Measured: same token, /api/workspaces 403, /mcp 200.
//
// web.go:311 already records this exact class of failure for `router register` and fixed it with
// authedOwner. This probe was not revisited then. It is now:
//
//	/api/routers  is authedOwner (web.go:111) — OWNER tenancy, which is what a user-global token has.
//
// NOT /api/meta, which is the tempting one-liner and is WRONG: it answers 200 UNAUTHENTICATED, so the
// probe would call any string a live token and reuse would install garbage. Whatever endpoint this
// points at must (a) accept an owner-scoped credential and (b) still 401 an unknown one.
func (c *CloudClient) TokenWorks(ctx context.Context, token string) bool {
	var out struct {
		Routers []struct {
			Host string `json:"host"`
		} `json:"routers"`
	}
	code, err := c.getJSON(ctx, "/api/routers", token, &out)
	return err == nil && code == http.StatusOK
}

// RegisterRouter records this machine's local router and its Ed25519 PUBLIC key with the control
// plane (VR-R10). Onboarding calls it once, holding an author credential; the router itself then
// heartbeats with the matching private key and never needs one.
//
// That split is deliberate and is the reason this lives here rather than in the router: a machine
// holding only PRODUCT folders has no author credential by construction, so if registration were the
// router's job it could not be done at all on exactly the machines the holdout matters most on.
//
// Registration is IDEMPOTENT at the store — re-running onboarding re-registers the same router_id
// with the same key, which is what makes it safe to call on every onboard rather than once ever.
// RegisterRouter registers this machine under the session's account and returns the account subject the
// control plane echoes (`owner`, V27-009 redesign) — the router records it beside the control-plane URL.
func (c *CloudClient) RegisterRouter(ctx context.Context, sessionToken, routerID, host, pubKeyB64 string) (owner string, err error) {
	body, _ := json.Marshal(map[string]string{"router_id": routerID, "host": host, "pubkey_b64": pubKeyB64})
	var out struct {
		Registered bool   `json:"registered"`
		Owner      string `json:"owner"`
		Error      string `json:"error"`
	}
	code, err := c.postJSON(ctx, "/api/routers/register", body, sessionToken, &out)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK || !out.Registered {
		if out.Error != "" {
			return "", fmt.Errorf("register router failed: status %d: %s", code, out.Error)
		}
		return "", fmt.Errorf("register router failed: status %d", code)
	}
	return out.Owner, nil
}

// TeardownInstance removes the instance from the control plane using the login session (UC145/UC053) —
// tenancy-checked at the CP; idempotent (returns deleted=false when already gone / not owned).
//
// ⛔ IT NO LONGER READS `auto_token_revoked` (VR10-T4-7). The route stopped deciding anything about a
// credential when the token became per machine, so the field is not sent; a reader for it would be
// nil forever, which teardown would have to render as "nobody said" on every single run.
func (c *CloudClient) TeardownInstance(ctx context.Context, sessionToken, instanceID string) (deleted bool, err error) {
	body, _ := json.Marshal(map[string]string{"instance_id": instanceID})
	var out struct {
		Deleted bool `json:"deleted"`
	}
	code, err := c.postJSON(ctx, "/api/instances/delete", body, sessionToken, &out)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("teardown failed: status %d", code)}
	}
	return out.Deleted, nil
}

// ExecutorPollAccepted reports whether the control plane has ever ACCEPTED a poll from this instance,
// and when. This is the check that says an onboard worked; ExecutorRegistered below says only that a
// row exists.
//
// VR6-I2 (V23-014). Registration returning 200 is not evidence that the executor works. Measured on
// orderservice-compose 2026-08-18: registered and unusable from creation — 7,747 `bad_signature`
// rejections, the first ONE SECOND after the container started, RestartCount 0, and the page reading
// REGISTERED for twenty hours. The executor was healthy, reachable and refused, and every check
// onboarding ran was satisfied.
//
// ✅ THE SIGNAL NEEDED NO NEW PLUMBING. TouchLastSeen runs on every poll — the poll IS the heartbeat
// (D-FED.4) — while RegisterInstance's INSERT never sets last_seen. Instance.LastSeen is a *time.Time,
// so it serialises as null until a poll lands, and InstanceFields has always published it here.
// Therefore `last_seen != null` IS "a poll was accepted", and this reads a value that was already in
// the response.
//
// ⚠ A TRANSPORT OR AUTH FAILURE IS AN ERROR, NOT A VERDICT. Returning (false, nil) on a 500 or a 401
// would make an unreachable control plane indistinguishable from a refused executor, and would fail an
// onboard that was fine. "I could not check" and "it was refused" are different facts with different
// remedies — collapsing them is the failure this whole round is about, and it is exactly what the
// teardown verify block did on 7 of 7 runs.
//
// An instance that is simply ABSENT is (false, zero, nil): registration may not have propagated yet,
// and the caller is expected to poll.
func (c *CloudClient) ExecutorPollAccepted(ctx context.Context, sessionToken, instanceID string) (bool, time.Time, error) {
	var out struct {
		Instances []struct {
			InstanceID string     `json:"instance_id"`
			LastSeen   *time.Time `json:"last_seen"`
		} `json:"instances"`
	}
	code, err := c.getJSON(ctx, "/api/instances", sessionToken, &out)
	if err != nil {
		return false, time.Time{}, err
	}
	if code != http.StatusOK {
		return false, time.Time{}, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("could not check whether the executor is polling: "+
			"/api/instances returned status %d — this is NOT a statement about the executor", code)}
	}
	for _, in := range out.Instances {
		if in.InstanceID != instanceID {
			continue
		}
		if in.LastSeen == nil || in.LastSeen.IsZero() {
			// Registered, never heard from. The defect's exact shape.
			return false, time.Time{}, nil
		}
		return true, *in.LastSeen, nil
	}
	return false, time.Time{}, nil
}

// ListInstancesRaw reads GET /api/instances with the token AS GIVEN. It never refreshes, so a caller
// that promises to change nothing (`argus doctor`) can use it without the session file being
// rewritten. It returns the workspace and each instance row undecoded, for the caller to read the
// fields it needs. A non-200 comes back as *HTTPStatusError carrying the control plane's own reason
// ("a workspace-bound token is required for the web" is a different fix from "invalid credential").
func (c *CloudClient) ListInstancesRaw(ctx context.Context, token string) (string, []json.RawMessage, error) {
	var out struct {
		Workspace string            `json:"workspace"`
		Instances []json.RawMessage `json:"instances"`
		Error     string            `json:"error"`
	}
	code, err := c.getJSON(ctx, "/api/instances", token, &out)
	if err != nil {
		return "", nil, err
	}
	if code != http.StatusOK {
		msg := fmt.Sprintf("/api/instances answered HTTP %d", code)
		if out.Error != "" {
			msg += ": " + out.Error
		}
		return "", nil, &HTTPStatusError{Status: code, Msg: msg}
	}
	return out.Workspace, out.Instances, nil
}

// ExecutorRegistered reports whether the instance shows up under the caller's workspace — the
// REGISTERED fail-loud check after the executor registers on start (UC128).
//
// ⚠ REGISTERED IS NOT WORKING. This answers "is there a row?", which an instance that has never
// successfully polled also satisfies. Use ExecutorPollAccepted above to decide whether an onboard
// succeeded (VR6-I2).
func (c *CloudClient) ExecutorRegistered(ctx context.Context, sessionToken, instanceID string) (bool, error) {
	var out struct {
		Instances []struct {
			InstanceID string `json:"instance_id"`
		} `json:"instances"`
	}
	code, err := c.getJSON(ctx, "/api/instances", sessionToken, &out)
	if err != nil {
		return false, err
	}
	if code != http.StatusOK {
		return false, &HTTPStatusError{Status: code, Msg: fmt.Sprintf("instances lookup failed: status %d", code)}
	}
	for _, in := range out.Instances {
		if in.InstanceID == instanceID {
			return true, nil
		}
	}
	return false, nil
}

// WorkspaceFromToken decodes the OAuth access token's "workspace" claim WITHOUT verifying the signature
// (the caller already holds the token; this is just to learn which workspace the login bound). "" on any
// parse failure. Used by cloud-login to report the workspace the executor should register under (UC119).
// SubjectFromToken returns the `sub` claim of a session/access JWT ("" when the token is not a JWT or carries
// none). V27-009 redesign: the router's record for a control plane is keyed by (URL, user), and the user is the
// session's subject — the same value the control plane echoes as `owner` on register.
func SubjectFromToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	blob, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(blob, &claims) != nil {
		return ""
	}
	return claims.Sub
}

func WorkspaceFromToken(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Workspace string `json:"workspace"`
	}
	_ = json.Unmarshal(b, &claims)
	return claims.Workspace
}

// VerifyToolsList runs the streamable-HTTP MCP handshake (initialize → notifications/initialized →
// tools/list) against the cloud /mcp with the author token, returning the visible tool names — the
// cloud-wiring verification during onboarding (UC126). The Camp-B transport replies plain JSON.
func (c *CloudClient) VerifyToolsList(ctx context.Context, authorToken string) ([]string, error) {
	sid, err := c.mcpInit(ctx, authorToken)
	if err != nil {
		return nil, err
	}
	_, _, _ = c.mcpPost(ctx, authorToken, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`) // 202, no body
	_, body, err := c.mcpPost(ctx, authorToken, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &out) != nil {
		return nil, fmt.Errorf("tools/list: bad response")
	}
	names := make([]string, 0, len(out.Result.Tools))
	for _, t := range out.Result.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}

// ── D1 (R9/R11): seed the cloud scenario catalog at onboarding ────────────────────────────────────
// The cloud registry starts empty; the onboarder uploads the baked/BYO scenarios so the web Scenarios
// page + author__list_scenarios show them from the first run.

// ScenarioFile is one scenario to seed: its registry key (POSIX-relative path) + the markdown content.
type ScenarioFile struct {
	Path    string
	Content string
}

// SeedFailure records one scenario the CP rejected (validation error / tool error). It is informational
// — a rejected scenario never aborts the seed.
type SeedFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// SeedResult is the outcome of a seed run: how many of Total scenarios were Written, and which Failed.
type SeedResult struct {
	Total   int           `json:"total"`
	Written int           `json:"written"`
	Failed  []SeedFailure `json:"failed,omitempty"`
}

// SeedScenarios uploads each scenario into the cloud catalog via the author__write_scenario MCP tool,
// reusing ONE streamable-HTTP session (initialize once, then a tools/call per scenario). Each write
// carries the registered instance_id + the registry path + the content; the CP validates-then-writes.
// A per-scenario validation/tool failure is RECORDED (not fatal) so one bad scenario never blocks the
// rest; only a handshake/transport failure returns a hard error.
func (c *CloudClient) SeedScenarios(ctx context.Context, authorToken, instanceID string, scenarios []ScenarioFile) (*SeedResult, error) {
	sid, err := c.mcpInit(ctx, authorToken)
	if err != nil {
		return nil, err
	}
	_, _, _ = c.mcpPost(ctx, authorToken, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`) // 202, no body
	res := &SeedResult{Total: len(scenarios)}
	for i, s := range scenarios {
		call, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": i + 2, "method": "tools/call",
			"params": map[string]any{
				// VR-C5: the CLOUD publishes the BARE name. This call goes DIRECTLY to the control
				// plane — it does not pass through the local router, so nothing translates it here.
				"name":      "author_write_scenario",
				"arguments": map[string]any{"instance_id": instanceID, "path": s.Path, "content": s.Content},
			},
		})
		status, body, perr := c.mcpPost(ctx, authorToken, sid, string(call))
		if perr != nil {
			res.Failed = append(res.Failed, SeedFailure{Path: s.Path, Error: perr.Error()})
			continue
		}
		if written, msg := parseWriteResult(status, body); written {
			res.Written++
		} else {
			res.Failed = append(res.Failed, SeedFailure{Path: s.Path, Error: msg})
		}
	}
	return res, nil
}

// parseWriteResult reads an author__write_scenario JSON-RPC response: (written, message-when-not). A
// top-level JSON-RPC error, an empty result, an isError tool result, or a payload with written:false
// all count as NOT written — the returned message surfaces the CP's own validation text.
func parseWriteResult(status int, body []byte) (bool, string) {
	var rpc struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &rpc) != nil {
		return false, fmt.Sprintf("bad response (status %d): %s", status, truncate(string(body), 200))
	}
	if rpc.Error != nil {
		return false, rpc.Error.Message
	}
	if rpc.Result == nil || len(rpc.Result.Content) == 0 {
		return false, fmt.Sprintf("empty tool result (status %d)", status)
	}
	text := strings.TrimSpace(rpc.Result.Content[0].Text)
	var payload struct {
		Written bool              `json:"written"`
		Error   string            `json:"error"`  // a refusal by name (a duplicate id, a cap)
		Errors  []json.RawMessage `json:"errors"` // validation: [{line, message}] — or bare strings
	}
	_ = json.Unmarshal([]byte(text), &payload)
	if payload.Written {
		return true, ""
	}
	if reason := renderRefusal(payload.Error, payload.Errors); reason != "" {
		return false, reason
	}
	return false, text // an unrecognised payload — surfaced verbatim rather than lost
}

// renderRefusal (VR10-S4-6) turns the control plane's refusal payload into the plain-English reason
// the operator fixes the file by: "line N: <message>" per validation error, joined with "; ", or the
// refusal sentence itself. It returns "" when the payload carries neither, so the caller can fall
// back to the raw text — an unexpected refusal must still be SEEN, which is the whole point of V28-006.
func renderRefusal(errText string, errs []json.RawMessage) string {
	var parts []string
	for _, raw := range errs {
		var str string
		if json.Unmarshal(raw, &str) == nil { // an error given as a bare string
			if str != "" {
				parts = append(parts, str)
			}
			continue
		}
		var e struct {
			Line    int    `json:"line"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			if e.Line > 0 {
				parts = append(parts, fmt.Sprintf("line %d: %s", e.Line, e.Message))
			} else {
				parts = append(parts, e.Message)
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	return errText
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// LoadScenarioDir walks dir for *.md scenario files and returns them keyed by their POSIX-relative path
// (so the cloud catalog mirrors the on-disk <layer>/ layout). Non-.md files, a top-level README.md, and
// HIDDEN dirs (R1b) are skipped. The result is sorted by path for a stable seed order.
func LoadScenarioDir(dir string) ([]ScenarioFile, error) {
	var out []ScenarioFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// R1b: skip hidden dirs (.claude, .git, …). The seed mounts the whole AGENT folder, which
			// carries .claude/skills/*/SKILL.md — installed-skill markdown that is NOT scenario content
			// and must never be seeded as a scenario. Never skip the walk root itself.
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.EqualFold(filepath.Ext(name), ".md") || strings.EqualFold(name, "README.md") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			rel = name
		}
		out = append(out, ScenarioFile{Path: filepath.ToSlash(rel), Content: string(b)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (c *CloudClient) mcpInit(ctx context.Context, token string) (string, error) {
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"argus-onboarding","version":"1"}}}`
	req, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/mcp", strings.NewReader(init))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HC.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &HTTPStatusError{Status: resp.StatusCode, Msg: fmt.Sprintf("mcp initialize: status %d", resp.StatusCode)}
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		return "", fmt.Errorf("mcp initialize: no session id")
	}
	return sid, nil
}

func (c *CloudClient) mcpPost(ctx context.Context, token, sid, body string) (int, []byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := c.HC.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// ClockDriftSeconds returns (local − control-plane) time in seconds, read from the CP's HTTP Date
// header — the onboarding clock preflight (UC055). A large drift (±) means the executor's short-lived
// federation JWTs may be rejected until the clock is fixed (it self-heals, but the user should know).
func (c *CloudClient) ClockDriftSeconds(ctx context.Context) (float64, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/healthz", nil)
	resp, err := c.HC.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	h := resp.Header.Get("Date")
	if h == "" {
		return 0, fmt.Errorf("no Date header from the control plane")
	}
	serverTime, err := http.ParseTime(h)
	if err != nil {
		return 0, fmt.Errorf("bad Date header %q: %w", h, err)
	}
	return time.Since(serverTime).Seconds(), nil
}

// ValidInstanceName enforces the instance-name charset (D-ONBOARD.4.4 / UC123): lowercase alphanumeric +
// hyphens (DNS/schema-safe), non-empty, no leading/trailing hyphen. Returns (ok, message-when-not).
func ValidInstanceName(name string) (bool, string) {
	if name == "" {
		return false, "instance name is empty"
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return false, "instance name must not start or end with a hyphen"
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false, fmt.Sprintf("instance name %q has an invalid character %q — use lowercase letters, digits, and hyphens only", name, string(r))
		}
	}
	return true, ""
}

// ── HTTP helpers (return the status so callers can distinguish pending / not-found from hard errors) ──

func (c *CloudClient) postForm(ctx context.Context, path string, form url.Values, out any) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, out)
}

func (c *CloudClient) postJSON(ctx context.Context, path string, body []byte, token string, out any) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, out)
}

func (c *CloudClient) getJSON(ctx context.Context, path, token string, out any) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, out)
}

// deleteJSON issues a DELETE with a bearer token — AC-D25's `DELETE /api/enrollments/{id}` is the
// only caller today, so this stays private rather than joining postJSON/getJSON as a third public verb.
func (c *CloudClient) deleteJSON(ctx context.Context, path, token string, out any) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, "DELETE", c.BaseURL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.do(req, out)
}

// do issues req under VR-F2b's retry policy — the single implementation in internal/retry.
//
// EVERY control-plane call onboarding makes arrives here: postForm, postJSON and getJSON are the
// only request builders, and the ~10 `cloudp cloud-*` invocations in onboard.sh are commands that go
// through them. So the retry lives HERE rather than as a loop in the shell, and that is not a
// convenience — the shell cannot see an HTTP status. A bash wrapper could only retry on "the command
// failed", which would hammer a 401 four times and wait 45 seconds to tell someone their token was
// rejected. Here the status is in hand and 4xx stops immediately.
//
// THE RETURN CONTRACT IS UNCHANGED, deliberately: callers inspect the status themselves, and a 5xx
// that never recovers still comes back as (500, nil) exactly as before. This adds attempts, not a new
// shape for callers to handle.
func (c *CloudClient) do(req *http.Request, out any) (int, error) {
	var (
		status int
		body   []byte
	)
	doRetry := c.Retry
	if doRetry == nil {
		doRetry = retry.Do
	}
	err := doRetry(req.Context(), func(ctx context.Context) error {
		r := req.Clone(ctx)
		// A retried POST needs its body again — the first attempt consumed the reader. GetBody is set
		// for the bytes/strings readers every builder above uses; without this rewind, attempt 2 would
		// send an EMPTY body and the CP would answer 400, turning a transient blip into a hard failure
		// that looks like a malformed request.
		if req.GetBody != nil {
			rb, gerr := req.GetBody()
			if gerr != nil {
				return retry.Fatal(gerr)
			}
			r.Body = rb
		}
		resp, derr := c.HC.Do(r)
		if derr != nil {
			status = 0
			return derr // transport / timeout — retryable
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		status = resp.StatusCode
		if retry.HTTPRetryable(status) {
			return fmt.Errorf("control plane returned %d", status)
		}
		return nil // 2xx AND 4xx are both final; the caller decides what the status means
	})
	if out != nil && len(body) > 0 {
		_ = json.Unmarshal(body, out)
	}
	if status == 0 {
		return 0, err // never reached the control plane at all
	}
	// a workspace-bound token is refused on everything onboarding does (it administers
	// the workspace). Say so at the FIRST refused call, in one place, instead of a bare "status 403".
	// The status stays in the error, so StatusOf-based callers see 403 exactly as before.
	if status == http.StatusForbidden {
		var refusal struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(body, &refusal) == nil && refusal.Reason == BoundTokenReason {
			return status, &HTTPStatusError{Status: status, Msg: BoundTokenMessage}
		}
	}
	return status, nil
}

// BoundTokenReason is the `reason` the control plane puts on a 403 for a workspace-bound token.
const BoundTokenReason = "workspace_bound_token"

// BoundTokenMessage is what the operator is told when onboarding or teardown is handed such a token.
const BoundTokenMessage = "the control plane refused this token: it is bound to one workspace and acts only inside it, " +
	"but onboarding and teardown administer the workspace (enroll executors, register routers, delete instances). " +
	"Use the owner's own sign-in (argus cloud-login) or a token for all workspaces, not one bound to a single workspace"

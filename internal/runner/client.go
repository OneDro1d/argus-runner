package runner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/mcp"
	"github.com/OneDro1d/argus-runner/internal/obslog"
	"github.com/OneDro1d/argus-runner/internal/retry"
)

// Transport selects how this Client reaches the control plane (AC-16 deliverable 3).
type Transport string

const (
	// TransportFed is the default, unchanged path: direct outbound-only HTTPS to /fed/*.
	TransportFed Transport = "fed"
	// TransportMCP reaches the SAME federation verbs as tools/call executor__<verb> against a hub
	// (or the control plane's own /mcp directly), bearing ARGUS_EXECUTOR_HUB_TOKEN plus the machine
	// JWT as the call's `envelope` argument. The outbox and the poll hold are unchanged either way.
	TransportMCP Transport = "mcp"
)

// Client is the executor's federation HTTP client — the outbound-only mirror of the CP's /fed/*
// endpoints (ADR-1). It signs every poll/push with the machine JWT (D-FED.2) and SELF-HEALS its clock:
// on any 401 carrying the CP's server_time it records the offset and compensates future JWTs, so a
// wrong laptop clock recovers with no user action.
type Client struct {
	baseURL    string
	instanceID string
	priv       ed25519.PrivateKey
	hc         *http.Client
	enrollment string // S1: the single-use first-register bearer (ARGUS_ENROLLMENT_TOKEN); "" after use / on restarts

	// Outbox, when set, makes ReportUp's terminal push DURABLE (F1). It lives on the Client rather
	// than the Executor because ReportUp has callers that never build an Executor — the MCP
	// runner__run seam and the `run` CLI — and those were exactly the ones still losing results
	// after the executor's own path had been fixed.
	Outbox *Outbox

	// Summary (UI-7a), when set, supplies the latest batch of summary readings that
	// Poll attaches. nil = this executor declares none; the poll body is then byte-identical to before.
	Summary *SummaryTicker

	mu     sync.Mutex
	offset time.Duration // added to local time to approximate CP time

	// hub, when non-nil, makes every verb below a tools/call of executor__<verb> against a
	// MCP-speaking hub instead of a direct HTTPS /fed/* request (AC-16 deliverable 3, TransportMCP).
	// nil (the zero value, from NewClient) keeps the unchanged fed behavior.
	hub *mcp.Client
}

// NewMCPClient builds a federation client that speaks EVERY verb below as a tools/call of
// executor__<verb> against mcpURL (a hub, or the control plane's /mcp directly) — the AC-16
// TransportMCP path. hubToken is the bearer the hub expects (ARGUS_EXECUTOR_HUB_TOKEN, the
// executor's own PAT per SY-11); the machine JWT still rides on every call, as the `envelope`
// argument, verified by the identical code path /fed/* verifies (control/executortools.go).
//
// The 40s per-call timeout matches the plain HTTP client's below: it comfortably holds the ~25s
// poll hold under a hub's documented 120s upstream timeout (router/config.go:18) without the
// executor ever appearing to hang past its own poll cadence.
func NewMCPClient(mcpURL, hubToken, instanceID string, priv ed25519.PrivateKey) *Client {
	return &Client{
		instanceID: instanceID,
		priv:       priv,
		hc:         &http.Client{Timeout: 40 * time.Second},
		hub: &mcp.Client{
			ServerURL: mcpURL, Transport: mcp.Streamable, Token: hubToken,
			Timeout: 40 * time.Second,
		},
	}
}

// SetEnrollment arms the S1 first-register credential (CP-M3-120). Call before Register; the token
// is single-use server-side, so restarts (which re-register via the machine JWT) leave it unset.
func (c *Client) SetEnrollment(tok string) { c.enrollment = tok }

// NewClient builds a federation client for instanceID against the CP at baseURL.
func NewClient(baseURL, instanceID string, priv ed25519.PrivateKey) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		instanceID: instanceID,
		priv:       priv,
		hc:         &http.Client{Timeout: 40 * time.Second}, // > the ~25s server hold
	}
}

func (c *Client) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *Client) setOffset(serverTime time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = time.Until(serverTime)
}

func (c *Client) mint() (string, error) { return federation.MintJWT(c.priv, c.instanceID, c.now()) }

// PublicKeyB64 renders this client's public key for the register wire.
func (c *Client) PublicKeyB64() string { return PublicKeyB64(c.priv) }

// --- TransportMCP: the shared tools/call plumbing (AC-16 deliverable 3) ---

// toArgs renders any federation wire struct as tools/call arguments — the same field names /fed/*
// decodes from the JSON body, since both sides share the one wire type.
func toArgs(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// remarshal re-encodes a decoded tool payload into a typed struct — used to read the SAME
// federation.RejectResponse shape /fed/* answers with, off a tools/call error payload.
func remarshal(m map[string]any, out any) {
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, out)
}

// callTool drives ONE executor__<verb> tools/call and decodes its content into out (nil = the
// caller only needs isError/payload). A transport failure (unreachable hub, handshake mismatch, a
// top-level JSON-RPC error) is a Go error; a tool-plane refusal (isError:true — including every
// distinguished federation reject) is reported via isError + payload so callers can read the SAME
// distinguished reasons /fed/* answers with.
func (c *Client) callTool(verb string, args map[string]any, out any) (isError bool, payload map[string]any, err error) {
	res := c.hub.Call(mcp.CallInput{Tool: "executor__" + verb, Args: args})
	if res.Unreachable {
		return false, nil, fmt.Errorf("%s: control-plane hub unreachable", verb)
	}
	if res.TransportErr != "" {
		return false, nil, fmt.Errorf("%s: %s", verb, res.TransportErr)
	}
	if res.JSONRPCError != nil {
		return false, nil, fmt.Errorf("%s: %s", verb, res.JSONRPCError.Message)
	}
	if len(res.Content) == 0 {
		return res.IsError, nil, nil
	}
	if res.IsError {
		var p map[string]any
		_ = json.Unmarshal([]byte(res.Content[0].Text), &p)
		return true, p, nil
	}
	if out != nil {
		if derr := json.Unmarshal([]byte(res.Content[0].Text), out); derr != nil {
			return false, nil, fmt.Errorf("%s: bad response: %w", verb, derr)
		}
	}
	return false, nil, nil
}

// payloadError renders a tool-error payload's "error" field as a message, falling back to the raw
// map when the field is absent or not a string.
func payloadError(payload map[string]any) string {
	if s, ok := payload["error"].(string); ok && s != "" {
		return s
	}
	return fmt.Sprintf("%v", payload)
}

// Register submits the registration (public key + metadata). No JWT — the onboarding/user session
// authorizes it (stubbed at I.1).
func (c *Client) Register(ctx context.Context, req federation.RegisterRequest) error {
	if c.hub != nil {
		return c.registerMCP(req)
	}
	body_, _ := json.Marshal(req)
	// VR-L2 (V17-020). The rule S1 states is right and the old code did not implement it: it sent
	// the enrollment WHENEVER IT HELD ONE — and onboarding mints one on every run, refresh included.
	// So every re-onboard of a live instance presented a first-register credential where the control
	// plane requires proof of the registered key, and was correctly refused with
	//   403 {"error":"re-register requires the machine JWT of the registered identity"}
	// which aborted the onboard AFTER registration, leaving it unrecoverable without a teardown.
	//
	// The client cannot know locally whether the control plane has seen this instance before. So it
	// PROVES ITS IDENTITY FIRST and falls back to the enrollment only when the control plane says a
	// first-register credential is what it wants. Deciding on the control plane's own answer, rather
	// than on anything about the local environment, is also why this fixes both tiers at once — the
	// k8s path had no separate mechanism, only a different accident.
	if tok, merr := c.mint(); merr == nil {
		code, body, derr := c.registerOnce(ctx, body_, tok)
		if derr != nil {
			return derr
		}
		if code == http.StatusOK {
			return nil
		}
		// Fall back ONLY on the control plane explicitly asking for an enrollment. Anything else —
		// a 500, a timeout, a different 403 — is a real answer and must surface as itself. Retrying
		// every failure with the other credential would turn a server error into a confusing auth
		// story, which is the shape of defect this round exists to remove.
		if !(code == http.StatusForbidden && c.enrollment != "" && strings.Contains(string(body), "enrollment credential")) {
			return fmt.Errorf("register: %d %s", code, body)
		}
	} else if c.enrollment == "" {
		return fmt.Errorf("register: no machine identity (%w) and no enrollment credential", merr)
	}
	code, body, derr := c.registerOnce(ctx, body_, c.enrollment)
	if derr != nil {
		return derr
	}
	if code != http.StatusOK {
		return fmt.Errorf("register: %d %s", code, body)
	}
	return nil
}

// registerMCP is Register's TransportMCP mirror: the SAME "prove identity first, fall back to the
// enrollment credential only when the control plane explicitly asks for one" rule as registerOnce/
// Register above (VR-L2, V17-020) — decided on the control plane's own answer, over executor__register.
func (c *Client) registerMCP(req federation.RegisterRequest) error {
	args := toArgs(req)
	if tok, merr := c.mint(); merr == nil {
		args["envelope"] = tok
		isErr, payload, err := c.callTool("register", args, nil)
		if err != nil {
			return err
		}
		if !isErr {
			return nil
		}
		if !(c.enrollment != "" && strings.Contains(payloadError(payload), "enrollment credential")) {
			return fmt.Errorf("register: %s", payloadError(payload))
		}
	} else if c.enrollment == "" {
		return fmt.Errorf("register: no machine identity (%w) and no enrollment credential", merr)
	}
	args["envelope"] = c.enrollment
	isErr, payload, err := c.callTool("register", args, nil)
	if err != nil {
		return err
	}
	if isErr {
		return fmt.Errorf("register: %s", payloadError(payload))
	}
	return nil
}

// registerOnce performs ONE register attempt with the given bearer and returns the status and body
// rather than an error, so the caller can distinguish "the control plane wants a different
// credential" from "the control plane failed".
// SetOnboardingState reports what onboarding achieved (VR-L5). Authenticated by the machine JWT.
func (c *Client) SetOnboardingState(ctx context.Context, state string) error {
	return c.SetOnboardingStateWithDashboard(ctx, state, "")
}

// SetOnboardingStateWithDashboard also carries VR5-O2's dashboard verdict, which travels on the same
// report because it is discovered during the same run.
//
// An EMPTY dashboardState means "no opinion" and the control plane leaves the stored value alone.
// That is what the first stamp sends — it happens immediately after registration, long before the
// Grafana wiring — and overwriting a real verdict with an empty one there would erase the answer.
func (c *Client) SetOnboardingStateWithDashboard(ctx context.Context, state, dashboardState string) error {
	if c.hub != nil {
		tok, err := c.mint()
		if err != nil {
			return err
		}
		args := toArgs(federation.OnboardingStateRequest{State: state, DashboardState: dashboardState})
		args["envelope"] = tok
		isErr, payload, err := c.callTool("onboarding_state", args, nil)
		if err != nil {
			return err
		}
		if isErr {
			return fmt.Errorf("onboarding-state: %s", payloadError(payload))
		}
		return nil
	}
	body, _ := json.Marshal(federation.OnboardingStateRequest{State: state, DashboardState: dashboardState})
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/onboarding-state", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	tok, err := c.mint()
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.hc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("onboarding-state: %d %s", resp.StatusCode, b)
	}
	return nil
}

func (c *Client) registerOnce(ctx context.Context, body []byte, bearer string) (int, []byte, error) {
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/register", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.hc.Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, b, nil
}

// Deregister removes THIS instance's cloud registration using the machine JWT (D-ONBOARD.6) — the
// non-interactive teardown: the machine identity IS the authorization (the SAME key that registered), so
// no Clerk/device-login is needed. Idempotent: a 401 unknown_instance (already gone / never registered) is
// treated as success, so re-running teardown is safe.
func (c *Client) Deregister(ctx context.Context) error {
	_, err := c.DeregisterWithKitDir(ctx)
	return err
}

// DeregisterOutcome is everything the de-register call reports back about what it just did.
//
// ⛔ IT NO LONGER CARRIES AutoTokenRevoked (VR10-T4-7). That field was a TRI-STATE reporting what the
// de-register had done to the ACCOUNT'S one auto token; 0.3.29 gives every machine its own token and
// an instance delete cannot name a machine, so the control plane makes no such decision and sends no
// such field. Keeping a reader for it would be worse than removing it: a value that is nil forever
// reads as "nobody said", and teardown would hedge on every run about a question that no longer
// exists. The fate of THIS machine's author token (minted during onboarding) comes from
// DELETE /api/routers/{id}, which revokes it in the same transaction as the registration.
type DeregisterOutcome struct {
	KitDir string
	// NotPresent means the control plane had NO registration for this instance, so this call
	// deleted nothing (VR8-V1 / V26-004).
	//
	// It exists because the idempotent path below already KNEW this and had no way to say it:
	// an unknown instance and a real de-registration both returned a zero outcome and a nil
	// error, so teardown printed "the name is free, the data is purged, the executor's tokens
	// are revoked" about an instance that had never existed. Idempotent is the right BEHAVIOUR
	// and was never in question; the defect was reporting it as the same OUTCOME.
	NotPresent bool
}

// DeregisterWithKitDir also returns the kit directory the control plane had recorded for this
// instance (VR5-T1), so teardown can scrub the credentials that were written there.
//
// It rides the DE-REGISTER response on purpose. De-register deletes the row, so any separate lookup
// would have to be sequenced before it — an ordering rule that must be got right at every call site
// forever. Returning it from the call that deletes makes the order structural.
//
// The kit path is returned even when de-register was a no-op (already gone); "" then means the
// control plane had nothing recorded, which teardown reports rather than treating as "nothing to do".
func (c *Client) DeregisterWithKitDir(ctx context.Context) (DeregisterOutcome, error) {
	tok, err := c.mint()
	if err != nil {
		return DeregisterOutcome{}, err
	}
	if c.hub != nil {
		var out struct {
			KitDir string `json:"kit_dir"`
		}
		isErr, payload, cerr := c.callTool("deregister", map[string]any{"envelope": tok}, &out)
		if cerr != nil {
			return DeregisterOutcome{}, cerr
		}
		if isErr {
			if reason, _ := payload["reason"].(string); reason == string(federation.ReasonUnknownInstance) {
				return DeregisterOutcome{NotPresent: true}, nil
			}
			return DeregisterOutcome{}, fmt.Errorf("deregister: %s", payloadError(payload))
		}
		return DeregisterOutcome{KitDir: out.KitDir}, nil
	}
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/deregister", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	resp, err := c.hc.Do(r)
	if err != nil {
		return DeregisterOutcome{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		var out struct {
			KitDir string `json:"kit_dir"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
		return DeregisterOutcome{KitDir: out.KitDir}, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusUnauthorized {
		var rej federation.RejectResponse
		if json.Unmarshal(b, &rej) == nil && rej.Reason == federation.ReasonUnknownInstance {
			// Idempotent: already deregistered / never registered. No kit path is available — the row
			// that held it is gone — and teardown must treat that as "could not find out", not as
			// "there was nothing to scrub". The token's fate is unknown here for the same reason: this
			// call deleted nothing, so nothing was decided about the account.
			//
			// VR8-V1: and NotPresent is how that last sentence finally leaves this function. It was
			// true here all along — the comment above has said so since this branch was written —
			// but a zero outcome is indistinguishable from a successful delete, so every caller had
			// to guess, and they all guessed "success".
			return DeregisterOutcome{NotPresent: true}, nil
		}
	}
	return DeregisterOutcome{}, fmt.Errorf("deregister: %d %s", resp.StatusCode, b)
}

// RegistrationPresent asks the control plane whether it STILL has a registration for this instance
// (AC-D61). It is the READ-BACK a teardown runs right after Deregister, with the SAME machine identity
// that just performed the delete — no new credential, no new scope, no loosened endpoint.
//
// It reuses the read-only `GET /fed/state` wire (and executor__state on the hub). The control plane
// authenticates the machine JWT BEFORE it reads any state: a row that is gone answers 401
// unknown_instance (fed.go authenticate), a row that is still there answers 200. So the answer is
// the control plane's own, not an inference from a delete's exit code.
//
// (false, nil) means GONE. (true, nil) means STILL REGISTERED. Any other outcome — an unreachable
// control plane, a 5xx, a bad signature, an unreadable body — is an ERROR: "I could not look" is not
// "it is gone", and the caller must not collapse them.
func (c *Client) RegistrationPresent(ctx context.Context) (bool, error) {
	tok, err := c.mint()
	if err != nil {
		return false, err
	}
	if c.hub != nil {
		isErr, payload, cerr := c.callTool("state", map[string]any{"envelope": tok}, nil)
		if cerr != nil {
			return false, cerr
		}
		if isErr {
			if reason, _ := payload["reason"].(string); reason == string(federation.ReasonUnknownInstance) {
				return false, nil
			}
			return false, fmt.Errorf("state: %s", payloadError(payload))
		}
		return true, nil
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/fed/state", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	resp, err := c.hc.Do(r)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		// A 200 that is not the state answer (an HTML sign-in page, a proxy) is not proof of anything.
		if !strings.Contains(string(b), "running_run_id") {
			return false, fmt.Errorf("state: 200 but not a state answer")
		}
		return true, nil
	case http.StatusUnauthorized:
		var rej federation.RejectResponse
		if json.Unmarshal(b, &rej) == nil && rej.Reason == federation.ReasonUnknownInstance {
			return false, nil
		}
	}
	return false, fmt.Errorf("state: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

// ErrUnknownInstance means the CP has no registration for this instance (D-FED.2): the executor must
// NOT exit — log once and drop to a slow retry so a later re-registration heals it.
var ErrUnknownInstance = fmt.Errorf("registration revoked — re-onboard or tear down this instance")

// Poll long-polls the CP for the next run. On a distinguished 401 it self-heals the clock and returns
// a retryable error (ErrUnknownInstance for a revoked registration; a generic error for expired/bad-sig
// after the offset is corrected).
func (c *Client) Poll(ctx context.Context, runnerVersion string, desired, total, ready, updated *int, pending []federation.PendingRun,
	sut federation.SUTObservation, moneyHandling *bool, moneyWritesAllow json.RawMessage, opts ...PollOption) (*federation.PollResponse, error) {
	// UI-7a: the latest summary batch rides this poll ONCE; a poll that fails hands it back so the next
	// one carries it again (a batch is small and idempotent -- the control plane keeps the newest).
	var summary []federation.SummaryReading
	if c.Summary != nil {
		summary = c.Summary.Take()
	}
	resp, err := c.pollWith(ctx, summary, runnerVersion, desired, total, ready, updated, pending, sut, moneyHandling, moneyWritesAllow, opts...)
	if err != nil && len(summary) > 0 {
		c.Summary.Unsend()
	}
	return resp, err
}

func (c *Client) pollWith(ctx context.Context, summary []federation.SummaryReading, runnerVersion string, desired, total, ready, updated *int, pending []federation.PendingRun,
	sut federation.SUTObservation, moneyHandling *bool, moneyWritesAllow json.RawMessage, opts ...PollOption) (*federation.PollResponse, error) {
	tok, err := c.mint()
	if err != nil {
		return nil, err
	}
	req := pollRequestFor(runnerVersion, desired, total, ready, updated, pending, sut, moneyHandling, moneyWritesAllow, opts...)
	req.SummaryReadings = summary
	if c.hub != nil {
		args := toArgs(req)
		args["envelope"] = tok
		var pr federation.PollResponse
		isErr, payload, cerr := c.callTool("poll", args, &pr)
		if cerr != nil {
			return nil, cerr
		}
		if isErr {
			var rej federation.RejectResponse
			remarshal(payload, &rej)
			if !rej.ServerTime.IsZero() {
				c.setOffset(rej.ServerTime)
			}
			if rej.Reason == federation.ReasonUnknownInstance {
				return nil, ErrUnknownInstance
			}
			return nil, fmt.Errorf("poll rejected: %s (clock compensated)", rej.Reason)
		}
		return &pr, nil
	}
	body, _ := json.Marshal(req)
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/poll", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	resp, err := c.hc.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		var rej federation.RejectResponse
		if json.Unmarshal(raw, &rej) == nil && !rej.ServerTime.IsZero() {
			c.setOffset(rej.ServerTime) // self-heal for the NEXT attempt
		}
		if rej.Reason == federation.ReasonUnknownInstance {
			return nil, ErrUnknownInstance
		}
		return nil, fmt.Errorf("poll rejected: %s (clock compensated)", rej.Reason)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("poll: %d %s", resp.StatusCode, raw)
	}
	var pr federation.PollResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, fmt.Errorf("poll: bad response: %w", err)
	}
	return &pr, nil
}

// Push uploads results (files-first at the caller; this is the network step). Idempotent by run_id at
// the CP, so a retry after a network blip never double-counts.
// ErrCPBusy is the DISTINGUISHED 409 from /fed/run/begin (the W1 fence, §D-3.1.2/UC188): a run —
// either path — is already in flight for this instance on the CP. The caller REFUSES the run and
// surfaces *busy*. Any other begin failure is a plain error (transport/CP-down), on which the caller
// may proceed offline — the documented CP-partition residual with the local lock as the second line.
var ErrCPBusy = errors.New("instance busy — a run is already in flight for this instance (control-plane fence)")

// BeginRun acquires the CP-side instance_run_lock for a DIRECT run before any SUT effect
// (POST /fed/run/begin under the machine JWT). nil = lock held (+ the CP now shows a 'running'
// ledger row under runID); ErrCPBusy = refuse; other errors = CP unreachable/failed (caller's call).
func (c *Client) BeginRun(ctx context.Context, runID, scope string) error {
	tok, err := c.mint()
	if err != nil {
		return err
	}
	if c.hub != nil {
		isErr, payload, cerr := c.callTool("run_begin", map[string]any{"envelope": tok, "run_id": runID, "scope": scope}, nil)
		if cerr != nil {
			return cerr
		}
		if isErr {
			if s, _ := payload["error"].(string); s == "instance_busy" {
				return ErrCPBusy
			}
			return fmt.Errorf("run begin: %s", payloadError(payload))
		}
		return nil
	}
	body, _ := json.Marshal(federation.RunBeginRequest{RunID: runID, Scope: scope})
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/run/begin", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, "run_"+runID) // ties the begin to the CP "direct run begun" log
	resp, err := c.hc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrCPBusy
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("run begin: %d %s", resp.StatusCode, b)
	}
}

// PushHeartbeat sends the minimal mid-run status='running' push (W1/3, §D-3.1.2/CP-M3-117): it bumps
// the CP-side updated_at (run_requests via rrID when federated; the run_ledger row either way), so
// the watchdog's HEARTBEAT-STALENESS cutoff never reaps a live long run — and a silent executor
// death is detected within one cutoff. Best-effort at every caller (a lost heartbeat is harmless;
// the next one re-arms the row). It also keeps the instance_run_lock (RecordResults releases on
// TERMINAL status only).
func (c *Client) PushHeartbeat(ctx context.Context, runID, rrID, scope string, startedAt time.Time) error {
	return c.Push(ctx, federation.ResultsPush{
		RunID: runID, RunRequestID: rrID, Scope: scope, Status: "running", StartedAt: &startedAt,
	})
}

func (c *Client) Push(ctx context.Context, push federation.ResultsPush) error {
	tok, err := c.mint()
	if err != nil {
		return err
	}
	if c.hub != nil {
		args := toArgs(push)
		args["envelope"] = tok
		isErr, payload, cerr := c.callTool("results", args, nil)
		if cerr != nil {
			return cerr
		}
		if isErr {
			var rej federation.RejectResponse
			remarshal(payload, &rej)
			if rej.Reason != "" {
				if !rej.ServerTime.IsZero() {
					c.setOffset(rej.ServerTime)
				}
				return fmt.Errorf("push rejected: %s", rej.Reason)
			}
			return fmt.Errorf("push: %s", payloadError(payload))
		}
		return nil
	}
	body, _ := json.Marshal(push)
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/results", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, "run_"+push.RunID) // ties this push to the CP "results recorded" log
	resp, err := c.hc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		var rej federation.RejectResponse
		if json.NewDecoder(resp.Body).Decode(&rej) == nil && !rej.ServerTime.IsZero() {
			c.setOffset(rej.ServerTime)
		}
		return fmt.Errorf("push rejected: %s", rej.Reason)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("push: %d %s", resp.StatusCode, b)
	}
	return nil
}

// CommandResult (AC-17) carries a relayed builder command's answer back — this executor's OWN push,
// exactly like Push above, never a control-plane-initiated call. result carries the verb's own
// (already custody-reduced) answer; errStr is set instead when the command could not be answered.
func (c *Client) CommandResult(ctx context.Context, commandID string, result json.RawMessage, errStr string) error {
	tok, err := c.mint()
	if err != nil {
		return err
	}
	push := federation.CommandResult{CommandID: commandID, Result: result, Error: errStr}
	if c.hub != nil {
		args := toArgs(push)
		args["envelope"] = tok
		isErr, payload, cerr := c.callTool("command_result", args, nil)
		if cerr != nil {
			return cerr
		}
		if isErr {
			return fmt.Errorf("command result: %s", payloadError(payload))
		}
		return nil
	}
	body, _ := json.Marshal(push)
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/command-result", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("command result: %d %s", resp.StatusCode, b)
	}
	return nil
}

// MarkDeployment posts an executor-emitted deployment marker to the CP (UC073/UC074), signed with the
// machine JWT like poll/push. `marker` is an opaque {at, commit?} JSON object.
func (c *Client) MarkDeployment(ctx context.Context, marker json.RawMessage) error {
	tok, err := c.mint()
	if err != nil {
		return err
	}
	if c.hub != nil {
		var markerVal any
		_ = json.Unmarshal(marker, &markerVal)
		isErr, payload, cerr := c.callTool("deployment", map[string]any{"envelope": tok, "marker": markerVal}, nil)
		if cerr != nil {
			return cerr
		}
		if isErr {
			return fmt.Errorf("mark-deployment: %s", payloadError(payload))
		}
		return nil
	}
	body, _ := json.Marshal(map[string]json.RawMessage{"marker": marker})
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/deployment", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	resp, err := c.hc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("mark-deployment: %d %s", resp.StatusCode, b)
	}
	return nil
}

// FetchSet asks the CP for a FRESH materialization of the current scenario set (the D-FED.3 direct-run
// source, UC059). The caller bounds it with a short-timeout ctx; a slow/absent CP surfaces as an error
// so the caller can fall back to the local cache.
func (c *Client) FetchSet(ctx context.Context, scope string, sel federation.Selection) (*federation.MaterializeResponse, error) {
	tok, err := c.mint()
	if err != nil {
		return nil, err
	}
	if c.hub != nil {
		var out federation.MaterializeResponse
		if err := c.catalogCallMCP(ctx, "materialize", tok, scope, sel, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	body, _ := json.Marshal(map[string]any{"scope": scope, "selection": sel})
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/materialize", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	var out federation.MaterializeResponse
	if err := c.catalogCall(ctx, r, "materialize", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrCatalogUnauthorized means the control plane REFUSED this executor's credential (401/403) rather
// than being unavailable (VR-F6).
//
// The distinction decides whether a direct run may offer to fall back to the local scenario set. An
// outage is temporary and the operator's own files are a reasonable stand-in; a rejected credential
// is not temporary, and running anyway would be running WITHOUT the authorisation that was just
// withdrawn. Same symptom at the call site, opposite correct response.
var ErrCatalogUnauthorized = errors.New("the control plane rejected this executor's credential")

// catalogCall issues a catalog request under VR-F2b's retry policy and decodes the response.
//
// The retry is what makes VR-F6's refusal honest: a direct run only offers the local set once the CP
// has been given the full policy — 4 attempts over 45 s — so "the control plane is unavailable" means
// it, rather than meaning one packet was dropped.
func (c *Client) catalogCall(ctx context.Context, req *http.Request, what string, out any) error {
	return retry.Do(ctx, func(actx context.Context) error {
		r := req.Clone(actx)
		if req.GetBody != nil {
			rb, gerr := req.GetBody()
			if gerr != nil {
				return retry.Fatal(gerr)
			}
			r.Body = rb
		}
		resp, err := c.hc.Do(r)
		if err != nil {
			return err // transport / timeout — worth another attempt
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			// 401/403 are FINAL and carry their own sentinel: repeating them cannot change the
			// answer, and the caller must not treat them as an outage.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return retry.Fatal(fmt.Errorf("%w: %s returned %d %s", ErrCatalogUnauthorized, what, resp.StatusCode, b))
			}
			err := fmt.Errorf("%s: %d %s", what, resp.StatusCode, b)
			if !retry.HTTPRetryable(resp.StatusCode) {
				return retry.Fatal(err)
			}
			return err
		}
		if derr := json.NewDecoder(resp.Body).Decode(out); derr != nil {
			// A 200 whose body will not parse is not a transport problem; asking again is unlikely to
			// produce different bytes, and pretending otherwise hides a real protocol mismatch.
			return retry.Fatal(fmt.Errorf("%s: bad response: %w", what, derr))
		}
		return nil
	})
}

// catalogCallMCP is catalogCall's TransportMCP mirror — the same VR-F2b policy (4 attempts, 1/3/8s
// waits, 45s budget), the same ErrCatalogUnauthorized distinction for a definitive federation reject,
// over a tools/call instead of a direct HTTPS request.
func (c *Client) catalogCallMCP(ctx context.Context, verb, envelope, scope string, sel federation.Selection, out any) error {
	args := map[string]any{"envelope": envelope, "scope": scope, "selection": sel}
	return retry.Do(ctx, func(context.Context) error {
		isErr, payload, err := c.callTool(verb, args, out)
		if err != nil {
			return err // transport failure — worth another attempt
		}
		if !isErr {
			return nil
		}
		if reason, _ := payload["reason"].(string); reason != "" {
			// a distinguished federation reject (bad_signature/expired/unknown_instance/malformed) is
			// DEFINITIVE, exactly like a 401/403 on the direct path — repeating it cannot change it.
			return retry.Fatal(fmt.Errorf("%w: %s returned %s", ErrCatalogUnauthorized, verb, reason))
		}
		return retry.Fatal(fmt.Errorf("%s: %s", verb, payloadError(payload)))
	})
}

// FetchSetHash asks ONLY whether the set has changed (VR-F6/INT-020), carrying no bodies.
//
// Same question as FetchSet, ~64 bytes of answer. A direct local run has to consult the catalog
// before executing — otherwise a scenario edited in the registry and then run locally executes the
// OLD copy on disk — and consulting on EVERY run is only affordable if the common "nothing changed"
// answer is cheap. Paying a full set transfer per fix-loop iteration is how a correctness check
// becomes something people switch off.
func (c *Client) FetchSetHash(ctx context.Context, scope string, sel federation.Selection) (string, error) {
	tok, err := c.mint()
	if err != nil {
		return "", err
	}
	if c.hub != nil {
		var out federation.SetHashResponse
		if err := c.catalogCallMCP(ctx, "set_hash", tok, scope, sel, &out); err != nil {
			return "", err
		}
		return out.SetHash, nil
	}
	body, _ := json.Marshal(map[string]any{"scope": scope, "selection": sel})
	r, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/fed/set-hash", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	var out federation.SetHashResponse
	if err := c.catalogCall(ctx, r, "set-hash", &out); err != nil {
		return "", err
	}
	return out.SetHash, nil
}

// pollRequestFor builds the poll payload. Extracted so its CONTENT is testable — INT-026 was a field
// nobody assigned inside an inline struct literal, and INT-029 was that same field assigned in the one
// place it could never be delivered from.
//
// EVERY executor-only fact belongs here, not on register. Register-on-start is refused with
//
//	403 {"error":"re-register requires the machine JWT of the registered identity"}
//
// for any instance the onboard already claimed — measured on COMPOSE as well as k3d/aks — so a field
// that rides only on register reaches the control plane exactly once, on a first onboard, and never
// again. The poll is the only carrier that runs continuously and is always authorised.
func pollRequestFor(runnerVersion string, desired, total, ready, updated *int, pending []federation.PendingRun,
	sut federation.SUTObservation, moneyHandling *bool, moneyWritesAllow json.RawMessage, opts ...PollOption) federation.PollRequest {
	req := federation.PollRequest{
		// T5.4 follow-up: see wire.go's PollRequest.MoneyHandling for the nil/
		// false/true rule. Set unconditionally (never gated like SUTReachable below) — nil already
		// means "say nothing", so there is no silence to preserve by omitting the assignment.
		MoneyHandling: moneyHandling,
		// money_writes follow-up: same unconditional treatment, one level down.
		MoneyWritesAllow: moneyWritesAllow,
		RunnerVersion:    runnerVersion, ProtocolVersion: federation.ProtocolVersion,
		// One image ships the binary and the suite in M3, so the suite version IS the runner version.
		// A test pins them together, making any future divergence a decision rather than an accident.
		SuiteVersion:    runnerVersion,
		ReplicasDesired: desired, ReplicasReady: ready, ReplicasUpdated: updated, ReplicasTotal: total,
		// U7 (CP-M3-III-83): read from the environment onboarding set, every poll. Cheap (three short
		// strings) and self-healing: an instance whose folders were lost, or which was registered
		// before this existed, repairs itself on the next poll instead of needing a re-onboard.
		ProductDir:  os.Getenv("ARGUS_PRODUCT_DIR_HOST"),
		KitDir:      os.Getenv("ARGUS_KIT_DIR_HOST"),
		TestDir:     os.Getenv("ARGUS_TEST_DIR_HOST"),
		OnboardHost: os.Getenv("ARGUS_ONBOARD_HOST"),
		// VR10-U1 (V28-020): the kube context and kubeconfig, by the same route and self-healing for
		// the same reason — an instance registered before 0.3.29 repairs itself on its next poll
		// rather than needing a re-onboard. Empty on compose; `omitempty` plus the store's
		// COALESCE(NULLIF(...)) means an executor that knows nothing cannot erase what the control
		// plane already has.
		KubeContext: os.Getenv("ARGUS_KUBE_CONTEXT_HOST"),
		Kubeconfig:  os.Getenv("ARGUS_KUBECONFIG_HOST"),
		// VR-F28: the runs that finished here and whose results are still queued. It rides on the
		// POLL for the reason stated above and one more that is specific to it — this fact EXISTS
		// only when the push path is failing, so the push path is the one carrier that could never
		// deliver it.
		ResultsPending: pending,
	}
	// VR6-W1, CORRECTED BY VR7-J1 (V24-001): THE TIMESTAMP IS THE CARRIER, NOT THE VERDICT.
	//
	// The original rule was "both halves or neither", and half of its reasoning still stands: a verdict
	// with no timestamp is a reachability the control plane can never age out, so a SUT that died a day
	// ago would stay green forever. That is why CheckedAt gates the pair.
	//
	// 🚨 The other half was wrong, and it silenced the feature. "A timestamp with no verdict says
	// nothing" is false — it says MEASURED, AND I COULD NOT TELL, which is a different fact from "nobody
	// has looked" and the exact distinction toolcore/reachability.go:167 exists to preserve:
	// "an operator must be able to tell 'checked and fine' from 'never checkable'".
	//
	// The control plane was already built for this payload and documents it at control/fed.go:306 —
	// "A REPORTED unknown (a timestamp with no verdict) does clear the column, which is deliberate."
	// Requiring a verdict here made that branch unreachable, so one layer produced the distinction and
	// the next destroyed it. Measured 2026-08-19: both OrderService instances reported NOTHING on two
	// tiers, while Social and Memstore reported fine on three between them.
	//
	// ⚠ An executor that has NOT probed still sends nothing — CheckedAt is nil then, and silence must
	// never overwrite a real measurement.
	if sut.CheckedAt != nil {
		req.SUTReachable, req.SUTCheckedAt = sut.Reachable, sut.CheckedAt
	}
	for _, o := range opts {
		o(&req)
	}
	return req
}

// State asks the control plane whether a run is in flight on this instance, and returns its run id
// ("" when none). VR9-C1's safety check.
//
// ── WHY THE EXECUTOR ASKS, RATHER THAN ANSWERING ──────────────────────────────────────────────────
//
// ⚠ THE CONTROL PLANE IS THE AUTHORITY, NOT THIS PROCESS. There is no currentRun/inFlight anywhere in
// internal/runner or internal/federation, so an executor answering from local state would be guessing —
// and the state where it matters most is exactly the one where the guess is wrong: an executor
// restarted mid-run holds nothing locally while the control plane's fence is still set.
//
// ⛔ AND IT ASKS WITH A CREDENTIAL IT ALREADY HAS. The update command the product renders carries none,
// and the three obvious ways to give it one are each forbidden — a --token VALUE in argv is SEC-4
// (world-readable via `ps`, replayed by `docker inspect`), an `export ARGUS_CP_AUTHOR_TOKEN=…`
// (formerly ARGUS_CP_TOKEN) printed beside it is a template the operator must repair (VR5-U3), and
// softening the guard is refused because
// it protects results computed and never reported. This mints from the machine identity already in this
// container's environment, so nothing new is passed in and nothing new is exposed.
func (c *Client) State(ctx context.Context) (string, error) {
	tok, err := c.mint()
	if err != nil {
		return "", err
	}
	if c.hub != nil {
		var out struct {
			RunningRunID *string `json:"running_run_id"`
		}
		isErr, payload, cerr := c.callTool("state", map[string]any{"envelope": tok}, &out)
		if cerr != nil {
			return "", cerr
		}
		if isErr {
			return "", fmt.Errorf("state: %s", payloadError(payload))
		}
		if out.RunningRunID == nil {
			return "", fmt.Errorf("state: the control plane answered without a running_run_id — this " +
				"build cannot tell whether a run is in flight, so it must not assume one is not")
		}
		return *out.RunningRunID, nil
	}
	// GET, deliberately: every other verb on /fed is an effect — run/begin ACQUIRES the very lock this
	// asks about and poll LEASES work — so asking through one of them would change the answer.
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/fed/state", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set(obslog.CorrelationHeader, obslog.NewID())
	resp, err := c.hc.Do(r)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return "", fmt.Errorf("state: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	// ⛔ A POINTER, SO A MISSING FIELD IS NOT AN EMPTY ONE. "" means "no run in flight" and PERMITS an
	// update; a control plane too old to serve this route, or a proxy answering 200 with something
	// else, must never be read as permission. An absent answer and an empty answer are different facts
	// and only one of them is safe to act on.
	var out struct {
		RunningRunID *string `json:"running_run_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("state: unreadable answer: %w", err)
	}
	if out.RunningRunID == nil {
		return "", fmt.Errorf("state: the control plane answered 200 without a running_run_id — this " +
			"build cannot tell whether a run is in flight, so it must not assume one is not")
	}
	return *out.RunningRunID, nil
}

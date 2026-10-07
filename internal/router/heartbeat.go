package router

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// The router's own identity and its heartbeat to the control plane (VR-R10).
//
// WHY THE ROUTER HAS A KEY RATHER THAN USING A TOKEN. A machine may hold ONLY product folders, which
// by construction carry no author-plane credential (VR-R4 — the table refuses to store one). If the
// heartbeat needed a token, silence would be the NORMAL state for exactly the folders where the
// holdout matters most. So onboarding registers this key once, while it legitimately holds a token,
// and the router reports with the key forever after.

const identityFile = "identity.key"

// Identity is the router's Ed25519 machine key. The private half never leaves this machine — the
// control plane stores only the public half, at registration.
type Identity struct {
	RouterID string
	Priv     ed25519.PrivateKey
}

// IdentityHolder publishes the router's CURRENT identity from whatever goroutine notices a change to
// the beat loop that signs with it.
//
// ⛔ IT EXISTS BECAUSE THE ALTERNATIVE IS AN INVISIBLE RACE. VR9-I1 requires the identity to be
// re-readable while the router runs, so one goroutine writes it and another reads it every beat. A
// plain variable there is a data race whose symptom is not a crash but a heartbeat signed with a stale
// or torn value — i.e. the exact silent 401 this requirement exists to remove.
//
// ⚠ IT LIVES HERE, NOT IN package main, DELIBERATELY. It was written in cmd/argus, where nothing
// can test it: the ONE defect VR9-I1 is about was a wiring defect, and putting the wiring somewhere
// untestable is how the defect survived. TestRouterServe_ANewKeyOnDiskReachesTheNextBeat composes this
// with WatchIdentity and RunHeartbeat exactly as `router serve` does, which is only possible from
// inside the package.
type IdentityHolder struct {
	v atomic.Pointer[Identity]
}

// NewIdentityHolder seeds a holder with the identity read at start-up.
func NewIdentityHolder(id Identity) *IdentityHolder {
	h := &IdentityHolder{}
	h.Store(id)
	return h
}

// Store publishes a new identity. Safe to call from the watcher goroutine.
func (h *IdentityHolder) Store(id Identity) { h.v.Store(&id) }

// Load returns the identity to sign the next beat with. The zero holder returns an empty Identity
// rather than panicking: a beat signed by nothing is refused by the control plane, which is a far
// better failure than taking down the router the beat is only reporting on.
func (h *IdentityHolder) Load() Identity {
	if p := h.v.Load(); p != nil {
		return *p
	}
	return Identity{}
}

// LoadOrCreateIdentityCreated is LoadOrCreateIdentity plus the one fact its caller cannot otherwise
// learn: whether this call MINTED the identity or merely loaded one that was already there.
//
// ⚠ VR9-I1 rule 2. Without it, onboarding cannot tell a fresh mint from a load — and that is exactly
// what decides whether a router already running against this directory is now signing with a key the
// control plane has never seen.
func LoadOrCreateIdentityCreated(dir string) (Identity, bool, error) {
	_, statErr := os.Stat(filepath.Join(dir, identityFile))
	existed := statErr == nil
	id, err := LoadOrCreateIdentity(dir)
	return id, !existed && err == nil, err
}

// LoadOrCreateIdentity returns the machine's router identity, minting one on first run.
//
// The router id is derived from the PUBLIC key rather than randomly generated, so the id and the key
// cannot drift apart: a state directory can never hold an id that its key does not correspond to,
// which would make every heartbeat fail with "the signed subject does not match".
// LoadIdentity reads an existing router identity and NEVER creates one (VR7-T1 / V24-003).
//
// LoadOrCreateIdentity is the right call on a path that is entitled to mint — `router serve` owns the
// machine's identity and creating it is its job. A READ is not entitled to that. `router status` is
// run by teardown against a state directory it is in the middle of dismantling, and minting an
// Ed25519 private key there would leave a credential behind on the machine teardown just cleaned.
//
// Returns fs.ErrNotExist when there is no identity, so a caller can say "no router here" rather than
// emitting an empty id — which a shell reading `cut -d'"' -f4` cannot tell apart from success.
func LoadIdentity(dir string) (Identity, error) {
	path := filepath.Join(dir, identityFile)
	blob, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, err
	}
	raw, derr := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(blob)))
	if derr != nil || len(raw) != ed25519.PrivateKeySize {
		return Identity{}, fmt.Errorf("router: %s is not a valid Ed25519 private key", path)
	}
	priv := ed25519.PrivateKey(raw)
	return Identity{RouterID: routerIDFor(priv.Public().(ed25519.PublicKey)), Priv: priv}, nil
}

func LoadOrCreateIdentity(dir string) (Identity, error) {
	path := filepath.Join(dir, identityFile)
	blob, err := os.ReadFile(path)
	if err == nil {
		raw, derr := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(blob)))
		if derr != nil || len(raw) != ed25519.PrivateKeySize {
			return Identity{}, fmt.Errorf("router: %s is not a valid Ed25519 private key — delete it to mint a fresh identity, then re-register the router", path)
		}
		priv := ed25519.PrivateKey(raw)
		return Identity{RouterID: routerIDFor(priv.Public().(ed25519.PublicKey)), Priv: priv}, nil
	}
	if !os.IsNotExist(err) {
		return Identity{}, err
	}
	pub, priv, gerr := ed25519.GenerateKey(rand.Reader)
	if gerr != nil {
		return Identity{}, gerr
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return Identity{}, err
	}
	return Identity{RouterID: routerIDFor(pub), Priv: priv}, nil
}

// routerIDFor derives a stable id from the public key. Short enough to read in a log line, long
// enough not to collide across the handful of machines one user has.
func routerIDFor(pub ed25519.PublicKey) string {
	return "rtr_" + base64.RawURLEncoding.EncodeToString(pub)[:16]
}

// HostEnv lets the deployment name the MACHINE. Inside a container os.Hostname() returns the
// CONTAINER ID, which is not the machine and does not survive a recreate.
const HostEnv = "ARGUS_ROUTER_HOST"

// HostName is the machine this router runs on, as the control plane should record it.
//
// MEASURED 2026-08-09, and it broke the heartbeat outright. `router register` ran in one container
// and recorded host=195f0b409198; the router container heartbeated as host=c036ec382a5b. Same
// router_id, different host — so the upsert's INSERT hit the router_id PRIMARY KEY while its
// `ON CONFLICT (owner_user, host)` arbiter matched no row, and the control plane answered 500 on
// every beat. The Environments page showed "stale", which VR-R10 defines as display-only, so a hard
// server error wore the costume of a router that simply had not spoken yet.
//
// VR-R1 is "one router per MACHINE per user". A container id is neither stable nor the machine, so
// the deployment declares it.
func HostName() string {
	if h := strings.TrimSpace(os.Getenv(HostEnv)); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

// PublicKeyB64 is what onboarding sends to POST /api/routers/register.
func (i Identity) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(i.Priv.Public().(ed25519.PublicKey))
}

// countOrUnknown calls a count callback, or reports -1 when this build has none wired.
//
// ⛔ The zero value is NOT the answer for an absent callback. See Heartbeat.HolderCount.
func countOrUnknown(f func() int) int {
	if f == nil {
		return -1
	}
	return f()
}

// Heartbeat is one report to the control plane.
type Heartbeat struct {
	// Identity is the key each beat is signed with, read AFRESH on every beat.
	//
	// ⛔ A FIELD, NOT AN ARGUMENT, AND THAT IS THE WHOLE POINT. It used to be the second parameter of
	// RunHeartbeat, which meant the CALL SITE could pass a constant — and an adversary gate did
	// exactly that, restoring V27-003 with the entire suite green, because every test drove the
	// assembly function rather than the call. There is now no second argument to swap: a caller that
	// wants the wrong identity has to build a different Heartbeat, which the wiring test asserts on.
	//
	// ⚠ A PROVIDER, NOT A VALUE. `router register` mints a new identity.key under this running
	// process; a value read once keeps signing with the start-up key and is answered 401 in silence.
	Identity func() Identity
	CPURL    string
	Host     string
	Version  string
	Port     int
	HTTP     *http.Client
	// OnRotatedToken is called when the control plane hands over a REPLACEMENT author token
	// (VR-B4/VR-B6). It must persist the new token for every folder that holds one and return nil
	// only when that has actually happened — the acknowledgement it triggers is what revokes the old
	// token, so a false success is an outage on the next heartbeat.
	//
	// Nil means "this router does not hold author tokens", which is legitimate: the offer is simply
	// left un-acknowledged, the OLD token keeps working, and the control plane flags the rotation as
	// stalled after a week rather than breaking anything.
	OnRotatedToken func(token string) error

	// RecordURL + Owner name the record this loop beats for (V27-009); they key its health sidecar.
	RecordURL string
	Owner     string
	// OnOwner receives the `owner` the control plane echoes; the router fills a nameless record with it.
	OnOwner func(owner string)

	// HolderCount reports how many agent folders on this machine carry a cloud entry — the number of
	// places a replacement token could actually BE USED. TestFolderCount reports how many test-hat
	// folders exist at all, holding or not.
	//
	// ⚠ CALLBACKS, NOT ints, and that is load-bearing. RunHeartbeat takes its Heartbeat BY VALUE, so a
	// plain field would be captured at start-up and report the count the machine had when the router
	// booted. That is VR9-I1's own defect — a value read once and never re-read — rebuilt somewhere new.
	//
	// ⛔ nil MUST report -1, never 0. -1 says "this build does not know"; 0 says "it holds none" and is
	// VR9-H4's recovery trigger. The planes ship separately, so a router that predates these fields is
	// an ordinary state, and calling it a zero-holder machine would withhold rotation estate-wide.
	HolderCount     func() int
	TestFolderCount func() int
	// StateDir is where the beat outcome is recorded (VR9-I1 rule 4's sidecar). Empty means "do not
	// record" — legitimate for callers that only want to send, and for tests.
	StateDir string

	// Interval overrides HeartbeatInterval. Zero means the real one. It exists so the composition of
	// watcher + holder + beat loop can be driven end-to-end in a test; a package-level var would have
	// done the same job by mutating global state across tests, which is worse.
	Interval time.Duration
	// pendingAck is the outgoing token id to acknowledge on the NEXT heartbeat. Deliberately sent on
	// the following beat rather than in the same exchange: the acknowledgement must mean "it is on
	// disk", and the write happens after the response has been read.
	pendingAck string
}

// Send reports once. It returns an error the CALLER is expected to log and ignore: a heartbeat that
// does not land changes nothing about whether the router works, and VR-R10 makes a stale heartbeat
// display-only, never blocking. An agent whose router is running does not care what the cloud
// believes, and one whose router is down finds out from its own call.
// POINTER RECEIVER, and it matters: Send REMEMBERS the acknowledgement it owes on the next beat
// (pendingAck). With a value receiver that write lands on a copy and is lost, so the rotation would
// be taken, stored, and never acknowledged — the old token would linger until the stalled sweep
// flagged it a week later, and every heartbeat would look successful throughout.
func (h *Heartbeat) Send(id Identity, now time.Time) error {
	jwt, err := federation.MintJWT(id.Priv, id.RouterID, now)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"router_id": id.RouterID, "host": h.Host, "version": h.Version, "port": h.Port, "jwt": jwt,
		// ALWAYS sent, and always a number. An omitted key and a -1 mean the same thing to the control
		// plane, but sending it explicitly keeps the wire self-describing.
		"holder_count":      countOrUnknown(h.HolderCount),
		"test_folder_count": countOrUnknown(h.TestFolderCount),
	}
	if h.pendingAck != "" {
		payload["rotation_ack"] = h.pendingAck
	}
	if h.Owner != "" {
		payload["owner"] = h.Owner
	}
	body, _ := json.Marshal(payload)
	cl := h.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequest(http.MethodPost, h.CPURL+"/fed/router/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// VR9-I1 rule 4 — CLASSIFY, do not stringify. This used to return a prose error carrying only
		// the status code, so the caller could not tell a control-plane DECISION about this machine's
		// identity (401) from a network event that merely arrived over HTTP (503 during a rollout) —
		// and therefore acted on neither. The body is read for the machine-readable `reason`; two of
		// the three 401 branches on an OLDER control plane do not send one, which is an ordinary
		// rollout state and leaves Reason empty rather than being an error in itself.
		blob, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		var wire struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(blob, &wire)
		return &RejectedError{Status: res.StatusCode, Reason: wire.Reason, Body: strings.TrimSpace(string(blob))}
	}
	// The ack was accepted (the CP answered 200), so stop repeating it. Doing this only on success
	// means a dropped heartbeat re-sends it rather than silently forgetting — and CompleteAutoRotation
	// is idempotent precisely so a re-send costs nothing.
	h.pendingAck = ""

	var out struct {
		RotateAuthorToken *struct {
			Token      string `json:"token"`
			OldTokenID string `json:"old_token_id"`
		} `json:"rotate_author_token"`
		// VR9-H4 — ITS OWN DECODE, deliberately. The control plane guarantees the two verbs are
		// mutually exclusive, but they mean opposite things about what this router owes afterwards, so
		// they are never folded into one field.
		Owner string `json:"owner"`
	}
	if derr := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); derr != nil {
		return nil // a body we cannot parse is not a reason to fail a heartbeat
	}

	if out.RotateAuthorToken != nil {
		if h.OnRotatedToken == nil {
			// No holder on this router. Say nothing and acknowledge nothing: the OLD token stays valid
			// and the control plane will flag the rotation stalled, which is the honest outcome.
			return nil
		}
		if err := h.OnRotatedToken(out.RotateAuthorToken.Token); err != nil {
			// NOT acknowledged. The old token keeps working — VR-B6 — and the next heartbeat retries.
			return fmt.Errorf("heartbeat: a rotated author token could not be stored, so it was NOT acknowledged and the old one stays valid: %w", err)
		}
		h.pendingAck = out.RotateAuthorToken.OldTokenID
		return nil
	}

	if out.Owner != "" && h.Owner == "" && h.OnOwner != nil {
		h.OnOwner(out.Owner)
		h.Owner = out.Owner
	}
	return nil
}

// HeartbeatInterval is how often the router reports. Comfortably inside store.RouterStaleAfter
// (3 minutes) so a single missed report does not make a healthy router look silent — a status that
// flaps teaches people to ignore it.
const HeartbeatInterval = 60 * time.Second

// RunHeartbeat reports every interval until stop is closed. It NEVER returns an error to its caller:
// the router's job is routing, and a control plane that cannot be reached must not take the machine's
// agents down with it.
// ⚠ IT TAKES A PROVIDER, NOT AN IDENTITY, AND THAT IS THE WHOLE OF VR9-I1. `router serve` used to read
// the identity once and pass it here by value, so when `router register` minted a new identity.key
// underneath a running server every later beat was still signed with the OLD key. The control plane
// answered 401 to all of them — silently, because a beat that does not land is deliberately non-fatal —
// and no token rotation could reach that machine again until it was restarted.
//
// The provider is called ON EVERY BEAT. Anything captured in the Heartbeat value freezes at start-up.
func RunHeartbeat(h Heartbeat, stop <-chan struct{}, onErr func(error)) {
	// ONE Heartbeat value for the life of the loop, so the ack it owes survives between beats.
	hb := &h
	if h.CPURL == "" {
		return // no control plane configured for this machine; nothing to report to
	}
	every := h.Interval
	if every <= 0 {
		every = HeartbeatInterval
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		now := time.Now()
		err := hb.Send(hb.Identity(), now)
		// VR9-I1 rule 4 — RECORD EVERY OUTCOME, including the good ones: an accepted beat is what
		// RESETS the streak, so skipping it here would turn the counter into a lifetime tally and
		// escalate on three unrelated blips spread over a month.
		//
		// ⛔ The recorder's own failure is swallowed on purpose. It writes a DIAGNOSTIC; a router that
		// cannot write its health file still routes, and taking the beat loop down over it would make
		// the reporting more dangerous than the thing it reports on.
		if hb.StateDir != "" {
			if health, rerr := RecordBeatOutcome(hb.StateDir, hb.RecordURL, hb.Owner, err, now); rerr == nil {
				// Say it out loud as well as on disk. `router status` is a question someone has to
				// think to ask, and nobody asks it about a machine they believe is fine.
				if msg := health.EscalationMessage(); msg != "" && onErr != nil {
					onErr(errors.New(msg))
				}
			}
		}
		if err != nil && onErr != nil {
			onErr(err)
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

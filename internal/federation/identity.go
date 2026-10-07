package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The executor's machine identity (D-FED.2): a per-instance Ed25519 keypair generated locally; the
// private key never leaves the environment. Per federation call the executor mints a short-lived
// (~60s) EdDSA JWT signed with that key; the CP verifies it against the registered public key, within
// a ±300s clock-skew window. Hand-rolled (no JWT dependency) so the skew window and the DISTINGUISHED
// rejection reasons are exactly what D-FED.2 specifies.

// JWTLifetime is the mint-time validity of an executor call JWT.
const JWTLifetime = 60 * time.Second

// SkewWindow is the accepted clock-skew tolerance on iat/exp (D-FED.2: ±300s).
const SkewWindow = 300 * time.Second

// RejectReason is the DISTINGUISHED reason a federation credential was refused (D-FED.2). It rides
// every 401/403 so the executor self-heals instead of dying mysteriously.
type RejectReason string

const (
	// ReasonUnknownInstance — no registration for this instance (e.g. torn down). The executor must
	// NOT exit: log once and drop to a slow retry so a later re-registration heals it.
	ReasonUnknownInstance RejectReason = "unknown_instance"
	// ReasonBadSignature — the JWT signature did not verify against the registered pubkey.
	ReasonBadSignature RejectReason = "bad_signature"
	// ReasonExpired — the JWT iat/exp is outside the skew-compensated window.
	ReasonExpired RejectReason = "expired"
	// ReasonMalformed — the token could not be parsed as an EdDSA JWT.
	ReasonMalformed RejectReason = "malformed"
)

// Errors carrying a RejectReason, so callers map to the right 401/403 body.
var (
	ErrBadSignature = &RejectError{Reason: ReasonBadSignature}
	ErrExpired      = &RejectError{Reason: ReasonExpired}
	ErrMalformed    = &RejectError{Reason: ReasonMalformed}
)

// RejectError wraps a RejectReason.
type RejectError struct{ Reason RejectReason }

func (e *RejectError) Error() string { return "federation credential rejected: " + string(e.Reason) }

var b64 = base64.RawURLEncoding

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// Claims are the executor JWT claims (machine identity — sub = instance_id, no delegated user).
type Claims struct {
	Sub string `json:"sub"` // instance_id
	Iat int64  `json:"iat"` // issued-at (unix seconds)
	Exp int64  `json:"exp"` // expiry (unix seconds)
	// Jti is the S9 replay nonce (§D-3.1.4/CP-M3-123): the 60s exp + ±300s skew acceptance window
	// (~11 min) would otherwise let a captured JWT replay DESTRUCTIVE verbs (/fed/deregister
	// cascade, /fed/rotate). Minted always; the CP enforces single-use on the destructive verbs.
	Jti string `json:"jti,omitempty"`
}

// MintJWT signs a ~60s EdDSA JWT for instance_id with priv, issued at now.
func MintJWT(priv ed25519.PrivateKey, instanceID string, now time.Time) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("mint: bad private key size %d", len(priv))
	}
	hdr, _ := json.Marshal(jwtHeader{Alg: "EdDSA", Typ: "JWT"})
	var jb [8]byte
	_, _ = rand.Read(jb[:])
	cl, _ := json.Marshal(Claims{Sub: instanceID, Iat: now.Unix(), Exp: now.Add(JWTLifetime).Unix(), Jti: hex.EncodeToString(jb[:])})
	signing := b64.EncodeToString(hdr) + "." + b64.EncodeToString(cl)
	sig := ed25519.Sign(priv, []byte(signing))
	return signing + "." + b64.EncodeToString(sig), nil
}

// VerifyJWT verifies token against pub at time now, tolerating ±SkewWindow on iat/exp. It returns the
// claims on success, or a *RejectError with the distinguished reason. Note: unknown_instance is NOT
// decided here — that is the caller's pubkey lookup; VerifyJWT is given the pubkey it must check.
func VerifyJWT(token string, pub ed25519.PublicKey, now time.Time) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(pub) != ed25519.PublicKeySize {
		return nil, ErrMalformed
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var hdr jwtHeader
	if err := json.Unmarshal(hb, &hdr); err != nil || hdr.Alg != "EdDSA" {
		return nil, ErrMalformed
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, ErrMalformed
	}
	// Signature first (constant-work verify; a valid sig is required before we trust the claims).
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, ErrBadSignature
	}
	var cl Claims
	if err := json.Unmarshal(cb, &cl); err != nil {
		return nil, ErrMalformed
	}
	// Skew-compensated window: reject if now is before iat-skew or after exp+skew.
	iat := time.Unix(cl.Iat, 0)
	exp := time.Unix(cl.Exp, 0)
	if now.Before(iat.Add(-SkewWindow)) || now.After(exp.Add(SkewWindow)) {
		return nil, ErrExpired
	}
	return &cl, nil
}

// PeekSubject decodes the UNVERIFIED subject (instance_id) claim from a JWT — used only to look up the
// registered pubkey the token must then be verified against. The signature is not trusted here; nothing
// acts on this value except the pubkey lookup. Returns ErrMalformed on a non-JWT.
func PeekSubject(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ErrMalformed
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return "", ErrMalformed
	}
	var cl Claims
	if err := json.Unmarshal(cb, &cl); err != nil || cl.Sub == "" {
		return "", ErrMalformed
	}
	return cl.Sub, nil
}

// AsRejectReason extracts the RejectReason from an error returned by VerifyJWT, or "" if none.
func AsRejectReason(err error) RejectReason {
	var re *RejectError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ""
}

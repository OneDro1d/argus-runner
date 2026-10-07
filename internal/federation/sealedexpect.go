package federation

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// sealedexpect.go — AC-4: EXPECT sealed on the wire under a run-scoped key, standard library only
// (crypto/ecdh X25519, crypto/hkdf, crypto/cipher AES-256-GCM, crypto/sha256). Design: for each
// assignment the control plane encrypts a scenario's body under a key derived from
// HKDF(X25519(cp-ephemeral, executor-pub), salt=run_id, info="argus expect v1") and ships
// {ciphertext, nonce, cp_ephemeral_pub} in place of the clear text (ScenarioPayload.Sealed); the
// executor decrypts in memory at execution with its own private key and the same run_id, and hands the
// parser exactly the author's text. Binding the key to run_id (the HKDF salt) means a ciphertext sealed
// for one run cannot be opened under another run's id, even by the correct executor.

// SealedBody is EXPECT's on-the-wire shape once sealed (AC-4). Body and Sealed are mutually exclusive on
// ScenarioPayload — a payload with Sealed set carries no clear EXPECT text (see
// TestScenarioPayload_SealedCarriesNoClearBody).
type SealedBody struct {
	Ciphertext     string `json:"ciphertext"`       // base64 (std), AES-256-GCM sealed body
	Nonce          string `json:"nonce"`            // base64 (std), 12 bytes
	CPEphemeralPub string `json:"cp_ephemeral_pub"` // base64 (std), 32-byte X25519 public key
}

// hkdfInfo is fixed per the design — a version tag so a future wire shape cannot silently derive the
// SAME key under a changed framing.
const hkdfInfo = "argus expect v1"

// SealBody encrypts plaintext (a scenario's body, EXPECT included) for ONE executor's X25519 public key,
// under a key scoped to runID. A fresh control-plane ephemeral X25519 keypair is generated per call —
// two scenarios in the same assignment, and any two assignments, never reuse an ECDH secret.
func SealBody(executorPub *ecdh.PublicKey, runID, plaintext string) (SealedBody, error) {
	cpPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return SealedBody{}, fmt.Errorf("sealedexpect: generate ephemeral key: %w", err)
	}
	shared, err := cpPriv.ECDH(executorPub)
	if err != nil {
		return SealedBody{}, fmt.Errorf("sealedexpect: ecdh: %w", err)
	}
	gcm, err := gcmFor(shared, runID)
	if err != nil {
		return SealedBody{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return SealedBody{}, fmt.Errorf("sealedexpect: nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return SealedBody{
		Ciphertext:     base64.StdEncoding.EncodeToString(ct),
		Nonce:          base64.StdEncoding.EncodeToString(nonce),
		CPEphemeralPub: base64.StdEncoding.EncodeToString(cpPriv.PublicKey().Bytes()),
	}, nil
}

// OpenBody reverses SealBody: the executor's OWN X25519 private key, plus the SAME runID the seal used
// (the assignment's run_id), recovers the plaintext exactly as the author wrote it. A wrong runID or a
// wrong private key derives the wrong AES key, and AES-GCM's tag check fails closed — no partial or
// garbled plaintext is ever returned.
func OpenBody(executorPriv *ecdh.PrivateKey, sealed SealedBody, runID string) (string, error) {
	pubBytes, err := base64.StdEncoding.DecodeString(sealed.CPEphemeralPub)
	if err != nil {
		return "", fmt.Errorf("sealedexpect: bad cp_ephemeral_pub: %w", err)
	}
	cpPub, err := ecdh.X25519().NewPublicKey(pubBytes)
	if err != nil {
		return "", fmt.Errorf("sealedexpect: bad cp_ephemeral_pub: %w", err)
	}
	shared, err := executorPriv.ECDH(cpPub)
	if err != nil {
		return "", fmt.Errorf("sealedexpect: ecdh: %w", err)
	}
	gcm, err := gcmFor(shared, runID)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(sealed.Nonce)
	if err != nil {
		return "", fmt.Errorf("sealedexpect: bad nonce: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(sealed.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("sealedexpect: bad ciphertext: %w", err)
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", errors.New("sealedexpect: open failed — wrong key, wrong run_id, or tampered ciphertext")
	}
	return string(pt), nil
}

// gcmFor derives the AES-256-GCM cipher for one ECDH shared secret, scoped to runID via the HKDF salt.
func gcmFor(shared []byte, runID string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, shared, []byte(runID), hkdfInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("sealedexpect: hkdf: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("sealedexpect: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("sealedexpect: gcm: %w", err)
	}
	return gcm, nil
}

// SealAssignmentScenarios seals every scenario's Body for executorPub, run-scoped to runID, clearing
// Body and setting Sealed on each. executorPub == nil (the executor has not registered an X25519 key)
// leaves every scenario UNCHANGED — EXPECT ships in the clear exactly as it did before this ticket,
// never a fault, never refused.
func SealAssignmentScenarios(executorPub *ecdh.PublicKey, runID string, scenarios []ScenarioPayload) ([]ScenarioPayload, error) {
	if executorPub == nil {
		return scenarios, nil
	}
	out := make([]ScenarioPayload, len(scenarios))
	for i, sc := range scenarios {
		sealed, err := SealBody(executorPub, runID, sc.Body)
		if err != nil {
			return nil, err
		}
		out[i] = ScenarioPayload{Path: sc.Path, Sealed: &sealed}
	}
	return out, nil
}

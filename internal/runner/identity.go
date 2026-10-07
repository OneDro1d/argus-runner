package runner

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

// The executor's machine identity (D-FED.2): a per-instance Ed25519 keypair generated LOCALLY, whose
// private key never leaves the environment (compose: a file beside the local tokens; k8s: a Secret).
// Only the public key is submitted at registration.

// LoadOrCreateKey loads the Ed25519 private key from path (raw seed/private bytes), or generates and
// persists a fresh one (0600) if absent. Returns the private key.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("identity: %s is not a %d-byte Ed25519 key (got %d)", path, ed25519.PrivateKeySize, len(b))
		}
		return ed25519.PrivateKey(b), nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	_, priv, gerr := ed25519.GenerateKey(nil)
	if gerr != nil {
		return nil, gerr
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return nil, mkErr
	}
	if wErr := os.WriteFile(path, priv, 0o600); wErr != nil {
		return nil, fmt.Errorf("identity: persist %s: %w", path, wErr)
	}
	return priv, nil
}

// LoadKey loads an EXISTING Ed25519 private key from path (load-only — it NEVER creates one, unlike
// LoadOrCreateKey). The in-env MCP server (argus serve) uses it to sign a direct-run results push as
// the SAME registered machine as the executor: both containers share the identity file on the results
// volume, and only the executor creates it. Returns an error if the key is absent (e.g. the executor has
// not started yet) so the caller can skip the best-effort push instead of minting a divergent identity.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: %s is not a %d-byte Ed25519 key (got %d)", path, ed25519.PrivateKeySize, len(b))
	}
	return ed25519.PrivateKey(b), nil
}

// PublicKeyB64 renders the public half as standard base64 for the register wire (RegisterRequest).
func PublicKeyB64(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// GenerateKeyB64 mints a fresh Ed25519 private key and returns it as standard base64 of the raw
// 64-byte private key — the exact bytes LoadKey/LoadOrCreateKey read from disk. Used by onboarding
// (the `keygen` subcommand) to provision the machine identity into a k8s Secret / compose secret so
// the key lives OFF the results volume (S4), instead of being self-minted by the first pod on the
// shared results PVC. base64-decoding this and writing the raw bytes to ARGUS_IDENTITY_PATH, or
// placing it verbatim in a k8s Secret's `data`, yields a file LoadKey accepts.
func GenerateKeyB64() (string, error) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(priv), nil
}

// ── the X25519 key (AC-4b) ───────────────────────────────────────────────────────────────────────
//
// Beside the Ed25519 machine identity above, an executor also holds an X25519 keypair — not an
// authenticator, but the ECDH public half a control plane seals a run's EXPECT material under
// (federation.SealBody / RegisterRequest.X25519PublicKey).
//
// The X25519 key is DERIVED from the Ed25519 machine key, never written. On the Kubernetes tiers the
// machine key is the exec-identity Secret, mounted read-only, so nothing can be created beside it; an
// executor that tried to mint and persist a second key there exited at start with "read-only file
// system" and never listened or registered. Deriving it keeps every property that file gave: the
// same machine key yields the same X25519 key across restarts, re-creates and replicas, and a new
// machine key yields a new one.
//
// An install that already has "<identity path>.x25519" on disk (minted by an earlier version and
// already registered with the control plane) keeps using that file, so its registered key does not
// change underneath it.

// x25519DerivationInfo is the HKDF info string for the derived X25519 key. It is versioned: changing
// the derivation means changing this string, which changes every derived key.
const x25519DerivationInfo = "argus executor x25519 v1"

// DeriveX25519Key derives the executor's X25519 private key from its Ed25519 machine key:
// HKDF-SHA256 over the Ed25519 seed with a fixed, versioned info string, 32 bytes, used as the X25519
// scalar. Deterministic and side-effect free.
func DeriveX25519Key(id ed25519.PrivateKey) (*ecdh.PrivateKey, error) {
	if len(id) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity: cannot derive an X25519 key from a %d-byte Ed25519 key (want %d)", len(id), ed25519.PrivateKeySize)
	}
	scalar, err := hkdf.Key(sha256.New, id.Seed(), nil, x25519DerivationInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("identity: derive X25519 key: %w", err)
	}
	return ecdh.X25519().NewPrivateKey(scalar)
}

// LoadOrDeriveX25519Key returns the executor's X25519 private key:
//
//   - if path exists, it must hold a raw 32-byte X25519 scalar, which is used as-is (an existing
//     install keeps its registered key); a file that is not a valid key is an error;
//   - if path does not exist, the key is derived from the Ed25519 machine key id (DeriveX25519Key)
//     and NOTHING is written, so a read-only identity mount works;
//   - any other read error fails, naming the path.
func LoadOrDeriveX25519Key(path string, id ed25519.PrivateKey) (*ecdh.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		priv, perr := ecdh.X25519().NewPrivateKey(b)
		if perr != nil {
			return nil, fmt.Errorf("identity: %s is not a valid X25519 private key: %w", path, perr)
		}
		return priv, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("identity: read %s: %w", path, err)
	}
	return DeriveX25519Key(id)
}

// X25519PublicKeyB64 renders the public half as standard base64 for the register wire
// (RegisterRequest.X25519PublicKey).
func X25519PublicKeyB64(priv *ecdh.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes())
}

// ── the identity CACHE (VR-F7 / INT-014) ─────────────────────────────────────────────────────────
//
// THE FAILURE, reproduced live 2026-08-07. On compose the machine key is a HOST FILE bind-mounted at
// /run/secrets/argus_identity. The onboarding kit lived under C:\tmp, C:\tmp was wiped, and the
// mount became a dangling reference:
//
//	$ docker exec <executor> ls -la /run/secrets/
//	-????????? ? ?    ?       ?            ? argus_identity      <- host source gone
//
// Deleting the host file does NOT stop the container; the mount just turns unreadable, silently. The
// executor kept running and kept reporting healthy, because it had read the key into memory at
// startup and never looked again. Teardown then failed at every step of its identity resolution and
// escalated to a browser sign-in — which is the symptom the owner reported, and the least of it.
//
// k3d and aks are immune: their identity is a Kubernetes Secret inside the cluster. Compose is the
// only tier whose machine identity depends on a directory the operator is free to delete.
//
// CacheKey is fix (1) from the finding: the running container owns a DURABLE COPY, so the identity
// survives the host file vanishing. The copy is deliberately in the container's own writable layer
// and NOT on the results volume — the results volume is shared with the in-env MCP server, and G4/S4
// moved the key OFF it on purpose. Putting it back would undo that to fix this.
//
// Best-effort by design: a read-only root filesystem or a missing /var/lib is a reason to carry on
// with the in-memory key, not a reason to refuse to start. It returns the error so the caller can
// SAY so rather than silently skip.
const DefaultIdentityCachePath = "/var/lib/argus/identity.key"

// identityCachePath resolves the cache location, overridable for a container whose /var/lib is not
// writable. Local to this package so the runner does not depend on the CLI's helper.
func identityCachePath() string {
	if v := os.Getenv("ARGUS_IDENTITY_CACHE"); v != "" {
		return v
	}
	return DefaultIdentityCachePath
}

func CacheKey(priv ed25519.PrivateKey, cachePath string) error {
	if cachePath == "" || len(priv) != ed25519.PrivateKeySize {
		return nil
	}
	if b, err := os.ReadFile(cachePath); err == nil && len(b) == ed25519.PrivateKeySize {
		if string(b) == string(priv) {
			return nil // already cached, byte-identical
		}
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return fmt.Errorf("identity cache: %w", err)
	}
	// Write-and-rename so a reader never sees a partial key — a truncated identity is worse than an
	// absent one, because it authenticates as nobody while looking present.
	tmp := cachePath + ".tmp"
	if err := os.WriteFile(tmp, priv, 0o600); err != nil {
		return fmt.Errorf("identity cache: %w", err)
	}
	if err := os.Rename(tmp, cachePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("identity cache: %w", err)
	}
	return nil
}

// IdentitySourceReadable reports whether the ORIGINAL key file can still be read — fix (2) from the
// finding, "detect and say so".
//
// This is not a health check on the executor's ability to work: it holds the key in memory and keeps
// authenticating fine. It is a check on whether the identity SURVIVES a container recreate, and on
// whether anything else that reaches for the file (teardown, notably) will find it. A dangling mount
// currently renders as perfect health, which is the absence-is-not-health pattern that made this
// take a wipe and a browser prompt to discover.
//
// Returns nil when readable and the correct size; an error naming what is wrong otherwise.
func IdentitySourceReadable(path string) error {
	if path == "" {
		return fmt.Errorf("no identity path configured")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return fmt.Errorf("%s is %d bytes, not a %d-byte Ed25519 key", path, len(b), ed25519.PrivateKeySize)
	}
	return nil
}

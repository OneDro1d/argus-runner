package runner

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC-4b — the X25519 key beside the machine identity. An existing "<identity>.x25519" file is used
// as-is; otherwise the key is derived from the Ed25519 machine key and nothing is written.

func newEd25519(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// testX25519Key gives a test an X25519 key by the same path Bootstrap uses when no key file exists.
func testX25519Key(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	priv, err := LoadOrDeriveX25519Key(filepath.Join(t.TempDir(), "id.key.x25519"), newEd25519(t))
	if err != nil {
		t.Fatalf("x25519 key: %v", err)
	}
	return priv
}

func TestLoadOrDeriveX25519Key_AbsentFile_DerivesWritesNothingAndIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.key.x25519")
	id := newEd25519(t)

	priv, err := LoadOrDeriveX25519Key(path, id)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if priv == nil {
		t.Fatal("LoadOrDeriveX25519Key returned a nil key")
	}
	if _, serr := os.Lstat(path); !os.IsNotExist(serr) {
		t.Errorf("a key file was written at %s (stat err %v) — the derived key must never be persisted", path, serr)
	}

	again, err := LoadOrDeriveX25519Key(path, id)
	if err != nil {
		t.Fatalf("second derive: %v", err)
	}
	if string(again.Bytes()) != string(priv.Bytes()) {
		t.Fatal("the same machine key derived two DIFFERENT X25519 keys")
	}

	// A copy of the same machine key (another replica mounting the same Secret) derives the same key.
	clone := append(ed25519.PrivateKey(nil), id...)
	fromClone, err := DeriveX25519Key(clone)
	if err != nil {
		t.Fatal(err)
	}
	if string(fromClone.Bytes()) != string(priv.Bytes()) {
		t.Error("a byte-identical machine key derived a different X25519 key")
	}

	other, err := LoadOrDeriveX25519Key(path, newEd25519(t))
	if err != nil {
		t.Fatal(err)
	}
	if string(other.Bytes()) == string(priv.Bytes()) {
		t.Error("two different machine keys derived the SAME X25519 key")
	}
}

func TestLoadOrDeriveX25519Key_ExistingFileIsPreferredOverDerivation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.key.x25519")
	existing, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, existing.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	id := newEd25519(t)

	got, err := LoadOrDeriveX25519Key(path, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(got.Bytes()) != string(existing.Bytes()) {
		t.Fatal("the existing X25519 key file was ignored")
	}
	derived, err := DeriveX25519Key(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Bytes()) == string(derived.Bytes()) {
		t.Fatal("precondition: the file key must differ from the derived key for this test to mean anything")
	}
}

func TestLoadOrDeriveX25519Key_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.x25519")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrDeriveX25519Key(path, newEd25519(t)); err == nil {
		t.Fatal("a corrupt X25519 key file was silently accepted")
	}
}

func TestLoadOrDeriveX25519Key_OtherReadErrorFailsNamingThePath(t *testing.T) {
	// A directory where the key file should be: the read fails with something other than not-exist,
	// which must not be papered over by deriving.
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.key.x25519")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrDeriveX25519Key(path, newEd25519(t))
	if err == nil {
		t.Fatal("an unreadable X25519 key path was silently replaced by a derived key")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error does not name the path: %v", err)
	}
}

func TestDeriveX25519Key_RejectsAMalformedMachineKey(t *testing.T) {
	if _, err := DeriveX25519Key(ed25519.PrivateKey(make([]byte, 10))); err == nil {
		t.Fatal("a 10-byte machine key was accepted")
	}
}

func TestX25519PublicKeyB64_RendersThePublicHalf(t *testing.T) {
	priv := testX25519Key(t)
	b64 := X25519PublicKeyB64(priv)
	pubBytes, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("not valid base64: %v", err)
	}
	if len(pubBytes) != 32 {
		t.Fatalf("public key = %d bytes, want 32", len(pubBytes))
	}
	if string(pubBytes) != string(priv.PublicKey().Bytes()) {
		t.Error("X25519PublicKeyB64 does not match the key's own public half")
	}
}

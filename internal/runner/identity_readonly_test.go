package runner

// identity_readonly_test.go — the executor STARTS when its machine identity is a read-only mount.
//
// On the Kubernetes tiers the identity key is the exec-identity Secret, mounted read-only at
// /etc/argus/identity/identity.key. The X25519 key used to be minted on first start and written
// beside it as "<identity>.x25519"; on a read-only mount that write fails, Bootstrap returns
// "identity: persist …: read-only file system", and the pod exits before it listens or registers.
//
// These tests drive the real constructor (Bootstrap) by the path a container takes, with the identity
// directory made unwritable.

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// readOnlyIdentity provisions an Ed25519 identity key in a directory that cannot be written to, the
// way a Secret mount presents it, and returns the key path plus a function listing what the
// directory holds (so a test can prove nothing was added).
//
// As a normal user a 0555 directory refuses new files. Root ignores mode bits, so under uid 0 the
// sibling "<identity>.x25519" is instead pre-placed as a dangling symlink into a directory that does
// not exist: reading it reports not-exist, and creating it fails, exactly as on a read-only mount.
func readOnlyIdentity(t *testing.T) (idPath string, entries func() []string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	idPath = filepath.Join(dir, "identity.key")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idPath, priv, 0o400); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		if err := os.Symlink(filepath.Join(dir, "absent", "x25519"), idPath+".x25519"); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // so TempDir's RemoveAll can clean up
	}
	entries = func() []string {
		des, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("list %s: %v", dir, err)
		}
		var names []string
		for _, de := range des {
			names = append(names, de.Name())
		}
		sort.Strings(names)
		return names
	}
	return idPath, entries
}

func bootstrapWithIdentity(t *testing.T, idPath string) *Server {
	t.Helper()
	t.Setenv("ARGUS_IDENTITY_CACHE", filepath.Join(t.TempDir(), "cache", "identity.key"))
	srv, err := Bootstrap(context.Background(), Config{
		Addr: "127.0.0.1:0",
		Fed: FedConfig{
			CPURL:        "http://127.0.0.1:1", // never dialled during Bootstrap
			InstanceID:   "ro-identity-test",
			IdentityPath: idPath,
			Exec:         ExecConfig{ResultsRoot: t.TempDir()},
		},
		Log: func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("Bootstrap with a read-only identity mount returned an error: %v", err)
	}
	t.Cleanup(func() { _ = srv.ln.Close() })
	return srv
}

func TestBootstrap_ReadOnlyIdentityMount_StartsWritesNothingAndIsStable(t *testing.T) {
	idPath, entries := readOnlyIdentity(t)
	before := entries()

	first := bootstrapWithIdentity(t, idPath)
	if first.x25519PubB64 == "" {
		t.Fatal("no X25519 public key was produced, so registration would carry none")
	}
	if after := entries(); len(after) != len(before) {
		t.Errorf("the identity directory changed: before %v, after %v — nothing may be written beside a read-only identity", before, after)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(idPath), "absent")); err == nil {
		t.Error("a directory was created beside the identity key")
	}

	// A restart (or a second replica mounting the same Secret) must present the SAME key, or the
	// control plane would seal run material to a key no pod holds.
	second := bootstrapWithIdentity(t, idPath)
	if second.x25519PubB64 != first.x25519PubB64 {
		t.Errorf("the X25519 key changed across restarts: %s then %s", first.x25519PubB64, second.x25519PubB64)
	}
}

func TestBootstrap_ExistingX25519File_IsPreferred(t *testing.T) {
	dir := t.TempDir()
	idPath := filepath.Join(dir, "identity.key")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idPath, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	// An install that already registered a minted key keeps it.
	existing, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idPath+".x25519", existing.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := bootstrapWithIdentity(t, idPath)
	if want := X25519PublicKeyB64(existing); srv.x25519PubB64 != want {
		t.Errorf("the existing X25519 key file was not used: got public key %s, want %s", srv.x25519PubB64, want)
	}
}

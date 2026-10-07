package runner

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// VR-F7 / INT-014 — the compose machine identity survives the host file vanishing, and the executor
// SAYS when it has.
//
// ── THE FAILURE, reproduced live 2026-08-07 ───────────────────────────────────────────────────────
//
// On compose the machine key is a HOST FILE bind-mounted at /run/secrets/argus_identity. The
// onboarding kit lived under C:\tmp, C:\tmp was wiped, and:
//
//	$ docker exec <executor> ls -la /run/secrets/
//	-????????? ? ?    ?       ?            ? argus_identity
//	$ docker exec <executor> cat /run/secrets/argus_identity
//	(empty)
//
// `-?????????` with every field unknown is a bind mount whose host source no longer exists. Deleting
// the host file does not stop the container — the mount just turns unreadable, silently. The
// executor kept running and kept reporting REGISTERED · HEALTHY, because it had read the key into
// memory at startup and never looked again.
//
// Teardown then failed at every step of its identity resolution and escalated to a browser sign-in.
// That prompt is what the owner reported, and it is the least of it: anything else authorised by that
// identity was equally dead, discoverable only by attempting it.
//
// k3d and aks are immune — their identity is a Kubernetes Secret inside the cluster. THE COMPOSE TIER
// IS THE ONLY ONE WHOSE MACHINE IDENTITY DEPENDS ON A DIRECTORY THE OPERATOR MAY DELETE.
//
// SUITE-7's fallback ("read the key from the still-running executor") was designed for exactly "the
// kit is gone" — and read it through the SAME host file, so it could not survive the one case it
// exists for.

func TestCacheKey_SurvivesTheSourceFileVanishing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "identity.key")
	cache := filepath.Join(dir, "cache", "identity.key")

	priv, err := LoadOrCreateKey(src)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := CacheKey(priv, cache); err != nil {
		t.Fatalf("cache: %v", err)
	}

	// THE WHOLE POINT: delete the source, as a wiped kit directory does.
	if err := os.Remove(src); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	got, err := LoadKey(cache)
	if err != nil {
		t.Fatalf("the cached identity is unreadable after the source vanished: %v\n"+
			"  This is the case the cache exists for — SUITE-7's fallback read through the same host\n"+
			"  file, so it could not survive it.", err)
	}
	if string(got) != string(priv) {
		t.Error("the cached key differs from the loaded one — a DIFFERENT identity authenticates as nobody")
	}

	// The cache file must not be group/world-readable: it is the same secret, in a second place.
	//
	// Asserted only where the assertion MEANS something. NTFS does not carry POSIX mode bits, so Go
	// reports 0666 for a file created with 0600 on Windows and this check can never pass here. The
	// executor runs in a Linux container, which is where the bits are real — skipping is honest;
	// deleting the check would drop it on the platform that actually runs the code.
	if runtime.GOOS == "windows" {
		t.Log("skipping the permission assertion: NTFS has no POSIX mode bits (the executor runs on Linux)")
	} else if fi, serr := os.Stat(cache); serr == nil {
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("cache mode = %o, want no group/other bits — this is a private key", mode)
		}
	}
}

func TestCacheKey_IsIdempotentAndRefusesJunk(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "identity.key")
	_, priv, _ := ed25519.GenerateKey(nil)

	for i := 0; i < 3; i++ {
		if err := CacheKey(priv, cache); err != nil {
			t.Fatalf("cache pass %d: %v", i, err)
		}
	}
	got, err := LoadKey(cache)
	if err != nil || string(got) != string(priv) {
		t.Fatalf("after three passes the cache holds the wrong thing: err=%v", err)
	}
	// No path, or a key that is not a key, is a no-op rather than a half-written file.
	if err := CacheKey(priv, ""); err != nil {
		t.Errorf("empty path should be a no-op, got %v", err)
	}
	if err := CacheKey(ed25519.PrivateKey("short"), cache); err != nil {
		t.Errorf("a malformed key should be a no-op, got %v", err)
	}
	if got, _ := LoadKey(cache); string(got) != string(priv) {
		t.Error("a malformed key OVERWROTE a good cache — a truncated identity is worse than an " +
			"absent one, because it authenticates as nobody while looking present")
	}
	// No .tmp left behind: a stray half-key beside the real one is exactly the confusion to avoid.
	if _, err := os.Stat(cache + ".tmp"); err == nil {
		t.Error("a .tmp file survived the write")
	}
}

// "Detect and say so" — fix (2) from the finding. A dangling identity currently renders as perfect
// health, which is the absence-is-not-health pattern that made this take a wipe and a browser prompt
// to discover.
func TestIdentitySourceReadable_NamesWhatIsWrong(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.key")
	if _, err := LoadOrCreateKey(good); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := IdentitySourceReadable(good); err != nil {
		t.Errorf("a healthy identity reported %v", err)
	}

	// The dangling-mount signature: the path exists to the container but reads EMPTY.
	empty := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := IdentitySourceReadable(empty)
	if err == nil {
		t.Fatal("an EMPTY identity file reported healthy.\n" +
			"  That is exactly what a dangling bind mount looks like from inside the container, and\n" +
			"  reporting it as health is how this went unnoticed until a teardown asked for a browser.")
	}
	if !strings.Contains(err.Error(), empty) {
		t.Errorf("the error does not name the path: %v", err)
	}

	if err := IdentitySourceReadable(filepath.Join(dir, "absent.key")); err == nil {
		t.Error("a MISSING identity file reported healthy")
	}
	if err := IdentitySourceReadable(""); err == nil {
		t.Error("an unconfigured identity path reported healthy")
	}

	// Wrong size is its own message: a 32-byte public key where a 64-byte private one belongs is a
	// real mistake, and "cannot read" would send the reader looking at permissions instead.
	half := filepath.Join(dir, "half.key")
	if err := os.WriteFile(half, make([]byte, ed25519.PublicKeySize), 0o600); err != nil {
		t.Fatal(err)
	}
	err = IdentitySourceReadable(half)
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("a wrong-size key reported %v, want a message naming the size", err)
	}
}

func TestIdentityCachePath_DefaultsAndOverride(t *testing.T) {
	if got := identityCachePath(); got != DefaultIdentityCachePath {
		t.Errorf("default = %q, want %q", got, DefaultIdentityCachePath)
	}
	// Overridable for a container whose /var/lib is not writable — a read-only root filesystem must
	// not be a reason the identity cannot be cached at all.
	t.Setenv("ARGUS_IDENTITY_CACHE", "/tmp/x/identity.key")
	if got := identityCachePath(); got != "/tmp/x/identity.key" {
		t.Errorf("override = %q", got)
	}
}

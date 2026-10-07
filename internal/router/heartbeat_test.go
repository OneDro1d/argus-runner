package router

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func TestIdentity_IsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.RouterID != again.RouterID {
		t.Errorf("router id changed across a restart: %s -> %s", first.RouterID, again.RouterID)
	}
	if !first.Priv.Equal(again.Priv) {
		t.Error("the private key changed across a restart — every heartbeat would fail verification")
	}
}

// The id is DERIVED from the public key rather than generated separately, so a state directory can
// never hold an id its key does not correspond to. That pairing is what the control plane checks
// ("the signed subject does not match router_id"), and a drift between them would be unfixable
// without re-registering.
func TestIdentity_IDIsDerivedFromTheKey(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := routerIDFor(id.Priv.Public().(ed25519.PublicKey))
	if id.RouterID != want {
		t.Errorf("RouterID = %q, want the key-derived %q", id.RouterID, want)
	}
	raw, err := base64.StdEncoding.DecodeString(id.PublicKeyB64())
	if err != nil || len(raw) != ed25519.PublicKeySize {
		t.Errorf("PublicKeyB64 is not a base64 Ed25519 public key: %v", err)
	}
}

// A corrupt key file must REFUSE with instructions, not silently mint a new identity: a fresh key
// would be rejected by the control plane on every heartbeat, and the router would look broken for a
// reason nobody could see.
func TestIdentity_CorruptKeyRefusesWithInstructions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, identityFile), []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateIdentity(dir)
	if err == nil {
		t.Fatal("a corrupt identity file was silently replaced")
	}
	if !contains(err.Error(), "re-register") {
		t.Errorf("the error must say what to do; got %v", err)
	}
}

// The heartbeat must be VERIFIABLE by the control plane's own verifier, with the router id as the
// signed subject — the exact pairing handleRouterHeartbeat checks.
func TestHeartbeat_SignsAJWTTheControlPlaneCanVerify(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fed/router/heartbeat" {
			t.Errorf("posted to %q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := Heartbeat{CPURL: srv.URL, Host: "DESKTOP-1", Version: "0.3.1", Port: 9765}
	if err := h.Send(id, time.Now()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got["router_id"] != id.RouterID || got["host"] != "DESKTOP-1" {
		t.Errorf("payload = %v", got)
	}
	jwt, _ := got["jwt"].(string)
	claims, verr := federation.VerifyJWT(jwt, id.Priv.Public().(ed25519.PublicKey), time.Now())
	if verr != nil {
		t.Fatalf("the control plane's own verifier rejected the heartbeat: %v", verr)
	}
	if claims.Sub != id.RouterID {
		t.Errorf("signed subject = %q, want the router id %q", claims.Sub, id.RouterID)
	}
}

// A heartbeat that cannot land is reported to the caller and CHANGES NOTHING ELSE. VR-R10 makes a
// stale heartbeat display-only: an agent whose router is running does not care what the cloud
// believes, and the router's job is routing.
func TestHeartbeat_UnreachableControlPlaneIsAnErrorNotACrash(t *testing.T) {
	dir := t.TempDir()
	id, _ := LoadOrCreateIdentity(dir)
	h := Heartbeat{CPURL: "http://127.0.0.1:1", Host: "H", HTTP: &http.Client{Timeout: 2 * time.Second}}
	if err := h.Send(id, time.Now()); err == nil {
		t.Fatal("an unreachable control plane reported success")
	}
}

// The interval must sit comfortably inside the staleness window, or a single missed report makes a
// healthy router look silent — and a status that flaps teaches people to ignore it.
func TestHeartbeatInterval_IsWellInsideTheStaleWindow(t *testing.T) {
	const staleAfter = 3 * time.Minute // store.RouterStaleAfter
	if HeartbeatInterval >= staleAfter {
		t.Fatalf("HeartbeatInterval %v >= stale window %v", HeartbeatInterval, staleAfter)
	}
	if HeartbeatInterval*2 >= staleAfter {
		t.Errorf("HeartbeatInterval %v leaves no room for ONE missed report before %v", HeartbeatInterval, staleAfter)
	}
}

// No control plane configured is not an error state — a machine can be onboarded without one, and the
// router must simply not report.
func TestRunHeartbeat_NoControlPlaneReturnsImmediately(t *testing.T) {
	done := make(chan struct{})
	go func() {
		RunHeartbeat(Heartbeat{Identity: func() Identity { return Identity{} }}, nil, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunHeartbeat blocked with no control plane configured")
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

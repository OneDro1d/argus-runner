package main

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// AC-D63 (#407): `update started` — the block tells the control plane it has BEGUN.
//
// Best-effort exactly as `update report`: a control plane that is down, or an old one with no such route, must never
// stop an update the operator started. It exits 0 and says what happened.

func startedKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "identity.i1.key")
	if err := os.WriteFile(key, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestUpdateStarted_PostsTheTargetSignedWithTheInstanceIdentity(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.Unmarshal(b, &gotBody)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	key := startedKey(t)
	if code := dispatch([]string{"update", "started", "--control-plane", srv.URL, "--instance-id", "i1",
		"--identity", key, "--version", "0.3.49"}); code != exitOK {
		t.Fatalf("update started exited %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/fed/update-started" {
		t.Errorf("posted to %q, want /fed/update-started", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || len(gotAuth) < 20 {
		t.Errorf("the start was not signed with the instance identity: Authorization = %q", gotAuth)
	}
	if gotBody["target"] != "0.3.49" || gotBody["kind"] != "update" {
		t.Errorf("body = %v, want target 0.3.49 kind update", gotBody)
	}
	if _, sent := gotBody["started_at"]; sent {
		t.Errorf("the machine sent a time (%v): the control plane stamps its own clock", gotBody)
	}
}

func TestUpdateStarted_ARollbackBlockSaysSo(t *testing.T) {
	var mu sync.Mutex
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(b, &gotBody)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if code := dispatch([]string{"update", "started", "--control-plane", srv.URL, "--instance-id", "i1",
		"--identity", startedKey(t), "--version", "0.3.48", "--rollback-to", "0.3.48"}); code != exitOK {
		t.Fatalf("exited %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotBody["kind"] != "rollback" || gotBody["target"] != "0.3.48" {
		t.Errorf("body = %v, want kind rollback target 0.3.48", gotBody)
	}
}

// ⛔ NEVER A REASON TO STOP THE UPDATE: an old control plane (404), a dead one, no URL, and an unreadable key all exit 0.
func TestUpdateStarted_IsBestEffort(t *testing.T) {
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	key := startedKey(t)
	for name, args := range map[string][]string{
		"an old control plane (404)": {"--control-plane", old.URL, "--identity", key},
		"a dead control plane":       {"--control-plane", deadURL, "--identity", key},
		"no control plane":           {"--identity", key},
		"no identity key":            {"--control-plane", old.URL},
		"an unreadable key":          {"--control-plane", old.URL, "--identity", filepath.Join(t.TempDir(), "missing.key")},
	} {
		argv := append([]string{"update", "started", "--instance-id", "i1", "--version", "0.3.49"}, args...)
		if code := dispatch(argv); code != exitOK {
			t.Errorf("%s: update started exited %d — a failed report would stop the operator's update", name, code)
		}
	}
}

func TestUpdateStarted_RequiresATargetAndAnInstance(t *testing.T) {
	if code := dispatch([]string{"update", "started", "--instance-id", "i1"}); code == exitOK {
		t.Error("a start with no target version was accepted: it would render as an update to nowhere")
	}
	if code := dispatch([]string{"update", "started", "--version", "0.3.49"}); code == exitOK {
		t.Error("a start naming no instance was accepted")
	}
}

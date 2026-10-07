package main

// cloud_deregister_readback_test.go — AC-D61: THE READ-BACK.
//
// After a successful de-register the CLI asks the control plane, with the SAME machine identity,
// whether it still lists the instance (GET /fed/state), and reports registration_after =
// gone | present | unknown, so teardown's verification no longer reads COULD NOT CHECK on every run.
// `unknown` must never be rendered as gone.

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func deregReadBack(t *testing.T, stateStatus int, stateBody string) map[string]any {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/fed/deregister" {
			_, _ = w.Write([]byte(`{"deregistered": true, "kit_dir": "/kits/x"}`))
			return
		}
		w.WriteHeader(stateStatus)
		_, _ = w.Write([]byte(stateBody))
	}))
	t.Cleanup(srv.Close)

	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(keyPath, priv, 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	t.Setenv("ARGUS_IDENTITY_PATH", keyPath)
	raw := captureEmitRaw(t, func() {
		_ = cmdCloudDeregister(&commonFlags{controlPlane: srv.URL, instance: "inst-emit"})
	})
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("cloud-deregister did not emit JSON: %v\n  output: %s", err, raw)
	}
	return got
}

func TestCloudDeregisterEmit_ReadsBackThatTheRegistrationIsGone(t *testing.T) {
	got := deregReadBack(t, http.StatusUnauthorized, `{"reason":"unknown_instance"}`)
	if got["registration_after"] != "gone" {
		t.Fatalf("registration_after = %v, want gone: %v", got["registration_after"], got)
	}
	if got["kit_dir"] != "/kits/x" {
		t.Errorf("kit_dir lost beside the read-back: %v", got)
	}
}

func TestCloudDeregisterEmit_ReportsAStillPresentRegistration(t *testing.T) {
	got := deregReadBack(t, http.StatusOK, `{"running_run_id":""}`)
	if got["registration_after"] != "present" {
		t.Fatalf("registration_after = %v, want present: %v", got["registration_after"], got)
	}
}

func TestCloudDeregisterEmit_AnUnansweredReadBackIsUnknownNeverGone(t *testing.T) {
	got := deregReadBack(t, http.StatusServiceUnavailable, `down`)
	if got["registration_after"] != "unknown" {
		t.Fatalf("registration_after = %v, want unknown: an unanswered question must not read as gone: %v",
			got["registration_after"], got)
	}
}

func TestCloudDeregisterEmit_NotPresentNeedsNoSecondQuestion(t *testing.T) {
	got := deregEmitStatus(t, http.StatusUnauthorized, `{"reason":"unknown_instance"}`)
	if got["registration_after"] != "gone" {
		t.Fatalf("the control plane already said it had no such instance; registration_after = %v",
			got["registration_after"])
	}
}

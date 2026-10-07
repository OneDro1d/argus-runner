package main

// cloud_deregister_emit_test.go — WHAT `cloud-deregister` PRINTS, and what it must never print again.
//
// ── WHAT THIS FILE USED TO BE ─────────────────────────────────────────────────────────────────────
//
// Hop 3 of a four-hop carrier for "what became of the ACCOUNT's author token (minted during onboarding)": control-plane
// response → the client decode → THIS EMIT → teardown.sh's parse. Its hardest case was the tri-state:
// the decode handed over a *bool and the emit had to turn nil into an ABSENT KEY rather than `false`,
// because teardown reads a present `false` as "the token is STILL LIVE and that is correct".
//
// ── WHY THAT HALF IS GONE ─────────────────────────────────────────────────────────────────────────
//
// VR10-T4-7: `deleteInstance` no longer revokes anything. The token is per MACHINE in 0.3.29 and an
// instance delete cannot name a machine, so the control plane makes no such decision, sends no such
// field, and the CLI has nothing to forward. The carrier's tests are deleted with the rule rather
// than weakened to pass — replaced below by the negative, which is the thing that can now regress:
// somebody "restoring" the key because a shell still greps for it.
//
// ⚠ VR8-V1's half of this file is UNCHANGED and still load-bearing: "I deleted it" and "it was not
// there" are opposite answers, and teardown prints three specific claims off the back of them.

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// deregEmitEnv stands up a stub control plane answering the de-register with `body`, and a machine
// identity on disk, then runs the real cmdCloudDeregister and returns what it printed.
func deregEmitEnv(t *testing.T, body string) map[string]any {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
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

// 🚩 THE NEGATIVE, AND IT IS THE ONE THAT CAN REGRESS. Nothing decides a token's fate at this call
// any more, so nothing may report one — not even from a control plane that still sends the retired
// key, which every deployment older than 0.3.29 does. A CLI that forwarded it would put teardown back
// to announcing an account-wide revoke that never happened.
func TestCloudDeregisterEmit_NeverReportsATokenVerdict(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"a current control plane", `{"deregistered": true, "kit_dir": "/kits/x"}`},
		{"one still sending the retired key", `{"deregistered": true, "kit_dir": "/kits/x", "auto_token_revoked": true}`},
		{"…and the false spelling of it", `{"deregistered": true, "kit_dir": "/kits/x", "auto_token_revoked": false}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := deregEmitEnv(t, c.body)
			if v, present := got["auto_token_revoked"]; present {
				t.Fatalf("the CLI emitted auto_token_revoked=%v. "+
					"De-registering an instance decides nothing about a credential in 0.3.29: the token "+
					"is per MACHINE and this call cannot name one. Reporting a verdict here would have "+
					"teardown announce that a colleague's machine had been cut off, or reassure the "+
					"operator about a token nobody examined.", v)
			}
			if got["kit_dir"] != "/kits/x" {
				t.Errorf("kit_dir = %#v — VR5-T1's value must survive on the same response", got["kit_dir"])
			}
		})
	}
}

// The kit path rides this emit and VR5-T1 depends on it reaching teardown. A Windows path, because
// that is where the backslashes are, and because this is the value teardown scrubs credentials from.
func TestCloudDeregisterEmit_KeepsTheKitPathBesideTheVerdict(t *testing.T) {
	got := deregEmitEnv(t,
		`{"deregistered": true, "kit_dir": "C:\\kits\\orderservice", "auto_token_revoked": true}`)
	if got["kit_dir"] != `C:\kits\orderservice` {
		t.Errorf("kit_dir = %#v, want the recorded path — teardown scrubs credentials from it (VR5-T1)",
			got["kit_dir"])
	}
	if got["instance_id"] != "inst-emit" {
		t.Errorf("instance_id = %#v, want inst-emit", got["instance_id"])
	}
}

// ── VR8-V1 (V26-004): "I deleted it" and "it was not there" are OPPOSITE answers ─────────────────
//
// THE DEFECT, measured live 2026-08-20. `social-mcp-compose` had never been registered — the control
// plane reported instances=6 without it, before and after. Tearing it down printed:
//
//	cloud registration removed — the name is free, the data is purged, the executor's tokens are revoked
//
// None of those three things happened. And the SAME run then said twice that it could not find out
// what became of the token or where the instance was onboarded from, without ever retracting the
// confident line above.
//
// The cause is three layers down and entirely deliberate at each layer, which is why it survived:
// the CP 401s ReasonUnknownInstance for a row that is already gone (fed.go:154), the client treats
// that as idempotent success and returns a ZERO OUTCOME (client.go:228-236) — its own comment says
// "this call deleted nothing" — and the CLI then hardcodes `"deregistered": true` (main.go:886).
// Every layer knew. None of them could say.
func deregEmitStatus(t *testing.T, status int, body string) map[string]any {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
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

func TestCloudDeregisterEmit_AnInstanceTheControlPlaneNeverHadIsNotASuccess(t *testing.T) {
	got := deregEmitStatus(t, http.StatusUnauthorized, `{"reason":"unknown_instance"}`)

	if got["deregistered"] == true {
		t.Error(`the CLI claimed "deregistered": true for an instance the control plane has never ` +
			`heard of. Nothing was removed, no name was freed, no data was purged and no token was ` +
			`revoked — and teardown prints exactly those three claims off the back of this field.`)
	}
	if got["not_present"] != true {
		t.Errorf(`the CLI did not report not_present for an unknown instance (emitted %v).`+"\n"+
			`  "I deleted it" and "it was not there" are opposite epistemic states. Collapsing them `+
			`leaves an operator clearing a GHOST registration — the one case where the whole question `+
			`is "did the control plane really let go?" — with an answer that was never asked for.`, got)
	}
}

// The other direction must not regress: a real de-registration still reports success.
func TestCloudDeregisterEmit_ARealDeregistrationStillSaysSo(t *testing.T) {
	got := deregEmitStatus(t, http.StatusOK, `{"deregistered": true, "kit_dir": "/kits/x"}`)
	if got["deregistered"] != true {
		t.Errorf("a genuine de-registration no longer reports success: %v", got)
	}
	if _, present := got["not_present"]; present {
		t.Errorf("not_present leaked into a successful de-registration: %v", got)
	}
}

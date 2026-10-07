package router

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// VR-B4 / VR-B6 — the router half of an auto-token rotation.
//
// The control plane CANNOT push to a router: it sits on the operator's machine behind whatever NAT
// that machine has. So the replacement travels on the heartbeat — the one channel that is always
// running — and the router acknowledges on the NEXT beat, once the token is actually on disk.
//
// That ordering is the whole safety property. The acknowledgement is what revokes the OLD token, so
// acknowledging before the write means a crash between the two leaves every folder holding a token
// that no longer authenticates. VR-B6 says the old one stays valid until the holder really has the
// new one; here is where "really" is decided.

type hbCall struct {
	RouterID    string `json:"router_id"`
	RotationAck string `json:"rotation_ack"`
}

// cpStub answers heartbeats, offering a rotation on the FIRST beat only, and records what it was told.
func cpStub(t *testing.T, offer bool) (*httptest.Server, *[]hbCall) {
	t.Helper()
	var calls []hbCall
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var c hbCall
		_ = json.Unmarshal(b, &c)
		calls = append(calls, c)
		n++
		out := map[string]any{"ok": true, "server_time": time.Now().UTC()}
		if offer && n == 1 {
			out["rotate_author_token"] = map[string]any{
				"token": "odts_replacement", "old_token_id": "tok_old", "new_token_id": "tok_new",
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testIdentity(t *testing.T) Identity {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{RouterID: "rtr-test", Priv: priv}
}

// THE HAPPY PATH: take it, store it, acknowledge on the NEXT beat.
func TestHeartbeat_StoresARotatedTokenThenAcknowledgesOnTheNextBeat(t *testing.T) {
	srv, calls := cpStub(t, true)
	id := testIdentity(t)

	var stored []string
	hb := &Heartbeat{
		CPURL: srv.URL, Host: "desk", Port: 9765,
		OnRotatedToken: func(tok string) error { stored = append(stored, tok); return nil },
	}

	if err := hb.Send(id, time.Now()); err != nil {
		t.Fatalf("beat 1: %v", err)
	}
	if len(stored) != 1 || stored[0] != "odts_replacement" {
		t.Fatalf("the replacement was not stored: %v", stored)
	}
	if (*calls)[0].RotationAck != "" {
		t.Errorf("beat 1 carried an acknowledgement (%q) — nothing had been stored when it was sent",
			(*calls)[0].RotationAck)
	}

	if err := hb.Send(id, time.Now()); err != nil {
		t.Fatalf("beat 2: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 heartbeats, got %d", len(*calls))
	}
	if (*calls)[1].RotationAck != "tok_old" {
		t.Errorf("beat 2 ack = %q, want tok_old.\n"+
			"  The acknowledgement is what revokes the outgoing token; without it the rotation never\n"+
			"  completes and the stalled sweep flags it a week later.", (*calls)[1].RotationAck)
	}

	// And it is not repeated forever once accepted.
	if err := hb.Send(id, time.Now()); err != nil {
		t.Fatalf("beat 3: %v", err)
	}
	if (*calls)[2].RotationAck != "" {
		t.Errorf("beat 3 repeated the ack (%q) after it was accepted", (*calls)[2].RotationAck)
	}
}

// THE ONE THAT PROTECTS THE ESTATE: if the token cannot be stored, it is NOT acknowledged — so the
// old one is never revoked, and the next beat tries again.
func TestHeartbeat_AFailedStoreDoesNotAcknowledge(t *testing.T) {
	srv, calls := cpStub(t, true)
	id := testIdentity(t)

	hb := &Heartbeat{
		CPURL: srv.URL, Host: "desk", Port: 9765,
		OnRotatedToken: func(string) error { return errNoDisk },
	}
	err := hb.Send(id, time.Now())
	if err == nil {
		t.Fatal("a failed store reported success — the caller would never know the rotation was lost")
	}
	if err := hb.Send(id, time.Now()); err != nil {
		t.Fatalf("beat 2: %v", err)
	}
	for i, c := range *calls {
		if c.RotationAck != "" {
			t.Errorf("beat %d acknowledged (%q) despite the store failing.\n"+
				"  That acknowledgement revokes the OLD token, so every folder on this machine would\n"+
				"  be holding a credential that no longer authenticates.", i+1, c.RotationAck)
		}
	}
}

// A router that holds no author token acknowledges nothing — the offer is simply left alone. The old
// token keeps working and the control plane flags the rotation stalled, which is honest.
func TestHeartbeat_NoHolderMeansNoAcknowledgement(t *testing.T) {
	srv, calls := cpStub(t, true)
	id := testIdentity(t)
	hb := &Heartbeat{CPURL: srv.URL, Host: "desk", Port: 9765} // OnRotatedToken nil

	if err := hb.Send(id, time.Now()); err != nil {
		t.Fatalf("beat 1: %v", err)
	}
	if err := hb.Send(id, time.Now()); err != nil {
		t.Fatalf("beat 2: %v", err)
	}
	for i, c := range *calls {
		if c.RotationAck != "" {
			t.Errorf("beat %d acknowledged a rotation this router never took (%q)", i+1, c.RotationAck)
		}
	}
}

// The ordinary beat is unchanged — no rotation offered, no ack, no error.
func TestHeartbeat_NoOfferIsANoOp(t *testing.T) {
	srv, calls := cpStub(t, false)
	id := testIdentity(t)
	called := false
	hb := &Heartbeat{
		CPURL: srv.URL, Host: "desk", Port: 9765,
		OnRotatedToken: func(string) error { called = true; return nil },
	}
	if err := hb.Send(id, time.Now()); err != nil {
		t.Fatalf("beat: %v", err)
	}
	if called {
		t.Error("the rotation hook fired without an offer")
	}
	if (*calls)[0].RotationAck != "" {
		t.Errorf("an ack was sent with no rotation in flight: %q", (*calls)[0].RotationAck)
	}
}

type diskErr struct{}

func (diskErr) Error() string { return "no space left on device" }

var errNoDisk = diskErr{}

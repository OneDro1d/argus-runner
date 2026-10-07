package mcpserver

// stateless_test.go — R3/R2 stateless sessions on the STREAMABLE transport (M3.1 §D-3.1.2,
// CP-M3-118). TEST-FIRST. The multi-replica contract: a streamable-HTTP client whose `initialize`
// landed on replica A must be servable by replica B (auth is per-request Bearer; the handshake
// state machine carries no security) — replica B ADOPTS the client-supplied unknown
// Mcp-Session-Id as ready. The legacy SSE transport stays strict (its stream IS the session and is
// inherently replica-pinned — the R3 decision routes it via gateway affinity instead).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func streamablePost(t *testing.T, url, sid, token string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

// TestStreamable_OriginReplicaServesWhenTheNotificationLandsElsewhere is the case a ROUND-ROBIN
// gateway actually produces, and the one TestStreamable_CrossReplicaSessionAdopted below does NOT
// cover: that test sends `notifications/initialized` back to replica A, the same replica that
// handled `initialize`. A load balancer has no reason to do that.
//
// Production sequence behind the AGW (measured live on example-cluster 2026-08-04, 3 replicas):
//
//	initialize                 -> replica A   (A: sConnected -> sInitializing)
//	notifications/initialized  -> replica B   (B ADOPTS the id as sReady; A never learns)
//	tools/call                 -> replica A   -> -32003, because A is still sInitializing
//
// Result: ~1/3 of all calls fail, and it is SILENT — the onboarder's bulk scenario import lost
// 9 of 27 scenarios that way, each one individually valid, leaving an instance that looks healthy
// with a third of its catalog missing.
//
// The contract: on the STREAMABLE transport a session id the client presents is servable, full
// stop. Two replicas must not disagree about whether the same id is usable — and today the
// ADOPTING replicas are already the permissive ones (AdoptSession marks sReady), so the origin
// replica is the outlier, not the other way round.
func TestStreamable_OriginReplicaServesWhenTheNotificationLandsElsewhere(t *testing.T) {
	a := newSrv(t)
	b := newSrv(t)
	srvA := httptest.NewServer(NewHTTPHandler(a))
	defer srvA.Close()
	srvB := httptest.NewServer(NewHTTPHandler(b))
	defer srvB.Close()

	// initialize lands on A
	resp, _ := streamablePost(t, srvA.URL, "", rtok, reqBytes(1, "initialize", map[string]any{}))
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize on A returned no Mcp-Session-Id")
	}
	// the handshake notification round-robins to B, NOT back to A
	_, _ = streamablePost(t, srvB.URL, sid, rtok, notifBytes("notifications/initialized", nil))

	// a later call round-robins back to A, which never saw the notification
	resp, body := streamablePost(t, srvA.URL, sid, rtok, reqBytes(2, "tools/call", callParamsFor("runner__ping")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("origin replica A answered %d: %s", resp.StatusCode, body)
	}
	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if r.Error != nil {
		t.Fatalf("origin replica REFUSED its own session (%d %s) — a round-robin gateway makes this ~1/N of ALL calls, silently",
			r.Error.Code, r.Error.Message)
	}
}

// The same defect with NO notification at all: a client that goes straight from initialize to a
// tool call on the SAME replica. The adopting replicas already serve this; the origin must too.
func TestStreamable_OriginReplicaServesImmediatelyAfterInitialize(t *testing.T) {
	a := newSrv(t)
	srvA := httptest.NewServer(NewHTTPHandler(a))
	defer srvA.Close()

	resp, _ := streamablePost(t, srvA.URL, "", rtok, reqBytes(1, "initialize", map[string]any{}))
	sid := resp.Header.Get("Mcp-Session-Id")
	resp, body := streamablePost(t, srvA.URL, sid, rtok, reqBytes(2, "tools/call", callParamsFor("runner__ping")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answered %d: %s", resp.StatusCode, body)
	}
	var r Response
	_ = json.Unmarshal(body, &r)
	if r.Error != nil {
		t.Fatalf("origin replica refused a call on the session it just minted: %d %s", r.Error.Code, r.Error.Message)
	}
}

// GUARD: the permissiveness above must NOT extend to a request that presents NO session at all.
// A bare tools/call with no Mcp-Session-Id mints a FRESH sConnected session and must still be the
// VR-H8 / UC-65 protocol error — otherwise the handshake requirement disappears entirely.
func TestStreamable_NoSessionHeaderStillRefusesToolsCall(t *testing.T) {
	a := newSrv(t)
	srvA := httptest.NewServer(NewHTTPHandler(a))
	defer srvA.Close()

	resp, body := streamablePost(t, srvA.URL, "", rtok, reqBytes(1, "tools/call", callParamsFor("runner__ping")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answered %d: %s", resp.StatusCode, body)
	}
	var r Response
	_ = json.Unmarshal(body, &r)
	if r.Error == nil || r.Error.Code != CodeNoSession {
		t.Fatalf("a tools/call with NO session id must be -32003 (VR-H8/UC-65); got %v", string(body))
	}
}

func TestStreamable_CrossReplicaSessionAdopted(t *testing.T) {
	// two REPLICAS: independent Server instances sharing NOTHING (like two CP pods).
	a := newSrv(t)
	b := newSrv(t)
	srvA := httptest.NewServer(NewHTTPHandler(a))
	defer srvA.Close()
	srvB := httptest.NewServer(NewHTTPHandler(b))
	defer srvB.Close()

	// initialize on A (mints the session id in the response header)
	resp, _ := streamablePost(t, srvA.URL, "", rtok, reqBytes(1, "initialize", map[string]any{}))
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize on A returned no Mcp-Session-Id")
	}
	_, _ = streamablePost(t, srvA.URL, sid, rtok, notifBytes("notifications/initialized", nil))

	// the SAME session id lands on B (a load balancer moved the client): B must ADOPT and serve.
	resp, body := streamablePost(t, srvB.URL, sid, rtok, reqBytes(2, "tools/call", callParamsFor("runner__ping")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replica B answered %d: %s", resp.StatusCode, body)
	}
	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if r.Error != nil {
		t.Fatalf("replica B rejected the adopted session: %+v (the streamable face must be replica-agnostic)", r.Error)
	}
}

func TestStreamable_FreshSessionStillRequiresHandshake(t *testing.T) {
	// A session id MINTED BY THIS replica (no client-supplied id) keeps the strict handshake:
	// tools/call before initialize is still a protocol error — adoption applies only to
	// CLIENT-SUPPLIED ids (the failover case), so first-time clients keep the negotiated bootup.
	a := newSrv(t)
	srvA := httptest.NewServer(NewHTTPHandler(a))
	defer srvA.Close()

	resp, body := streamablePost(t, srvA.URL, "", rtok, reqBytes(1, "tools/call", callParamsFor("runner__ping")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST answered %d: %s", resp.StatusCode, body)
	}
	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if r.Error == nil {
		t.Fatalf("a brand-new session skipped the handshake; the strict bootup must hold for minted ids")
	}
}

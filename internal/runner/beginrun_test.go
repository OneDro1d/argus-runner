package runner

// beginrun_test.go — Client.BeginRun (the direct-path fence call, W1/CP-M3-115). TEST-FIRST.
// Contract: 200 → nil (lock held); 409 → ErrCPBusy (a DISTINGUISHED sentinel — the caller refuses
// the run and surfaces *busy*); any transport/other failure → a plain error that is NOT ErrCPBusy
// (the caller may proceed offline — the documented CP-partition residual, local lock second line).

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func beginTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	return NewClient(srvURL, "inst-brc", priv)
}

func TestBeginRun_OKHoldsLock(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := beginTestClient(t, srv.URL)
	if err := c.BeginRun(context.Background(), "run-1", "full"); err != nil {
		t.Fatalf("BeginRun 200: err=%v; want nil", err)
	}
	if gotPath != "/fed/run/begin" {
		t.Fatalf("path = %q; want /fed/run/begin", gotPath)
	}
	if gotAuth == "" {
		t.Fatalf("no Authorization header — the begin must ride the machine JWT")
	}
}

func TestBeginRun_409IsErrCPBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"instance_busy"}`))
	}))
	defer srv.Close()
	c := beginTestClient(t, srv.URL)
	err := c.BeginRun(context.Background(), "run-2", "full")
	if !errors.Is(err, ErrCPBusy) {
		t.Fatalf("BeginRun 409: err=%v; want ErrCPBusy", err)
	}
}

func TestBeginRun_TransportErrorIsNotBusy(t *testing.T) {
	// A dead endpoint (closed server) = transport failure → generic error, NOT ErrCPBusy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	c := beginTestClient(t, srv.URL)
	err := c.BeginRun(context.Background(), "run-3", "full")
	if err == nil {
		t.Fatalf("BeginRun against a dead CP: err=nil; want a transport error")
	}
	if errors.Is(err, ErrCPBusy) {
		t.Fatalf("transport failure classified as ErrCPBusy — the caller would refuse instead of proceeding offline")
	}
}

func TestBeginRun_ServerErrorIsNotBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := beginTestClient(t, srv.URL)
	err := c.BeginRun(context.Background(), "run-4", "full")
	if err == nil || errors.Is(err, ErrCPBusy) {
		t.Fatalf("BeginRun 500: err=%v; want a non-busy error", err)
	}
}

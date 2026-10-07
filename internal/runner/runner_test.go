package runner

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestRunnerSkeleton_StartsAndServesHealth is the I.0 gate for the runner profile: the process
// bootstraps, serves the service-anatomy floor, and shuts down cleanly on ctx cancel.
func TestRunnerSkeleton_StartsAndServesHealth(t *testing.T) {
	srv, err := Bootstrap(context.Background(), Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if srv.Mode() != "runner" {
		t.Fatalf("Mode() = %q, want runner", srv.Mode())
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	base := "http://" + srv.Addr()
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/healthz", http.StatusOK},
		{"/health/live", http.StatusOK},
		{"/health", http.StatusOK},
	} {
		resp, err := http.Get(base + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Errorf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.code)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of ctx cancel (graceful shutdown failed)")
	}
}

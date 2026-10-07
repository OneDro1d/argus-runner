package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

func fakeMaterializeCP(t *testing.T, resp federation.MaterializeResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fed/materialize" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// The D-FED.3 hybrid source: FRESH when the CP answers (+ persists the cache) · CACHED with an honest
// stamp when the CP is unreachable · a structured REFUSAL with neither (UC059/UC060/UC061).
func TestResolveDirectSet_FreshCachedRefusal(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	dir := t.TempDir()
	cache := filepath.Join(dir, "set-cache.json")
	set := federation.MaterializeResponse{
		Scope: "full", SetHash: "h1",
		Scenarios: []federation.ScenarioPayload{{Path: "a/S1.md", Body: "# S1"}},
	}

	// 1) FRESH — CP reachable → fresh set, not cached, cache written.
	srv := fakeMaterializeCP(t, set)
	client := NewClient(srv.URL, "inst-1", priv)
	src, err := ResolveDirectSet(context.Background(), client, cache, "full", federation.Selection{}, 5*time.Second)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if src.Cached || src.SetHash != "h1" || len(src.Scenarios) != 1 || src.Scenarios[0].Path != "a/S1.md" {
		t.Fatalf("fresh wrong: %+v", src)
	}
	if _, err := readCache(cache); err != nil {
		t.Fatalf("fresh did not persist the cache: %v", err)
	}
	srv.Close()

	// 2) CACHED — CP now unreachable (closed) → the cached set, stamped.
	src2, err := ResolveDirectSet(context.Background(), client, cache, "full", federation.Selection{}, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if !src2.Cached || src2.Stamp != "cached (CP unreachable)" || src2.SetHash != "h1" || len(src2.Scenarios) != 1 {
		t.Fatalf("cached wrong: %+v", src2)
	}

	// 3) REFUSAL — no CP (nil client) + no cache → the structured error.
	_, err = ResolveDirectSet(context.Background(), nil, filepath.Join(dir, "missing.json"), "full", federation.Selection{}, time.Second)
	if !errors.Is(err, ErrNoSetAvailable) {
		t.Fatalf("refusal: got %v, want ErrNoSetAvailable", err)
	}
}

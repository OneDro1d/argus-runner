package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
)

// item 7b (msgbus tester 2026-09-28): "runner__run (relay) returns a run id but runs nothing" on
// a catalog-driven executor (empty local scenarios/) because the relay's own toolcore.Env carries
// no SetFetcher/SetHasher — it was built independently of the federated poll loop's client. These
// tests wire NewCommandFunc's new CatalogClient field against a fake control plane and prove the
// relay's env actually consults it (exercised through validate_config, item 8a's catalog-count
// fallback — the cheapest observable proof the wiring reaches toolcore), plus that a relayed run's
// toolcore.Run failure is logged rather than silently discarded.

func fakeCatalogCP(t *testing.T, scenarios []federation.ScenarioPayload) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/fed/materialize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(federation.MaterializeResponse{Scope: "full", SetHash: "h1", Scenarios: scenarios})
	})
	mux.HandleFunc("/fed/set-hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(federation.SetHashResponse{SetHash: "h1"})
	})
	return httptest.NewServer(mux)
}

// TestNewCommandFunc_ValidateConfig_WiresCatalogClient: with NO local scenarios and a CatalogClient
// wired, the relay's validate_config answer must count the catalog's build set (item 8a) — proving
// the relay's env actually carries a working SetFetcher, not just that ExecConfig has the field.
func TestNewCommandFunc_ValidateConfig_WiresCatalogClient(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	srv := fakeCatalogCP(t, []federation.ScenarioPayload{
		{Path: "a/ORDE-001.md", Body: "# S1"},
		{Path: "a/ORDE-002.md", Body: "# S2"},
	})
	defer srv.Close()
	client := NewClient(srv.URL, "inst-1", priv)

	dir := t.TempDir() // no local scenario files — the catalog-driven-executor shape
	cfgPath := dir + "/argus-config.yaml"
	writeFile(t, cfgPath, "project:\n  name: catalog-driven\n")

	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir(), ConfigPath: cfgPath, ScenariosDir: dir, CatalogClient: client})
	got, err := cf(context.Background(), "validate_config", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("NewCommandFunc validate_config: %v", err)
	}
	var out struct {
		ScenariosFound int `json:"scenarios_found"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v (got %s)", err, got)
	}
	if out.ScenariosFound != 2 {
		t.Errorf("scenarios_found = %d, want 2 (the catalog build set, via the wired CatalogClient) — got %s", out.ScenariosFound, got)
	}
}

// TestNewCommandFunc_ValidateConfig_NoCatalogClient_StaysEmptyLocalCount: unchanged behaviour when
// no CatalogClient is wired (every caller before this ticket, and any deployment where the
// executor has no CP identity yet).
func TestNewCommandFunc_ValidateConfig_NoCatalogClient_StaysEmptyLocalCount(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/argus-config.yaml"
	writeFile(t, cfgPath, "project:\n  name: standalone\n")

	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir(), ConfigPath: cfgPath, ScenariosDir: dir})
	got, err := cf(context.Background(), "validate_config", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("NewCommandFunc validate_config: %v", err)
	}
	var out struct {
		ScenariosFound int `json:"scenarios_found"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v (got %s)", err, got)
	}
	if out.ScenariosFound != 0 {
		t.Errorf("scenarios_found = %d, want 0 (no catalog wired)", out.ScenariosFound)
	}
}

// TestNewCommandFunc_Run_LogsFailureByRunID: a relayed `run` whose toolcore.Run fails (here: a
// ConfigPath that does not parse — deterministic, no JMeter needed) must be logged BY RUN ID
// through cfg.Log, not silently discarded. This is the exact silence the msgbus builder's relayed
// run hit: {running:true, run_id} returned, then NOTHING anywhere named why it never produced a
// report.
func TestNewCommandFunc_Run_LogsFailureByRunID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/argus-config.yaml"
	writeFile(t, cfgPath, "not: [valid yaml") // config.Load fails fast, before any JMeter/SUT call

	var mu sync.Mutex
	var lines []string
	done := make(chan struct{}, 1)
	logFn := func(format string, a ...any) {
		mu.Lock()
		lines = append(lines, sprintfCompat(format, a...))
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	}

	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir(), ConfigPath: cfgPath, Log: logFn})
	got, err := cf(context.Background(), "run", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("NewCommandFunc run: %v", err)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.RunID == "" {
		t.Fatal("run answered with no run_id")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the background run's failure to be logged")
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range lines {
		if strings.Contains(l, out.RunID) {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line named the failed run's id %q; lines=%v", out.RunID, lines)
	}
}

// TestNewCommandFunc_Run_NoLog_NeverPanics: cfg.Log unset (every pre-7b caller, and the existing
// TestNewCommandFunc_Run_ReturnsImmediately test) must not panic when the background run fails.
func TestNewCommandFunc_Run_NoLog_NeverPanics(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/argus-config.yaml"
	writeFile(t, cfgPath, "not: [valid yaml")

	cf := NewCommandFunc(ExecConfig{ToolInstance: "local", ResultsRoot: t.TempDir(), ConfigPath: cfgPath})
	got, err := cf(context.Background(), "run", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("NewCommandFunc run: %v", err)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.RunID == "" {
		t.Fatal("run answered with no run_id")
	}
	time.Sleep(50 * time.Millisecond) // let the background goroutine run to completion; no assertion needed beyond "did not panic"
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sprintfCompat(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}

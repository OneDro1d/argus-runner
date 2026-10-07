package runner

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneDro1d/argus-runner/internal/federation"
	"github.com/OneDro1d/argus-runner/internal/report"
)

// D3 (M3 fix plan R10): ReportUp is the "ALL RUNS REPORT UP" path for a DIRECT in-env run (the MCP
// runner__run). It maps the report to the evidence-free ResultsPush and pushes it under the run's OWN
// run_id with NO run_request_id (a direct run is not enqueued). Evidence never crosses.
func TestReportUp_PushesDirectRunNoRunRequestID(t *testing.T) {
	var got federation.ResultsPush
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fed/results" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	_, priv, _ := ed25519.GenerateKey(nil)
	client := NewClient(srv.URL, "inst-1", priv)
	rep := &report.Report{
		Summary: report.Summary{Passed: 2, Failed: 1, Total: 3},
		Layers: []report.Layer{{Layer: "http", Scenarios: []report.ScenarioResult{
			{ID: "A-1", Status: "passed"},
			{ID: "A-2", Status: "failed", Failure: &report.Failure{Observed: "boom"}},
		}}},
	}
	if err := client.ReportUp(context.Background(), rep, "20260715T120000", "layer", "sh-1", "http://g/d?x=1", federation.Annotations{}, time.Time{}, time.Time{}); err != nil {
		t.Fatalf("ReportUp: %v", err)
	}
	if got.RunID != "20260715T120000" {
		t.Errorf("run_id = %q, want 20260715T120000", got.RunID)
	}
	if got.RunRequestID != "" {
		t.Errorf("run_request_id = %q, want EMPTY (a direct run is not enqueued)", got.RunRequestID)
	}
	if got.Scope != "layer" || got.SetHash != "sh-1" || got.DeepLink != "http://g/d?x=1" {
		t.Errorf("scope/setHash/deepLink = %q/%q/%q, want layer/sh-1/http://g/d?x=1", got.Scope, got.SetHash, got.DeepLink)
	}
	if got.Tallies.Passed != 2 || got.Tallies.Failed != 1 || got.Tallies.Total != 3 {
		t.Errorf("tallies = %+v, want 2/1/3", got.Tallies)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed (the report has a failed scenario)", got.Status)
	}
	if gotAuth == "" {
		t.Error("push must be signed (Authorization header present)")
	}
	// evidence-free: the failure detail must NOT appear anywhere on the wire.
	raw, _ := json.Marshal(got)
	if string(raw) != "" && (contains(raw, "boom")) {
		t.Fatalf("SECURITY: the ResultsPush leaked failure evidence: %s", raw)
	}
	if len(got.Scenarios) != 2 || got.Scenarios[0].ID != "A-1" || got.Scenarios[1].Outcome != "failed" {
		t.Errorf("scenarios = %+v, want [A-1 passed, A-2 failed] (outcome only)", got.Scenarios)
	}
}

func contains(b []byte, sub string) bool {
	return len(sub) > 0 && bytesIndex(b, sub) >= 0
}

func bytesIndex(b []byte, sub string) int {
	s := string(b)
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// LoadKey loads an EXISTING machine key (no create) so the in-env MCP server signs pushes as the SAME
// registered instance as the executor (shared identity file). It errors when the key is absent.
func TestLoadKey_LoadOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "identity.key")
	if _, err := LoadKey(p); err == nil {
		t.Fatal("LoadKey on a missing file must error (load-only, never create)")
	}
	created, err := LoadOrCreateKey(p) // the executor path creates + persists
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadKey(p)
	if err != nil {
		t.Fatalf("LoadKey after create: %v", err)
	}
	if base64.StdEncoding.EncodeToString(loaded) != base64.StdEncoding.EncodeToString(created) {
		t.Fatal("LoadKey returned a different key than the one persisted — the two containers would not share an identity")
	}
}

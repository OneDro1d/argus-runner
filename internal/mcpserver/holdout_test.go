package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/auth"
	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/toolcore"
)

const sentinelExpected = "SENTINEL-EXPECTED-9F3A"

// writeFixtureReport drops a report.json with one FAILED scenario whose
// failure.expected carries a sentinel value, and returns an Env pointing at it.
func writeFixtureReport(t *testing.T) toolcore.Env {
	t.Helper()
	tmp := t.TempDir()
	exp := sentinelExpected
	rep := report.Report{
		Project: "fixture", Summary: report.Summary{Total: 1, Failed: 1},
		Layers: []report.Layer{{Layer: "http-ingestion", Scenarios: []report.ScenarioResult{
			{ID: "ORD-X", Status: "failed", CorrelationID: "tr-x",
				Failure: &report.Failure{Expected: &exp, Observed: "responder returned status 200"}},
		}}},
	}
	b, _ := json.MarshalIndent(&rep, "", "  ")
	dir := filepath.Join(tmp, "local")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return toolcore.Env{Instance: "local", ResultsRoot: tmp, ScenariosDir: tmp,
		Grafana: "http://grafana", Loki: "http://127.0.0.1:9"}
}

func callGetReport(t *testing.T, env toolcore.Env, token string) []byte {
	t.Helper()
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, DefaultTools(env)...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "s", token)
	raw, _ := s.Dispatch("s", token, reqBytes(1, "tools/call", map[string]any{
		"name":      "runner__get_report",
		"arguments": map[string]any{"instance_id": "local"},
	}))
	return raw
}

// VR-C8 (through the MCP envelope): after get_failure_context was removed (M25-FX4 C1),
// get_report is the sole holdout point — the product-hat envelope must carry the expected
// value NOWHERE.
func TestGetReport_ProductHat_NoExpectedThroughEnvelope(t *testing.T) {
	env := writeFixtureReport(t)
	raw := callGetReport(t, env, rtok) // product (runner) token
	if strings.Contains(string(raw), sentinelExpected) {
		t.Fatalf("SECURITY: product-hat get_report envelope leaked the expected value:\n%s", raw)
	}
	if r := decode(t, raw); r.Error != nil {
		t.Fatalf("get_report must still succeed for the product hat: %+v", r.Error)
	}
}

// Positive control: the test hat DOES keep the expected value in the report.
func TestGetReport_TestHat_HasExpected(t *testing.T) {
	env := writeFixtureReport(t)
	raw := callGetReport(t, env, atok) // test (author) token
	if !strings.Contains(string(raw), sentinelExpected) {
		t.Fatalf("test hat must keep the expected value in get_report:\n%s", raw)
	}
}

// C1: get_failure_context is removed — it must not appear in the tool set in either hat.
func TestGetFailureContextToolRemoved(t *testing.T) {
	for _, tl := range DefaultTools(toolcore.Env{Instance: "local"}) {
		if tl.Name == "runner__get_failure_context" {
			t.Fatal("runner__get_failure_context must be removed (M25-FX4 C1)")
		}
	}
}

// C2 (M3 fix plan, R7 addendum): runner__get_sagas is OBSERVED saga data (from Loki), NOT the
// dark-factory holdout — it carries no scenario EXPECT to redact and is visible to BOTH hats identically.
// This locks that boundary: get_sagas is runner-scope (never author-scope), so the PRODUCT hat is not
// denied it, and the report's EXPECT never leaks through it (get_sagas reads Loki, never the report).
// The real holdout — get_report EXPECT redaction — is covered by TestGetReport_ProductHat_* above.
func TestGetSagas_ObservedData_NotAuthorScopedNoExpect(t *testing.T) {
	env := writeFixtureReport(t) // report.json carries the sentinel EXPECT; Loki points at an unreachable addr
	tools := DefaultTools(env)
	var sagas *Tool
	for i := range tools {
		if tools[i].Name == "runner__get_sagas" {
			sagas = &tools[i]
		}
	}
	if sagas == nil {
		t.Fatal("runner__get_sagas must be registered")
	}
	if sagas.Namespace != NSRunner {
		t.Fatalf("get_sagas namespace = %q, want %q (observed data, NOT the author holdout)", sagas.Namespace, NSRunner)
	}
	s, err := NewServer(auth.Config{RunnerToken: rtok, AuthorToken: atok}, tools...)
	if err != nil {
		t.Fatal(err)
	}
	handshake(t, s, "p", rtok) // PRODUCT token
	raw, _ := s.Dispatch("p", rtok, reqBytes(1, "tools/call", map[string]any{
		"name":      "runner__get_sagas",
		"arguments": map[string]any{"instance_id": "local", "correlation_id": "tr-x"},
	}))
	// the product hat must NOT be denied get_sagas (unlike an author tool) — it is observed data.
	if r := decode(t, raw); r.Error != nil && r.Error.Code == CodeUnauthorized {
		t.Fatalf("product hat must NOT be denied get_sagas (observed data, not the holdout): %+v", r.Error)
	}
	// and the report's EXPECT sentinel must never appear in saga output (saga data comes from Loki).
	if strings.Contains(string(raw), sentinelExpected) {
		t.Fatalf("SECURITY: get_sagas leaked the report's EXPECT sentinel:\n%s", raw)
	}
}

// C1 (M3 fix plan, R7): the IN-ENV server is RUNNER-ONLY. It must expose exactly the 6
// runner__* tools and ZERO author__* tools — authoring is a cloud-plane capability now
// (doc ruling: in-env = 6 runner locked; authoring → cloud registry, not disk). An in-env
// author tool is a scope-boundary breach: it would let the test agent write scenarios to
// DISK instead of the cloud catalog (the M2.5 leftover this fix removes).
func TestDefaultTools_InEnvRunnerOnly(t *testing.T) {
	tools := DefaultTools(toolcore.Env{Instance: "local"})
	var runnerNames, authorNames []string
	for _, tl := range tools {
		switch tl.Namespace {
		case NSRunner:
			runnerNames = append(runnerNames, tl.Name)
		case NSAuthor:
			authorNames = append(authorNames, tl.Name)
		default:
			t.Errorf("tool %q has an unexpected namespace %q", tl.Name, tl.Namespace)
		}
	}
	if len(authorNames) != 0 {
		t.Fatalf("SCOPE: the in-env server must expose ZERO author__ tools (C1); found %d: %v", len(authorNames), authorNames)
	}
	want := []string{
		"runner__validate_config", "runner__run", "runner__get_report",
		"runner__get_sagas", "runner__get_tail_logs", "runner__get_dashboard_url",
	}
	if len(runnerNames) != len(want) {
		t.Fatalf("the in-env server must expose exactly %d runner tools, got %d: %v", len(want), len(runnerNames), runnerNames)
	}
	have := make(map[string]bool, len(runnerNames))
	for _, n := range runnerNames {
		have[n] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing runner tool %q from the in-env server", w)
		}
	}
}

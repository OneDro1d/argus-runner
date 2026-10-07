package toolcore

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// T4.2: the matrix a stack reads to learn what it can test, from validate-config alone.

func matrixRows(t *testing.T, body string) (map[string]CapabilityRow, map[string]any) {
	t.Helper()
	t.Setenv(probeDisableEnv, "1") // the matrix is derived from the file; no dial is needed or wanted
	p, _, err := ValidateConfig(capEnv(t, body))
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	m := p.(map[string]any)
	rows, ok := m["capability_matrix"].([]CapabilityRow)
	if !ok {
		t.Fatalf("validate-config must carry capability_matrix, got %T", m["capability_matrix"])
	}
	// All eight layers, in the documented order, whatever the config declares — a layer missing from
	// the matrix is the "discovered scenario by scenario" failure this ticket exists to remove.
	if len(rows) != len(matrixLayers) {
		t.Fatalf("want %d rows, got %d: %+v", len(matrixLayers), len(rows), rows)
	}
	by := map[string]CapabilityRow{}
	for i, r := range rows {
		if r.Layer != matrixLayers[i] {
			t.Errorf("row %d: want layer %q, got %q", i, matrixLayers[i], r.Layer)
		}
		if r.Needs == "" || r.Reason == "" {
			t.Errorf("%s: every row must say what it needs and why — got %+v", r.Layer, r)
		}
		by[r.Layer] = r
	}
	return by, m
}

func wantStatus(t *testing.T, by map[string]CapabilityRow, layer, status string) {
	t.Helper()
	if got := by[layer].Status; got != status {
		t.Errorf("%s: want %s, got %s (%s)", layer, status, got, by[layer].Reason)
	}
}

// The no-bus HTTP+DB shape (T4.3's starter pack): Message Flow is unavailable and says why, and the
// config stays VALID — a layer the SUT does not have is not a defect in the file.
func TestCapabilityMatrix_NoBusStack(t *testing.T) {
	by, m := matrixRows(t, "project:\n  name: items\ntargets:\n  http:\n    base_url: http://items:8080\n  database:\n    type: postgres\n    jdbc_url: jdbc:postgresql://items-db:5432/items\n  auth:\n    type: bearer\n    bearer_token: tok\n")
	for _, l := range []string{"HTTP Ingestion", "Database State", "Error Path", "Rate Limiting", "Permissions"} {
		wantStatus(t, by, l, CapAvailable)
	}
	wantStatus(t, by, "Message Flow", CapUnavailable)
	wantStatus(t, by, "External Delivery", CapUnavailable)
	wantStatus(t, by, "Web UI", CapOutsideConfig)
	if r := by["Message Flow"]; r.Needs != "targets.message_broker" || !strings.Contains(r.Reason, "AMQP") {
		t.Errorf("Message Flow must name the missing target and the only engine: %+v", r)
	}
	if m["valid"] != true {
		t.Errorf("an unavailable layer is information, never an error: valid=%v errors=%v", m["valid"], m["errors"])
	}
	s, _ := m["capability_summary"].(string)
	// the tenth layer, HTTP Load, needs a NAMED http target, which this stack does not declare.
	for _, want := range []string{"5 of 10 layers testable", "not testable: Message Flow, External Delivery, AMQP Load, HTTP Load", "decided outside the config: Web UI"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q: %s", want, s)
		}
	}
}

// the HTTP Load row needs a NAMED http target, and says when no load_allowed_targets entry names one.
func TestCapabilityMatrix_HTTPLoad(t *testing.T) {
	plain, _ := matrixRows(t, "project:\n  name: p\ntargets:\n  http:\n    base_url: http://items:8080\n")
	wantStatus(t, plain, "HTTP Load", CapUnavailable)
	if r := plain["HTTP Load"]; !strings.Contains(r.Reason, "the plain targets.http slot is never a load target") {
		t.Errorf("plain slot only: %+v", r)
	}
	named, _ := matrixRows(t, "project:\n  name: p\ntargets:\n  http_targets:\n    api-lab:\n      base_url: http://api-lab:8080\n")
	wantStatus(t, named, "HTTP Load", CapAvailable)
	if r := named["HTTP Load"]; !strings.Contains(r.Reason, "every HTTP Load run is refused until the operator marks") {
		t.Errorf("named target, nothing allowed: %+v", r)
	}
	allowed, _ := matrixRows(t, "project:\n  name: p\ntargets:\n  http_targets:\n    api-lab:\n      base_url: http://api-lab:8080\nload_allowed_targets:\n  api-lab: {}\n")
	if r := allowed["HTTP Load"]; r.Status != CapAvailable || strings.Contains(r.Reason, "refused") {
		t.Errorf("named and allowed: %+v", r)
	}
}

// A pure-MCP SUT (Memstore / Social shape): HTTP Ingestion is testable ONLY through MCP, and the row
// must say so — "available" alone would send a plain-HTTP scenario to a refusal.
func TestCapabilityMatrix_PureMCP(t *testing.T) {
	by, _ := matrixRows(t, "project:\n  name: memstore\ntargets:\n  mcp:\n    base_url: http://memstore-gateway:8090/mcp\n    transport: streamable-http\n    auth:\n      type: none\n")
	// The four layers whose whole check is the call and its response are covered by MCP — including
	// Permissions with no targets.auth, which is how Memstore's valid pack runs its Permissions scenarios.
	for _, l := range []string{"HTTP Ingestion", "Error Path", "Rate Limiting", "Permissions"} {
		wantStatus(t, by, l, CapAvailable)
		if r := by[l]; !strings.Contains(r.Reason, "mcp/chain scenarios only") {
			t.Errorf("%s via MCP alone must say it covers mcp/chain scenarios only: %+v", l, r)
		}
	}
	// A layer that reads something besides the response is NOT covered by MCP.
	for _, l := range []string{"Database State", "Message Flow", "External Delivery"} {
		wantStatus(t, by, l, CapUnavailable)
	}
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// A NAMED entry covers its layer exactly as SelectTarget lets it cover a scenario — a config with only
// message_broker_targets has Message Flow, and the row names the entry the operator wrote.
func TestCapabilityMatrix_NamedTargetCounts(t *testing.T) {
	by, _ := matrixRows(t, "project:\n  name: audit\ntargets:\n  http:\n    base_url: http://svc:8080\n  message_broker_targets:\n    audit:\n      url: amqp://guest:guest@audit-rabbit:5672/\n      management_url: http://audit-rabbit:15672\n      queues:\n        incoming: audit.q\n")
	wantStatus(t, by, "Message Flow", CapAvailable)
	if r := by["Message Flow"]; !strings.Contains(r.Reason, "audit") || strings.Contains(r.Reason, "targets.message_broker declared") {
		t.Errorf("a named-only broker must be credited to the named entry, not to a plain slot that is absent: %+v", r)
	}
}

// Nothing to trigger with: every request-driven layer is unavailable — and still no error from the
// matrix itself (a config with no trigger is refused elsewhere, by scenario, if at all).
func TestCapabilityMatrix_NoTrigger(t *testing.T) {
	by, _ := matrixRows(t, "project:\n  name: bare\ntargets:\n  database:\n    jdbc_url: jdbc:postgresql://db:5432/x\n")
	for _, l := range []string{"HTTP Ingestion", "Error Path", "Rate Limiting"} {
		wantStatus(t, by, l, CapUnavailable)
	}
	wantStatus(t, by, "Database State", CapAvailable)
}

// Web UI is never answered yes/no from the config: its page comes from the scenario or APP_URL.
func TestCapabilityMatrix_WebUIReportsAPP_URL(t *testing.T) {
	t.Setenv("APP_URL", "")
	by, _ := matrixRows(t, "project:\n  name: ui\ntargets:\n  http:\n    base_url: http://svc:8080\n")
	if r := by["Web UI"]; r.Status != CapOutsideConfig || !strings.Contains(r.Reason, "APP_URL is not set") {
		t.Errorf("unset APP_URL: %+v", r)
	}
	t.Setenv("APP_URL", "http://spa:5190")
	by, _ = matrixRows(t, "project:\n  name: ui\ntargets:\n  http:\n    base_url: http://svc:8080\n")
	if r := by["Web UI"]; r.Status != CapOutsideConfig || !strings.Contains(r.Reason, "APP_URL is set") {
		t.Errorf("set APP_URL must still not claim the layer is ready, only report it: %+v", r)
	}
}

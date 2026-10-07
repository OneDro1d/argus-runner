package toolcore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// item 8a (msgbus tester 2026-09-28): "runner__validate_config reports scenarios_found: 0" for a
// catalog-driven executor (k8s, where /scenarios is a per-pod emptyDir — the SAME trap the RUN path
// already refuses in resolveScenarioDir). scenarios_found must count the catalog build set when the
// local walk finds nothing and a catalog is wired (item 7b's SetFetcher), never just report 0.

func TestValidateConfig_EmptyLocalDirWithCatalogWired_CountsTheCatalogSet(t *testing.T) {
	dir := t.TempDir() // scenarios/ is EMPTY — the catalog-driven-executor shape
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: catalog-driven\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	e := Env{ConfigPath: cfgPath, ScenariosDir: dir, SetFetcher: func(ctx context.Context, layer, tag, scenarioID string) ([]CatalogScenario, error) {
		calls++
		if layer != "" || tag != "" || scenarioID != "" {
			t.Errorf("SetFetcher called with a filter (layer=%q tag=%q scenario=%q), want the FULL build set", layer, tag, scenarioID)
		}
		return []CatalogScenario{
			{Path: "a/ORDE-001.md", Body: "# S1"},
			{Path: "a/ORDE-002.md", Body: "# S2"},
			{Path: "a/ORDE-003.md", Body: "# S3"},
		}, nil
	}}

	p, _, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	m := p.(map[string]any)
	if calls != 1 {
		t.Fatalf("SetFetcher called %d times, want exactly 1", calls)
	}
	if got, ok := m["scenarios_found"].(int); !ok || got != 3 {
		t.Errorf("scenarios_found = %v, want 3 (the catalog's build set, not the empty local dir)", m["scenarios_found"])
	}
}

// TestValidateConfig_NoCatalogWired_EmptyLocalDirStaysZero: unchanged pre-8a behaviour — no
// SetFetcher means there is nothing to ask, and scenarios_found stays the honest local 0.
func TestValidateConfig_NoCatalogWired_EmptyLocalDirStaysZero(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: standalone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, err := ValidateConfig(Env{ConfigPath: cfgPath, ScenariosDir: dir})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	m := p.(map[string]any)
	if got, ok := m["scenarios_found"].(int); !ok || got != 0 {
		t.Errorf("scenarios_found = %v, want 0 (no catalog wired)", m["scenarios_found"])
	}
}

// TestValidateConfig_NonEmptyLocalDir_NeverConsultsTheCatalog: a real local set still wins
// outright (VR-F6's own posture) — the catalog is never even asked when the local walk found
// something, so a filtered/offline local set is not silently overridden by SetFetcher's answer.
func TestValidateConfig_NonEmptyLocalDir_NeverConsultsTheCatalog(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte("project:\n  name: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeChainScenario(t, dir, "CHN-010", "Message Flow", chainOnlyAMQPHTTPTrigger, chainOnlyAMQPHTTPExpect)
	called := false
	e := Env{ConfigPath: cfgPath, ScenariosDir: dir, SetFetcher: func(context.Context, string, string, string) ([]CatalogScenario, error) {
		called = true
		return nil, nil
	}}
	p, _, err := ValidateConfig(e)
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	if called {
		t.Error("SetFetcher was consulted although the local walk already found a scenario — local must win without a network round trip")
	}
	m := p.(map[string]any)
	if got, ok := m["scenarios_found"].(int); !ok || got != 1 {
		t.Errorf("scenarios_found = %v, want 1 (the local scenario)", m["scenarios_found"])
	}
}

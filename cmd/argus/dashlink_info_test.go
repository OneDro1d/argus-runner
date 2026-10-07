package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
)

// (UI-2, A'2): an environment that declares observability.dashboard_link.template needs no
// Grafana base, so the executor must not warn "no dashboard link will be produced" for it.

func tmplCfg(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "argus-config.yaml")
	if err := os.WriteFile(p, []byte("project:\n  name: p\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseUnresolved(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGrafanaResolution_TemplateDeclaredIsInfoNotWarn(t *testing.T) {
	c := tmplCfg(t, "observability:\n  dashboard_link:\n    template: \"https://g.example/d/x?r={run_id}\"\n")
	base, ev := grafanaResolution(c, "managed")
	if base != "" {
		t.Errorf("no per-tier base declared, want empty: %q", base)
	}
	if _, warn := ev["warn"]; warn || ev == nil {
		t.Fatalf("a declared template must not warn: %v", ev)
	}
	if got, _ := ev["info"].(string); got != "dashboard links come from observability.dashboard_link.template" {
		t.Errorf("info = %q", got)
	}
}

func TestGrafanaResolution_NoTemplateStillWarns(t *testing.T) {
	c := tmplCfg(t, "observability:\n  grafana:\n    public_url: {compose: http://localhost:3000}\n")
	_, ev := grafanaResolution(c, "managed")
	if got, _ := ev["warn"].(string); got != "no dashboard link will be produced" {
		t.Fatalf("the warning must stay for an environment with neither: %v", ev)
	}
	if r, _ := ev["reason"].(string); !strings.Contains(r, "managed") {
		t.Errorf("reason lost: %v", ev)
	}
}

func TestGrafanaResolution_TierDeclaredIsSilent(t *testing.T) {
	c := tmplCfg(t, "observability:\n  grafana:\n    public_url: {managed: \"https://grafana.example.com\"}\n  dashboard_link:\n    template: \"https://g.example/x\"\n")
	base, ev := grafanaResolution(c, "managed")
	if base != "https://grafana.example.com" || ev != nil {
		t.Fatalf("base %q event %v", base, ev)
	}
}

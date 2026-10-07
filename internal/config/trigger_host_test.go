package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC-D21 — a TRIGGER URL's host is never used: the runner keeps only its path (PathFromURL) and
// takes the host from targets.http.base_url, or from the entry a **Target** names. A scenario
// written against a second host with no **Target** therefore probes the WRONG app, and nothing
// said so (Shop health on argus-home, 09-18: 3/6 "failures" that were all requests to retail).
// validate-config must name it.
func writeHTTPScenario(t *testing.T, dir, id, target, url string) {
	t.Helper()
	meta := "- **ID**: " + id + "\n- **Layer**: HTTP Ingestion\n"
	if target != "" {
		meta += "- **Target**: " + target + "\n"
	}
	md := "# Scenario: h\n\n## Metadata\n" + meta + "- **Tags**: http\n\n## TRIGGER\nGET `" + url + "`\n\n" +
		"## EXPECT\n### Runnable\n- status=200\n"
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

const twoHostConfig = "project:\n  name: p\ntargets:\n  http:\n    base_url: https://retail.example\n" +
	"  http_targets:\n    utils:\n      base_url: https://utils.example\nscenarios:\n  timeout_default: 20s\n"

func TestTriggerHostWarnings_NamesAScenarioWhoseHostTheRunWillNotUse(t *testing.T) {
	dir := t.TempDir()
	writeHTTPScenario(t, dir, "H-1", "", "https://utils.example/health") // wrong: no Target, goes to retail
	c := loadYAML(t, twoHostConfig)
	w := c.TriggerHostWarnings(dir)
	if len(w) != 1 || !strings.Contains(w[0], "H-1") || !strings.Contains(w[0], "utils.example") ||
		!strings.Contains(w[0], "retail.example") {
		t.Fatalf("want one warning naming H-1, the written host and the host actually used; got %q", w)
	}
}

func TestTriggerHostWarnings_QuietWhenTheHostIsTheOneUsed(t *testing.T) {
	dir := t.TempDir()
	writeHTTPScenario(t, dir, "H-2", "", "https://retail.example/health")         // matches base_url
	writeHTTPScenario(t, dir, "H-3", "utils", "https://utils.example/health")     // Target picks it
	writeHTTPScenario(t, dir, "H-4", "", "${RETAIL_URL}/health")                  // placeholder: no host written
	writeHTTPScenario(t, dir, "H-5", "", "/health")                               // a bare path
	writeHTTPScenario(t, dir, "H-6", "", "https://RETAIL.example:443/api/health") // same host, spelled differently
	c := loadYAML(t, twoHostConfig)
	if w := c.TriggerHostWarnings(dir); len(w) != 0 {
		t.Fatalf("no warning expected; got %q", w)
	}
}

func TestTriggerHostWarnings_ATargetPointingElsewhereIsAlsoNamed(t *testing.T) {
	dir := t.TempDir()
	writeHTTPScenario(t, dir, "H-7", "utils", "https://retail.example/health") // Target and URL disagree
	c := loadYAML(t, twoHostConfig)
	if w := c.TriggerHostWarnings(dir); len(w) != 1 || !strings.Contains(w[0], "H-7") {
		t.Fatalf("want one warning naming H-7; got %q", w)
	}
}

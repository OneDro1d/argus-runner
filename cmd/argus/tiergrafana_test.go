package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TS-F6 / VR-E8 / VR-F9 — INT-008, FOURTH incarnation.
//
// The ledger records INT-008 as "fixed three times before it actually worked", and each earlier
// round was found the same way: by resolving the REAL configs instead of reasoning about the code.
// This is the fourth, found by asking the live managed executor two questions and getting two
// different answers:
//
//	runner__get_dashboard_url (MCP surface) -> https://grafana.example.com/d/argus-overview-social-aks-v1?…
//	the deep_link stored with its last run  -> http://localhost:3000/d/argus-overview-social-aks-v1?…
//
// SAME PROCESS. Same config. Same tier. `cmdServeMode` resolved the per-tier Grafana into the env it
// handed the MCP tools, and built the runner's ExecConfig fifteen lines EARLIER from the raw
// `--grafana` flag — which onboarding sets to the compose default `http://localhost:3000` on every
// tier. So the tool told the truth and the durable record lied, and the record is the one an operator
// opens during triage, days later, from the Runs page.
//
// One value, resolved once, used by both. That is the whole fix, and the structural assertion below
// is what keeps it that way: the defect is not a wrong computation, it is a SECOND SOURCE for a value
// that must have one.

// ── 1. the resolver itself, against the exact shape the live estate uses ─────────────────────────

func TestTierGrafana_ResolvesTheManagedTierForAnAksExecutor(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	// The per-tier map exactly as social-aks-v1's ConfigMap carries it.
	if err := os.WriteFile(cfgPath, []byte(`project:
  name: social
targets:
  mcp:
    base_url: http://social-gateway:8080/mcp
    transport: streamable-http
observability:
  grafana:
    dashboard_template: dashboards/argus-overview.json
    public_url:
      compose: http://localhost:3000
      k3d:     http://localhost:3000
      managed: https://grafana.example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// ARGUS_TIER is the CONCRETE cluster flavour the onboarder speaks ("aks"); the config declares
	// the CANONICAL tier ("managed"). env() folds it via federation.WireTier — if that ever stops
	// happening, this test says so rather than the estate discovering it months later.
	t.Setenv("ARGUS_TIER", "aks")

	cf := &commonFlags{configPath: cfgPath, grafana: "http://localhost:3000", instance: "local"}
	got := tierGrafana(cf)

	if got != "https://grafana.example.com" {
		t.Errorf("tierGrafana = %q, want the MANAGED entry \"https://grafana.example.com\"\n"+
			"  ARGUS_TIER=aks must fold to the canonical 'managed' before the lookup", got)
	}
	if got == "http://localhost:3000" {
		t.Error("resolved to the compose default on a managed tier — this is INT-008 exactly")
	}
}

// A tier whose entry is absent must resolve to ABSENCE, not to the flag's compose default. A
// link-shaped string that opens nothing is worse than no link: it survives into a report and is
// clicked during triage.
func TestTierGrafana_MissingTierEntryYieldsAbsenceNotTheComposeDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`project:
  name: p
observability:
  grafana:
    public_url:
      compose: http://localhost:3000
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_TIER", "aks") // declared: compose only

	cf := &commonFlags{configPath: cfgPath, grafana: "http://localhost:3000", instance: "local"}
	if got := tierGrafana(cf); got != "" {
		t.Errorf("tierGrafana = %q, want \"\" — the managed entry is absent, so there is no link to give.\n"+
			"  Falling back to the flag would hand an operator a localhost URL for a cloud instance", got)
	}
}

// The NEGATIVE CONTROL for the two above: the compose tier still resolves normally. Without this a
// resolver that always returned "" would satisfy the absence test.
func TestTierGrafana_ComposeTierStillResolves(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "argus-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`project:
  name: p
observability:
  grafana:
    public_url:
      compose: http://localhost:3000
      managed: https://grafana.example.com
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGUS_TIER", "compose")

	cf := &commonFlags{configPath: cfgPath, grafana: "http://ignored:9999", instance: "local"}
	if got := tierGrafana(cf); got != "http://localhost:3000" {
		t.Errorf("tierGrafana = %q, want the compose entry", got)
	}
}

// ── 2. THE REGRESSION GUARD: one value, one source ──────────────────────────────────────────────
//
// The bug was never a wrong computation — resolveTierGrafana was correct all along. It was a SECOND
// SOURCE for the same value: the federated ExecConfig read `cf.grafana` directly while the MCP env
// read the resolved one. A behavioural test cannot see that without standing up a whole serve
// process, so this reads the source, in the style of doclint_test.go.
func TestServeMode_ExecConfigUsesTheResolvedGrafanaNotTheRawFlag(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)

	// EVERY runner.ExecConfig literal must take a RESOLVED base. There are two — the federated one in
	// cmdServeMode and the direct one in cmdRunDirect — and BOTH store deep_links, so both matter.
	// The first draft of this test only looked at the first literal it found; that is what surfaced
	// cmdRunDirect as a second, independent instance of the same defect. Check them all.
	//
	// env() legitimately holds `Grafana: cf.grafana` and is deliberately NOT checked here:
	// resolveTierGrafana overwrites it on the next line. An ExecConfig literal has no such follow-up —
	// whatever it is given is what lands in the ledger.
	for i, rest := 0, s; ; i++ {
		k := strings.Index(rest, "runner.ExecConfig{")
		if k < 0 {
			if i == 0 {
				t.Fatal("no runner.ExecConfig literal found — this test has lost its subject")
			}
			break
		}
		rest = rest[k+len("runner.ExecConfig{"):]
		end := strings.Index(rest, "}")
		if end < 0 {
			t.Fatal("unterminated runner.ExecConfig literal")
		}
		// `Grafana: cf.grafana`, not a bare `cf.grafana` — the field ASSIGNMENT. Matching the bare
		// identifier also matched the prose in the comments explaining why it must not be used, so the
		// fix tripped its own test.
		if strings.Contains(rest[:end], "Grafana: cf.grafana") {
			t.Errorf("runner.ExecConfig #%d takes Grafana straight from the --grafana flag.\n"+
				"  On a k8s tier that flag is the compose default (http://localhost:3000), so every\n"+
				"  deep_link this executor STORES points at the operator's own machine — INT-008.\n"+
				"  Use tierGrafana(cf).\n  literal: %s", i+1, strings.TrimSpace(rest[:end]))
		}
	}

	// cmdServeMode must NOT resolve a second time: it shares the one value with the ExecConfig above.
	// A second resolution there is precisely how the two copies drifted apart.
	if strings.Contains(s, "resolveTierGrafana(&e) // VR-E8 / FX-1: deep links are for a HUMAN browser") {
		t.Error("cmdServeMode resolves the Grafana base separately again — one process, one value")
	}
	if !strings.Contains(s, "func tierGrafana(cf *commonFlags) string") {
		t.Error("the single-source helper tierGrafana is gone")
	}
}

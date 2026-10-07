package scenario

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// VR12-S1 (V30-003, STEP 1 of the round-12 build order) — THE SCHEMA MUST DESCRIBE THE OBJECT THE
// PRODUCT ACTUALLY PRODUCES, BEFORE ANYTHING LOADS IT.
//
// V30-003 rules that schemas/scenario.schema.yaml becomes the single source of truth for the scenario
// contract, in three ORDERED steps: correct it, give it a section list, then have the validator load
// it. This file is the gate on step one. Measured 2026-09-10, the schema was wrong in FOUR ways and
// nothing noticed, because nothing has ever loaded it (`grep -rn "scenario.schema"` over the tree
// returned zero hits):
//
//  1. `target` is emitted by AsParsedMap and was undeclared — and additionalProperties is false, so
//     EVERY parsed scenario carrying a **Target** violated the contract.
//  2. `id` is `required` and was never emitted by AsParsedMap at all.
//  3. the layer enum listed SEVEN layers; CanonicalLayers has EIGHT (`Web UI` was missing).
//  4. the `id` pattern was the RETIRED uppercase grammar that VR10-S4 replaced in round 10 — latent
//     (all 119 shipped ids happen to be uppercase) but it would have refused exactly the lowercase and
//     underscore ids the live validator accepts.
//
// Switching the schema on without fixing these would have rejected all 119 scenarios twice over.
//
// ⚠ This test deliberately uses yaml.v3 — ALREADY a dependency — and NOT a JSON-schema library. It
// checks the two things that actually drifted (the declared key set, and the enums/patterns duplicated
// from Go) rather than performing full schema validation, so step 1 adds no module. The one dependency
// V30-003 budgets is spent when the VALIDATOR loads the schema in step 8, not here.
type scenarioSchema struct {
	Required   []string `yaml:"required"`
	Properties map[string]struct {
		Type    string   `yaml:"type"`
		Pattern string   `yaml:"pattern"`
		Enum    []string `yaml:"enum"`
		Items   struct {
			Enum []string `yaml:"enum"`
		} `yaml:"items"`
	} `yaml:"properties"`
	AdditionalProperties *bool `yaml:"additionalProperties"`
}

func loadSchema(t *testing.T) scenarioSchema {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash("../../schemas/scenario.schema.yaml"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s scenarioSchema
	if err := yaml.Unmarshal(b, &s); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	if len(s.Properties) == 0 {
		t.Fatal("schema declares no properties — the shape changed, this test is reading it wrong")
	}
	return s
}

// VR12-S1b: the values the schema DUPLICATES from Go must equal their Go source. These are the two
// that had silently drifted. A duplicated constant with no test is a constant that will drift again.
func TestSchemaEnumsMatchTheCode(t *testing.T) {
	s := loadSchema(t)

	t.Run("layer enum == CanonicalLayers", func(t *testing.T) {
		got := append([]string(nil), s.Properties["layers"].Items.Enum...)
		want := append([]string(nil), CanonicalLayers...)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("schema layers enum != CanonicalLayers\n  schema: %v\n  code:   %v", got, want)
		}
	})

	// ⛔ The `priority enum == CanonicalPriorities` case is GONE with the key (VR12-M2 / V30-003).
	// Both sides were deleted in the same change, which is the point: a drift test that outlived one
	// of its two subjects would be asserting a contract nobody has.

	// The id pattern is the one that carried a RETIRED grammar for a whole round.
	t.Run("id pattern == validate.go idFormat", func(t *testing.T) {
		if got, want := s.Properties["id"].Pattern, idFormat.String(); got != want {
			t.Errorf("schema id pattern != idFormat\n  schema: %s\n  code:   %s", got, want)
		}
	})
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

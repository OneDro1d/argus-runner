package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-E8 — THE Go↔JMX CONTRACT, GUARDED.
//
// DeriveProps computes properties in Go; a JMX template consumes them in Groovy. Nothing has ever
// checked that the two agree, and the cost of that gap is not hypothetical:
//
//   - V30-004 (round 13): DeriveProps computes the ParseDBExpect properties for all THREE content
//     layers, and only database-state.jmx was ever taught to read them. MSGF-003's three field
//     assertions have been computed into properties nothing consumes, and it passes.
//   - This very change would have repeated it: switching DeriveProps to the numbered
//     expect.body.N.* set without teaching http-ingestion.jmx to read it would have silently
//     stopped body enforcement on the JMeter path entirely.
//
// So the contract is asserted on BOTH sides: every body property this package emits must appear in
// the template that is supposed to read it, and vice versa. A test that reads only one side would
// pass while the halves drifted apart.
//
// ⚠ It is a STRING match against the template, which cannot prove the Groovy is CORRECT — only that
// the property names still line up. Correctness of the assertion logic is proven by a live run
// (the QA stage), and this guards the failure mode that has actually happened twice.
func TestTemplateReadsTheBodyPropertiesDeriveEmits(t *testing.T) {
	jmx := readTemplate(t, "http-ingestion.jmx")

	// ── side 1: what DeriveProps actually emits for a scenario with two body assertions ────────
	md := strings.Join([]string{
		"# Scenario: t", "", "## Metadata",
		"- **ID**: T-001", "- **Layer**: HTTP Ingestion", "- **Tags**: http, t", "",
		"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
		"## EXPECT", "### Runnable",
		"- status=202",
		"- body has order_id containing 01",
		"- body contains accepted",
		"", "## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	c := &config.Config{}
	c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
	props, err := DeriveProps(c, scenario.Parse(md), "tr-t")
	if err != nil {
		t.Fatalf("DeriveProps: %v", err)
	}

	if got := props["expect.body.count"]; got != "2" {
		t.Fatalf("expect.body.count = %q, want 2 — both bullets must be wired (VR12-E8 R1)", got)
	}
	// The field must survive: it is the entire difference between the two forms.
	if props["expect.body.1.field"] != "order_id" {
		t.Errorf("assertion 1 lost its field: %q", props["expect.body.1.field"])
	}
	if props["expect.body.2.field"] != "" {
		t.Errorf("`body contains` is a WHOLE-RESPONSE assertion and must carry no field: %q", props["expect.body.2.field"])
	}

	// ── side 2: the template must actually read every one of them ─────────────────────────────
	for _, key := range []string{
		`expect.body.count`,
		`expect.body." + i + ".field`,
		`expect.body." + i + ".op`,
		`expect.body." + i + ".value`,
	} {
		if !strings.Contains(jmx, key) {
			t.Errorf("http-ingestion.jmx does not read %q — DeriveProps would emit a property nothing consumes, "+
				"which is exactly the V30-004 defect", key)
		}
	}

	// ── the transitional fallback must stay until the estate is on 0.3.31 ─────────────────────
	// A pre-0.3.31 executor reads only the old scalars; dropping them would take a stale image from
	// "enforces the first assertion" to "enforces nothing", which is worse than the defect.
	for _, legacy := range []string{"expect.body_contains", "expect.body_matches"} {
		if !strings.Contains(jmx, legacy) {
			t.Errorf("the legacy fallback %q was removed from the template — a mixed estate then enforces NOTHING", legacy)
		}
	}
	// The scalar is emitted only when an assertion of that KIND exists — this fixture declares two
	// `containing` and no `matching`, so only the first scalar is expected here.
	if props["expect.body_contains"] != "01" {
		t.Errorf("the legacy contains-fallback must carry the FIRST contains assertion, got %q", props["expect.body_contains"])
	}
	if _, present := props["expect.body_matches"]; present {
		t.Errorf("no `matching` bullet was declared, so the matches-fallback must not be set: %q", props["expect.body_matches"])
	}
	// …and a scenario that DOES declare one gets it.
	mdRe := strings.Replace(md, "- body contains accepted", "- body has order_id matching ^01[A-Z0-9]+$", 1)
	reProps, err := DeriveProps(c, scenario.Parse(mdRe), "tr-t")
	if err != nil {
		t.Fatalf("DeriveProps (matching): %v", err)
	}
	if reProps["expect.body_matches"] != "^01[A-Z0-9]+$" {
		t.Errorf("the legacy matches-fallback must carry the FIRST matching assertion, got %q", reProps["expect.body_matches"])
	}

	// ── VR-C8: no failure message in the template may echo an asserted value ───────────────────
	for _, leaky := range []string{`+ v)`, `+ v +`, `" + value`} {
		if strings.Contains(jmx, leaky) {
			t.Errorf("a template failure message interpolates the asserted value (%q) — observed is "+
				"product-hat-visible and must never echo the holdout (VR-C8)", leaky)
		}
	}
}

func readTemplate(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash("../../templates/" + name))
	if err != nil {
		t.Fatalf("read template %s: %v", name, err)
	}
	return string(b)
}

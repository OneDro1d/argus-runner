package argus

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/config"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// V30-004 T-A (VR13-MF) — THE TEMPLATE IS NOT CHOSEN BY LAYER ALONE.
//
// Everything this row adds keys on WHICH TEMPLATE WILL RUN: the write-time refusal (F-1), tier 3's
// report (F-5) and the gate test all have to ask the same question the runner asks. The runner's
// answer is three steps, not one (argus.go:590-597): the layer's dedicated template, THEN
// `http-idempotency` when a second status is declared on HTTP Ingestion, THEN `saga-presence` when
// the scenario carries the tag — and the last match wins.
//
// ⛔ A refusal computed from the LAYER alone would refuse a legal saga-presence scenario (its layer
// says database-state, but saga-presence.jmx is what runs) and miss an illegal idempotency one. So
// there is ONE exported helper and every caller uses it.
func TestRuntimeTemplateBase_MatchesWhatTheRunnerChooses(t *testing.T) {
	md := func(layer, tags, expect string) *scenario.Scenario {
		return scenario.Parse(strings.Join([]string{
			"# Scenario: t", "",
			"## Metadata", "- **ID**: T-001", "- **Layer**: " + layer, "- **Tags**: " + tags, "",
			"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
			"## EXPECT", "### Runnable", expect, "",
			"### Non-runnable", "- a fixture", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n"))
	}

	for _, c := range []struct {
		name, layer, tags, expect, want string
	}{
		{"the layer's own template", "Message Flow", "http", "- row_count == 1", "message-flow"},
		{"a layer with none falls back", "Permissions", "http", "- status=401", "http-ingestion"},
		{"database state", "Database State", "http", "- row_count == 1", "database-state"},
		{"external delivery", "External Delivery", "http", "- row_count == 1", "external-delivery"},

		// ⭐ the two overrides, in the runner's own order
		{"a second status on HTTP Ingestion", "HTTP Ingestion", "http", "- status=202\n- status2=409", "http-idempotency"},
		{"…but NOT on another layer", "Database State", "http", "- status=202\n- status2=409", "database-state"},
		{"the saga tag wins over the layer", "Database State", "http, saga-presence", "- row_count == 1", "saga-presence"},
		{"…and over idempotency, because it is checked last",
			"HTTP Ingestion", "http, saga-presence", "- status=202\n- status2=409", "saga-presence"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RuntimeTemplateBase(md(c.layer, c.tags, c.expect)); got != c.want {
				t.Errorf("RuntimeTemplateBase = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("a nil scenario answers the fallback, never panics", func(t *testing.T) {
		if got := RuntimeTemplateBase(nil); got != "http-ingestion" {
			t.Errorf("RuntimeTemplateBase(nil) = %q, want the fallback", got)
		}
	})
}

// V30-004 T-H — every key of TemplateReads is a real template basename, and every template on disk
// is in it. A map that drifts from `templates/` is how a property comes to be computed for a
// template that does not read it, which is this row's entire subject.
func TestTemplateReads_CoversExactlyTheShippedTemplates(t *testing.T) {
	onDisk := templateBasenames(t)
	for _, base := range onDisk {
		if _, ok := TemplateReads[base]; !ok {
			t.Errorf("templates/%s.jmx is on disk and not in TemplateReads — nothing knows what it reads", base)
		}
	}
	for base := range TemplateReads {
		found := false
		for _, d := range onDisk {
			if d == base {
				found = true
			}
		}
		if !found {
			t.Errorf("TemplateReads names %q, which is not a template on disk", base)
		}
	}
}

// templateBasenames lists the templates actually on disk, so the table above is checked against the
// tree rather than against itself.
func templateBasenames(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "templates", "*.jmx"))
	if err != nil {
		t.Fatalf("glob templates: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no templates found — the walk is not seeing the tree")
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.TrimSuffix(filepath.Base(p), ".jmx"))
	}
	return out
}

// ⛔ V30-004 — TemplateReads MUST DESCRIBE THE TEMPLATE, NOT THE INTENTION.
//
// The map is the product's own answer to "is this property read?", and every refusal and report in
// this row keys on it. If it claims a property a template does not actually reference, the product
// stops reporting a bullet as unexecuted while the bullet goes on being unexecuted — the same defect
// this row exists to remove, now hidden behind the fix.
//
// So each entry is checked against the file: the template must call `props.get("<prop>")` for every
// property claimed.
func TestTemplateReads_IsGroundedInTheTemplateFiles(t *testing.T) {
	for base, props := range TemplateReads {
		if len(props) == 0 {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "templates", base+".jmx"))
		if err != nil {
			t.Fatalf("read %s.jmx: %v", base, err)
		}
		for _, p := range props {
			// ⛔ `props.get("<name>"` — NOT `__P(<name>`. The expect.* family is read inside the Groovy
			// post-processors; `__P(…)` interpolates the REQUEST properties. The map's previous comment
			// claimed it had been measured with `__P(expect.`, which matches nothing in any template.
			if !strings.Contains(string(body), `props.get("`+p) {
				t.Errorf("TemplateReads says %s.jmx reads %q, and the file never calls props.get(%q)", base, p, p)
			}
		}
	}
}

// T-C — no property is referenced as a VARIABLE anywhere in the shipped templates.
func TestTemplates_ReferencePropertiesAsProperties(t *testing.T) {
	for _, base := range templateBasenames(t) {
		body, err := os.ReadFile(filepath.Join("..", "..", "templates", base+".jmx"))
		if err != nil {
			t.Fatalf("read %s.jmx: %v", base, err)
		}
		for _, bad := range []string{"${expect.", "${mq.", "${trigger.", "${correlation.id}"} {
			if strings.Contains(string(body), bad) {
				t.Errorf("%s.jmx references %q — that is a VARIABLE and resolves to nothing; properties are `${__P(name,default)}`", base, bad)
			}
		}
	}
}

// ⭐ V30-004's DEFINITION OF DONE — EVERY DERIVED CONTENT PROPERTY IS READABLE ON ITS LAYER.
//
// This is the row's actual statement, and the one that was RED before the fix: for a scenario on a
// content layer declaring column and row-count assertions, the CONTENT properties DeriveProps
// computes minus the ones its runtime template reads must be EMPTY. Red for Message Flow and
// External Delivery before F-2/F-3, green after.
//
// ⛔ CONTENT KEYS ONLY. Not "every key DeriveProps computes": that set includes trigger.*,
// expect.status, mq.* and more — none of which a template reads, because the runner-core judges
// them in Go — so the difference could never be empty and the gate could never go green.
func TestEveryDerivedPropertyIsReadableOnItsLayer(t *testing.T) {
	isContentKey := func(k string) bool {
		return strings.HasPrefix(k, "expect.body.") || k == "expect.body_contains" || k == "expect.body_matches" ||
			k == "expect.columns" || k == "expect.row_count" || k == "expect.has_rows" || k == "expect.refusal_code"
	}
	for _, layer := range []string{"Database State", "Message Flow", "External Delivery"} {
		t.Run(layer, func(t *testing.T) {
			md := strings.Join([]string{
				"# Scenario: t", "",
				"## Metadata", "- **ID**: T-001", "- **Layer**: " + layer, "- **Tags**: http", "",
				"## TRIGGER", "POST \x60${INGESTION_URL}/api/v1/orders\x60", "",
				"## EXPECT", "### Runnable",
				"- row_count == 1",
				"- customer_id == c1", "",
				"### Non-runnable", "- a fixture", "",
				"## TIMEOUT", "30s", "",
				"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
			}, "\n")
			s := scenario.Parse(md)
			c := &config.Config{}
			c.Targets.HTTP = &config.HTTPTarget{BaseURL: "http://sut.invalid"}
			c.Targets.Database = &config.DBTarget{JDBCURL: "jdbc:postgresql://db.invalid:5432/t", Username: "u"}
			p, err := DeriveProps(c, s, "tr-1")
			if err != nil {
				t.Fatalf("DeriveProps: %v", err)
			}
			base := RuntimeTemplateBase(s)
			var unread []string
			for k := range p {
				if isContentKey(k) && !templateReadsProperty(base, k) {
					unread = append(unread, k)
				}
			}
			if len(unread) != 0 {
				t.Errorf("%s runs %s.jmx, which reads none of %v — they are computed and never consumed",
					layer, base, unread)
			}
		})
	}
}

// ⛔ V30-004 (Obs gate) — THE TAP'S CALLS ARE NOT SUT REQUESTS.
//
// classifyRequestSamples counts one SUT request per .jtl sample and excludes a sample ONLY when
// isVerifySample matches, which is anchored to the exact `<scenario.id>-verify` prefix. F-3 and F-4
// add six management calls to message-flow.jmx; labelled anything else, each one would show up as
// load the SUT never received — the Test-requests panel and the per-request distribution would both
// move, and an operator would read traffic that does not exist.
func TestMessageFlowTemplate_EveryManagementCallIsAVerifySample(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "templates", "message-flow.jmx"))
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`testname="\$\{__P\(scenario\.id,unknown\)\}-([a-z-]+)"`).FindAllStringSubmatch(string(body), -1)
	if len(names) < 7 {
		t.Fatalf("expected the trigger plus six management samplers, found %d", len(names))
	}
	for _, m := range names {
		suffix := m[1]
		if suffix == "trigger" {
			continue // the ONE sampler that is a real SUT request
		}
		if !strings.HasPrefix(suffix, "verify") {
			t.Errorf("sampler %q is not a `-verify` sample, so it will be counted as a SUT request", m[0])
		}
		// and the exclusion is by PREFIX on `<id>-verify`, so the label must begin there
		if !isVerifySample("MF-001-"+suffix, "MF-001") {
			t.Errorf("isVerifySample does not exclude %q — the request count would include it", suffix)
		}
	}
}

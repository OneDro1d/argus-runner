package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// V30-004 F-1 (VR13-MF) — A PROPERTY NO TEMPLATE CAN READ IS REFUSED WHEN THE SCENARIO IS WRITTEN.
//
// The class this row is really about: `DeriveProps` computes a property set per LAYER, the template
// is chosen per layer too, and the two lists were never tied together. So an author could write a
// column assertion on a Message Flow scenario, have it computed into `expect.columns`, and have no
// template ever read it — the scenario passed on the correlation-id "a message exists" proxy alone.
//
// The refusal lives in ExpectProblems, which the author path already calls through
// toolcore.ValidateAll — so it reaches author__validate_scenario, author__write_scenario and the
// control plane's write with no second rule set (V31-002's ruling: refusal belongs at write time).
func TestExpectProblems_RefusesAPropertyNoTemplateCanRead(t *testing.T) {
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
	has := func(ps []ExpectProblem, needle string) bool {
		for _, p := range ps {
			if strings.Contains(p.Message, needle) {
				return true
			}
		}
		return false
	}

	t.Run("a body assertion on a saga-presence scenario is refused", func(t *testing.T) {
		// saga-presence.jmx reads no expect.* property: its verdict is the Loki assertion.
		ps := ExpectProblems(md("HTTP Ingestion", "http, saga-presence",
			"- status=202\n- body has order_id containing 01"))
		if !has(ps, "does not read") {
			t.Fatalf("a body assertion on saga-presence must be refused: %+v", ps)
		}
		if !has(ps, "saga-presence.jmx") {
			t.Errorf("the message must name the template that will actually run: %+v", ps)
		}
		if !has(ps, "### Non-runnable") {
			t.Errorf("the message must offer the author somewhere to put it: %+v", ps)
		}
	})

	t.Run("⭐ and on an HTTP Ingestion scenario that declares status2", func(t *testing.T) {
		// The SA gate's case: `status2=` switches the runtime template to http-idempotency.jmx,
		// which reads NO content property. Keyed on the layer alone this would be missed.
		ps := ExpectProblems(md("HTTP Ingestion", "http",
			"- status=202\n- status2=409\n- body has order_id containing 01"))
		if !has(ps, "http-idempotency.jmx") {
			t.Fatalf("a content check beside status2= must be refused, naming the template: %+v", ps)
		}
	})

	t.Run("a column assertion on a layer whose template reads it is NOT refused", func(t *testing.T) {
		ps := ExpectProblems(md("Database State", "http", "- row_count == 1\n- customer_id == c1"))
		if has(ps, "does not read") {
			t.Fatalf("database-state.jmx reads the column family — nothing to refuse: %+v", ps)
		}
	})

	t.Run("⛔ a plain `- status=` bullet is NEVER refused, on any layer", func(t *testing.T) {
		// expect.status is emitted for every scenario and read by NO template: the verdict is made
		// in Go. A rule keyed on "emitted but unread" would refuse `- status=202` everywhere, which
		// is why F-1 walks only the two content classes.
		for _, layer := range []string{"HTTP Ingestion", "Database State", "Message Flow", "External Delivery", "Permissions"} {
			if ps := ExpectProblems(md(layer, "http", "- status=202")); has(ps, "does not read") {
				t.Errorf("%s: a plain status bullet was refused: %+v", layer, ps)
			}
		}
	})

	t.Run("⛔ a `ui` scenario is out of F-1's scope entirely", func(t *testing.T) {
		// A ui scenario runs Playwright, not a .jmx, so "runs ui.jmx, which does not read …" would
		// be false. V29-021 owns that layer's grammar.
		ps := ExpectProblems(md("Web UI", "ui", "- row_count == 1"))
		if has(ps, "does not read") {
			t.Errorf("a ui scenario must not be judged against a .jmx: %+v", ps)
		}
	})

	t.Run("⛔ and so are mcp and chain", func(t *testing.T) {
		for _, tag := range []string{"mcp", "chain"} {
			if ps := ExpectProblems(md("HTTP Ingestion", tag, "- result.isError == false")); has(ps, "does not read") {
				t.Errorf("%s: a native-engine scenario must not be judged against a .jmx: %+v", tag, ps)
			}
		}
	})
}

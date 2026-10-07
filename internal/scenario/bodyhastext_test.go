package scenario

import (
	"strings"
	"testing"
)

// V31-004 fix 3 (VR13-BF) · TS-BF-1 — `body has text containing …` WARNS, and the write succeeds.
//
// Since V29-017 the word after `body has` is THE NAME OF A JSON FIELD (`bodyassert.go`), so
// `body has text containing x` asks for a top-level field called `text`. Almost no answer in this
// estate has one: an MCP tool answer carries its text at `content[0].text`, and an HTTP JSON body
// carries whatever the SUT named it. The form therefore fails on every such answer — and it is the
// form the product's own guidance, hints and shipped examples taught, in 15 places.
//
// The owner's model decides the severity: the skill GUIDES, validation REFUSES, the run REPORTS —
// and a bullet that IS understood but is probably not what the author meant is the WARNING case
// (E5's rule), never a refusal. An author whose SUT really does answer `{"text": …}` must still be
// able to save it.
//
// ⚠ The warning covers HTTP scenarios too, not only MCP and chain (the owner's ruling **g**).
func TestWarnings_BodyHasTextIsWarnedAndStillValid(t *testing.T) {
	md := func(tag, layer, plane, bullet string) string {
		return strings.Join([]string{
			"# Scenario: BF-001", "",
			"## Metadata",
			"- **ID**: BF-001",
			"- **Layer**: " + layer,
			"- **Tags**: " + tag, "",
			"## TRIGGER",
			"POST `${INGESTION_URL}/api/v1/orders`", "",
			"## EXPECT",
			"### Runnable",
			"- " + plane,
			"- " + bullet, "",
			"### Non-runnable",
			"- this fixture exists to exercise the warning", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
	}

	// ⚠ NOT the needle "body has text": E5's own warning QUOTES the offending bullet, so that
	// substring appears in a warning this rule does not own. Assert on the substance instead.
	const needle = "asks for a JSON field called `text`"

	count := func(ws []string) int {
		n := 0
		for _, w := range ws {
			if strings.Contains(w, needle) {
				n++
			}
		}
		return n
	}

	t.Run("it warns, on every layer that carries a body", func(t *testing.T) {
		for _, c := range []struct{ tag, layer, plane string }{
			{"http", "HTTP Ingestion", "status=200"},
			{"mcp", "MCP Tool", "result.isError == false"},
		} {
			layer := c.layer
			text := md(c.tag, c.layer, c.plane, "body has text containing argus-race")
			w := Warnings(text)
			if n := count(w); n != 1 {
				t.Errorf("%s: warned %d time(s) about %q, want exactly 1 — got %v", layer, n, needle, w)
			}
			// the warning must TEACH the form that works, not merely complain
			joined := strings.Join(w, " | ")
			if !strings.Contains(joined, "body contains <value>") {
				t.Errorf("%s: the warning does not offer `body contains <value>`: %v", layer, w)
			}
		}
	})

	t.Run("the write still succeeds — it is advisory, never a refusal", func(t *testing.T) {
		text := md("http", "HTTP Ingestion", "status=200", "body has text containing argus-race")
		if _, errs := Validate(text); len(errs) != 0 {
			t.Fatalf("`body has text` must NOT be an error — the author may have a SUT that really answers {\"text\":…}; got %v", errs)
		}
	})

	t.Run("a `### Non-runnable` line is not warned about", func(t *testing.T) {
		// It is documentation. E5 already tells the author it looks like an assertion; a second
		// warning about the FORM of a line nobody runs is noise.
		text := strings.Join([]string{
			"# Scenario: BF-002", "",
			"## Metadata", "- **ID**: BF-002", "- **Layer**: HTTP Ingestion", "- **Tags**: http", "",
			"## TRIGGER", "POST `${INGESTION_URL}/api/v1/orders`", "",
			"## EXPECT",
			"### Runnable", "- status=200", "",
			"### Non-runnable", "- body has text containing argus-race", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		if n := count(Warnings(text)); n != 0 {
			t.Errorf("a `### Non-runnable` line warned %d time(s) about %q — it is documentation", n, needle)
		}
	})

	t.Run("a real field name is NOT warned about", func(t *testing.T) {
		// The rule is about the word `text` specifically, because that is the word the guidance
		// taught. `body has error containing …` and `body has content[0].text containing …` are
		// both correct uses of the grammar and must stay silent.
		for _, b := range []string{
			"body has error containing missing_token",
			"body has content[0].text containing argus-race",
			"body has status matching ^healthy$",
			"body contains argus-race",
		} {
			if n := count(Warnings(md("mcp", "MCP Tool", "result.isError == false", b))); n != 0 {
				t.Errorf("%q warned about %q — it is a correct use of the grammar", b, needle)
			}
		}
	})
}

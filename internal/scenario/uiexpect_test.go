package scenario

import (
	"strings"
	"testing"
)

// V29-021 G-1 (VR13-UI) — A `ui` SCENARIO CAN FINALLY STATE A RUNNABLE EXPECTATION.
//
// Until 0.3.32 a Web UI scenario's `## EXPECT` was read by NOTHING: its real assertions were locked
// inside the Playwright spec file and the verdict was a bare exit code. An author could write three
// careful bullets and the product would compare none of them.
//
// The grammar is deliberately small, and it invents no selector language: the selector is a
// Playwright LOCATOR string, which is what the spec already speaks (the owner's U-3).
func TestParseUIExpect_TheThreeForms(t *testing.T) {
	t.Run("dom has <selector>", func(t *testing.T) {
		got, errs := ParseUIExpect([]string{"- dom has .order-banner"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(got) != 1 || got[0].Kind != UIKindDOM || got[0].Selector != ".order-banner" || got[0].Op != "" {
			t.Fatalf("got %+v", got)
		}
		if got[0].Bullet != "dom has .order-banner" {
			t.Errorf("the bullet must travel with the assert — the outcomes file keys on it: %q", got[0].Bullet)
		}
	})

	t.Run("dom has <selector> containing <text>", func(t *testing.T) {
		got, errs := ParseUIExpect([]string{"- dom has .total containing 42.00"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got[0].Op != UIOpContains || got[0].Value != "42.00" || got[0].Selector != ".total" {
			t.Fatalf("got %+v", got[0])
		}
	})

	t.Run("dom has <selector> matching <regex>", func(t *testing.T) {
		got, errs := ParseUIExpect([]string{"- dom has .total matching ^\\d+\\.\\d{2}$"})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if got[0].Op != UIOpMatches || got[0].Value != `^\d+\.\d{2}$` {
			t.Fatalf("got %+v", got[0])
		}
	})

	t.Run("a QUOTED selector, because a locator contains spaces", func(t *testing.T) {
		// `text=Order created` has a space in it, so the selector group has to be quote-aware or the
		// value half would swallow the rest of the locator.
		for _, b := range []string{
			`- dom has "text=Order created" containing created`,
			"- dom has `text=Order created` containing created",
			`- dom has 'text=Order created' containing created`,
		} {
			got, errs := ParseUIExpect([]string{b})
			if len(errs) != 0 {
				t.Fatalf("%s: unexpected errors: %v", b, errs)
			}
			if got[0].Selector != "text=Order created" {
				t.Errorf("%s: selector = %q, want the unquoted locator", b, got[0].Selector)
			}
			if got[0].Value != "created" {
				t.Errorf("%s: value = %q", b, got[0].Value)
			}
		}
	})

	t.Run("no backend 4xx/5xx", func(t *testing.T) {
		got, errs := ParseUIExpect([]string{"- no backend 4xx/5xx"})
		if len(errs) != 0 || len(got) != 1 || got[0].Kind != UIKindNoBackendErrors {
			t.Fatalf("got %+v errs=%v", got, errs)
		}
	})

	t.Run("no console errors", func(t *testing.T) {
		for _, b := range []string{"- no console errors", "- no console error"} {
			got, errs := ParseUIExpect([]string{b})
			if len(errs) != 0 || len(got) != 1 || got[0].Kind != UIKindNoConsoleErrors {
				t.Fatalf("%s: got %+v errs=%v", b, got, errs)
			}
		}
	})

	// ⛔ A CORRECT OPENER WITH A MALFORMED TAIL IS AN ERROR, NEVER PROSE. That is what the three
	// openers buy — and it is ALL they buy.
	//
	// ⚠ The justification this rule is often given is wrong, and it is written down here so it is
	// not repeated: a bullet whose OPENER is misspelled (`- no backedn 4xx/5xx`) parses as prose
	// under any opener list, so this regex cannot be what protects against it. The guarantee there
	// comes from V31-002 — a scenario declaring no runnable check is refused when written and
	// reported `error` when it runs.
	t.Run("a claim-shaped bullet that does not parse is an ERROR", func(t *testing.T) {
		for _, b := range []string{"- dom has", "- no backend", "- no console"} {
			got, errs := ParseUIExpect([]string{b})
			if len(errs) == 0 {
				t.Errorf("%q was accepted as %+v — a ui claim that does not parse must be an error", b, got)
				continue
			}
			if !strings.Contains(errs[0].Error(), "is not a `ui` assertion") {
				t.Errorf("%q: %v — the message must say what the forms are", b, errs[0])
			}
		}
	})

	t.Run("ordinary prose is prose", func(t *testing.T) {
		for _, b := range []string{
			"- the dashboard renders",
			"- the operator sees the new order",
			"- no backedn 4xx/5xx", // a misspelled OPENER is prose, and V31-002 is what catches it
		} {
			got, errs := ParseUIExpect([]string{b})
			if len(errs) != 0 {
				t.Errorf("%q was refused: %v", b, errs)
			}
			if len(got) != 0 {
				t.Errorf("%q was read as an assertion: %+v", b, got)
			}
		}
	})

	t.Run("several bullets, in order", func(t *testing.T) {
		got, errs := ParseUIExpect([]string{
			"- dom has .banner",
			"- the operator sees it", // prose, skipped
			"- no console errors",
		})
		if len(errs) != 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if len(got) != 2 || got[0].Kind != UIKindDOM || got[1].Kind != UIKindNoConsoleErrors {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("an uncompilable regex is an authoring error", func(t *testing.T) {
		_, errs := ParseUIExpect([]string{"- dom has .total matching ("})
		if len(errs) == 0 {
			t.Fatal("a regex that does not compile must be refused before any browser is started")
		}
	})
}

// V29-021 G-4 (U-F) — THE VALIDATOR IS STRICTER THAN THE RUN PATH, AND DELIBERATELY SO.
//
// Two rules, and which applies where matters:
//
//	· the RUN path uses ParseUIExpect's claim-shaped rule — a prose bullet is prose, so a catalogue
//	  written before this grammar keeps running;
//	· the AUTHOR path refuses ANY `### Runnable` bullet on a `ui` scenario that yields no UIAssert.
//	  A bullet under `### Runnable` is a CLAIM by position (VR12-E1), so a claim the product cannot
//	  execute is exactly what must not be saved — and the author is standing right there.
func TestValidate_UIRunnableBulletMustParse(t *testing.T) {
	md := func(runnable string) string {
		return strings.Join([]string{
			"# Scenario: u", "",
			"## Metadata", "- **ID**: UI-010", "- **Layer**: Web UI", "- **Tags**: ui", "",
			"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
			"## EXPECT", "### Runnable", runnable, "",
			"### Non-runnable", "- the flow is described in the spec file", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
	}
	const needle = "is not a `ui` assertion"

	t.Run("every form is accepted", func(t *testing.T) {
		for _, b := range []string{
			"- dom has .banner",
			"- dom has .total containing 42.00",
			"- no backend 4xx/5xx",
			"- no console errors",
		} {
			if _, errs := Validate(md(b)); hasMessage(errs, needle) {
				t.Errorf("%q was refused: %v", b, errs)
			}
		}
	})

	t.Run("⛔ a PROSE bullet under `### Runnable` is refused on a ui scenario", func(t *testing.T) {
		// It is prose to the RUN path and a claim to the AUTHOR path — because position is what
		// makes it a claim, and a claim nothing can execute must not be saved.
		if _, errs := Validate(md("- the dashboard renders")); !hasMessage(errs, needle) {
			t.Errorf("a runnable bullet the ui grammar cannot execute must be refused: %v", errs)
		}
	})

	t.Run("…and the same sentence under `### Non-runnable` is fine", func(t *testing.T) {
		text := strings.Join([]string{
			"# Scenario: u", "",
			"## Metadata", "- **ID**: UI-011", "- **Layer**: Web UI", "- **Tags**: ui", "",
			"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
			"## EXPECT", "### Runnable", "- dom has .banner", "",
			"### Non-runnable", "- the dashboard renders", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		if _, errs := Validate(text); hasMessage(errs, needle) {
			t.Errorf("documentation was refused: %v", errs)
		}
	})

	t.Run("⛔ and a NON-ui scenario is untouched by this rule", func(t *testing.T) {
		text := strings.Join([]string{
			"# Scenario: h", "",
			"## Metadata", "- **ID**: H-010", "- **Layer**: HTTP Ingestion", "- **Tags**: http", "",
			"## TRIGGER", "POST \x60${INGESTION_URL}/x\x60", "",
			"## EXPECT", "### Runnable", "- status=202", "",
			"### Non-runnable", "- a fixture", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		if _, errs := Validate(text); hasMessage(errs, needle) {
			t.Errorf("an http scenario was judged by the ui grammar: %v", errs)
		}
	})
}

// V29-021 (U-H) — THE "EXCEEDS THE WEB UI LAYER" WARNING NARROWS.
//
// UC-83's warning fires on a `ui` bullet that reaches past what the layer can verify — a saga, a DB
// row, an HTTP status. It was right while the layer could verify only "the page rendered". Now that
// the grammar EXECUTES `dom has …`, a bullet whose TEXT happens to contain one of those words is
// executed, and warning about it tells the author to move something the product just ran.
func TestWarnings_UIExceedNarrows(t *testing.T) {
	md := func(runnable string) string {
		return strings.Join([]string{
			"# Scenario: u", "",
			"## Metadata", "- **ID**: UI-012", "- **Layer**: Web UI", "- **Tags**: ui", "",
			"## TRIGGER", "POST \x60tests/live/x.spec.ts\x60", "",
			"## EXPECT", "### Runnable", runnable, "",
			"### Non-runnable", "- the flow is described in the spec file", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
	}
	const needle = "exceeds the Web UI layer"
	count := func(ws []string) int {
		n := 0
		for _, w := range ws {
			if strings.Contains(w, needle) {
				n++
			}
		}
		return n
	}

	t.Run("⭐ a dom assertion whose VALUE mentions a saga is EXECUTED, so it must not warn", func(t *testing.T) {
		// uiExceedRe matches the word `saga` anywhere. The product now runs this bullet: it checks
		// the DOM for that text. Telling the author to move it would be telling them to remove a
		// check that works.
		if n := count(Warnings(md("- dom has .banner containing saga"))); n != 0 {
			t.Errorf("warned %d time(s) about a bullet the grammar executes", n)
		}
	})

	t.Run("…and a bullet that really does reach past the layer still warns", func(t *testing.T) {
		// This one is prose to the grammar — nothing runs it — and it asserts a DB row.
		if n := count(Warnings(md("- the objects table has 3 rows"))); n != 1 {
			t.Errorf("warned %d time(s), want exactly 1 — a DB-row claim on a ui scenario is not executed", n)
		}
	})
}

package reporoute

import (
	"fmt"
	"strings"
)

// getMethod is the only HTTP verb this tool ever calls "safe by default" (spec item 3): it needs
// no config to be proposed, and its EXPECT is the one thing route discovery can assert without
// guessing — that the route answers at all.
const getMethod = "GET"

// draftBody builds an HTTP Ingestion scenario draft (spec item 3) for g, a deduplicated routeGroup
// that may carry MULTIPLE source occurrences (spec item 3: "keep ONE draft whose References list
// EVERY source file:line that declared it"). The **ID** field carries the literal placeholder
// "__SCENARIO_ID__", filled in by withID once the deterministic id is known — draftBody itself has
// no id to assign yet (that depends on collisions across the whole route set, decided by the
// caller). proposed reports whether this is a non-GET draft proposed without a config, clearly
// tagged and flagged for human review (spec item 3, third bullet).
func draftBody(g routeGroup, moneyHandling bool) (body string, proposed bool) {
	r := Route{Method: g.method, Path: g.path, ParamNames: g.paramNames}
	tags := "http, proposed"
	nonRunnable := []string{}

	for _, p := range r.ParamNames {
		nonRunnable = append(nonRunnable, fmt.Sprintf(
			"path parameter `%s` needs a real value from the SUT before this scenario can run — "+
				"the TRIGGER below uses the literal placeholder `%s` from the route source", p, p))
	}

	isGet := r.Method == getMethod
	if !isGet {
		proposed = true
		nonRunnable = append(nonRunnable, fmt.Sprintf(
			"this scenario WRITES to the SUT (%s %s) and needs a human decision before it runs — "+
				"propose-from-repo proposes it because it validated, but does not judge whether it is safe to fire", r.Method, r.Path))
		// VR12-E6 (validate.go) requires a declared `status=<code>` for any scenario judged by
		// response code, and the grammar only accepts an EXACT 3-digit code (status.go) — there is
		// no "unknown" form. 200 is a PLACEHOLDER, not a discovered fact, and this note says so
		// rather than letting the Runnable bullet read as a confirmed answer.
		nonRunnable = append(nonRunnable, "the Runnable status=200 below is a PLACEHOLDER, not a value read "+
			"from the SUT — confirm the real status this write returns before trusting a run against it")
	}

	cleanup := "N/A — a GET creates nothing."
	if !isGet {
		cleanup = fmt.Sprintf(
			"N/A — REVIEW THIS: propose-from-repo cannot know what a %s to %s creates. Confirm this scenario "+
				"leaves nothing behind, or replace this line with a ```sql / ```bash block that removes what it creates.",
			r.Method, r.Path)
	}

	verify := "N/A — the HTTP Ingestion layer judges the response directly."

	var expect strings.Builder
	expect.WriteString("### Runnable\n- status=200\n")
	if len(nonRunnable) > 0 {
		expect.WriteString("\n### Non-runnable\n")
		for _, n := range nonRunnable {
			expect.WriteString("- " + n + "\n")
		}
	}

	var refs strings.Builder
	for _, s := range g.sources {
		refs.WriteString(fmt.Sprintf("- Discovered by propose-from-repo (extractor: %s) at %s\n", s.Extractor, source(s)))
	}

	title := r.Method + " " + r.Path
	body = "# Scenario: " + title + " (proposed from repo route)\n\n" +
		"## Metadata\n" +
		"- **ID**: __SCENARIO_ID__\n" +
		"- **Layer**: HTTP Ingestion\n" +
		"- **Tags**: " + tags + "\n\n" +
		"## TRIGGER\n" +
		r.Method + " `${INGESTION_URL}" + r.Path + "`\n\n" +
		"## VERIFY\n" + verify + "\n\n" +
		"## EXPECT\n" + expect.String() + "\n" +
		"## TIMEOUT\n30s\n\n" +
		"## CLEANUP\n" + cleanup + "\n\n" +
		"## References\n" + refs.String()

	_ = moneyHandling // moneyHandling is applied by the caller via MoneyGuardViolations; kept for signature symmetry
	return body, proposed
}

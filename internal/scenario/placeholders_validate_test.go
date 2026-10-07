package scenario

import (
	"strings"
	"testing"
)

// placeholders_validate_test.go — V31-003 rule 3-6, AT WRITE TIME.
//
// ⛔ THE RULE SAYS *"REFUSED AT VALIDATION"* AND ONLY THE RUNTIME HALF WAS COVERED. The round-13
// reconciliation found the register about to cite `TestChainRun_LegacyExpectWithAnUnfilledPlaceholderStopsAtPreflight`
// — a RUN-time test — for a rule whose whole point is that the author is told when they WRITE the
// file. The two are not interchangeable: a refusal that only arrives at run time means the author
// has already committed the scenario, and on a cloud write the file is already in the catalog.
//
// ⚠ WHY THE DISTINCTION IS THE ROW'S OWN POINT. A `${TOKEN}` in a check cannot be filled in, and if
// it could its VALUE would be printed in the report. Catching that at write time is the difference
// between a message and a leak.

func placeholderMD(tags, expect string) string {
	return strings.Join([]string{
		"# S-PH-1 — a check with a placeholder",
		"",
		"## Metadata",
		"- **ID**: S-PH-1",
		"- **Layer**: MCP Contract",
		"- **Tags**: " + tags,
		"",
		"## TRIGGER",
		"```json",
		`{"tool":"t","args":{}}`,
		"```",
		"",
		"## EXPECT",
		expect,
		"## TIMEOUT",
		"15s",
		"",
		"## CLEANUP",
		"N/A — reads only",
		"",
	}, "\n")
}

func TestValidate_APlaceholderNothingFillsIsRefusedWhenItIsWritten(t *testing.T) {
	for _, c := range []struct {
		name, tags, expect, want string
	}{
		{
			"an environment variable in a check",
			"mcp",
			"### Runnable\n- body contains ${TOKEN}\n",
			"${TOKEN}",
		},
		{
			// ⛔ an UNCLOSED one is caught too: `${cid` is not `${cid}`, and a reader skimming would
			// see the name they expected.
			"an unclosed placeholder",
			"mcp",
			"### Runnable\n- body contains ${cid\n",
			"${cid",
		},
		{
			// ⛔ `${saved.<var>}` is a CHAIN form. On a non-chain scenario nothing fills it.
			"a saved ref outside a chain",
			"mcp",
			"### Runnable\n- body contains ${saved.docId}\n",
			"${saved.docId}",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, errs := Validate(placeholderMD(c.tags, c.expect))
			var found bool
			for _, e := range errs {
				if strings.Contains(e.Message, c.want) && strings.Contains(e.Message, "never filled in") {
					found = true
				}
			}
			if !found {
				t.Fatalf("writing %q was ACCEPTED. The author is told at run time instead — by which "+
					"point the scenario is committed, and on a cloud write it is already in the "+
					"catalog. Errors: %v", c.want, errs)
			}
		})
	}
}

// ⛔ AND THE THREE THAT ARE FILLED IN ARE NOT REFUSED. A rule that refuses the legal forms is worse
// than no rule: it teaches authors to stop using the placeholders the product provides.
func TestValidate_TheFilledPlaceholdersAreAccepted(t *testing.T) {
	for _, c := range []struct{ name, tags, bullet string }{
		{"${cid}", "mcp", "- body contains ${cid}"},
		{"${correlation_id}", "mcp", "- body contains ${correlation_id}"},
		{"${saved.<var>} in a chain", "chain", "- step read: body has id containing ${saved.docId}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, errs := Validate(placeholderMD(c.tags, "### Runnable\n"+c.bullet+"\n"))
			for _, e := range errs {
				if strings.Contains(e.Message, "never filled in") {
					t.Fatalf("%s was refused, and it IS filled in: %v", c.name, e.Message)
				}
			}
		})
	}
}

// ⚠ A `### Non-runnable` LINE IS DOCUMENTATION AND IS NEVER POLICED. Nothing fills it in because
// nothing runs it, and refusing an author for writing "the token is read from ${TOKEN}" under the
// heading that exists for exactly that kind of sentence would be the rule biting its own purpose.
func TestValidate_APlaceholderUnderNonRunnableIsLeftAlone(t *testing.T) {
	_, errs := Validate(placeholderMD("mcp",
		"### Runnable\n- result.isError == false\n\n### Non-runnable\n- the token is read from ${TOKEN}\n"))
	for _, e := range errs {
		if strings.Contains(e.Message, "never filled in") {
			t.Fatalf("a documentation line was refused for containing a placeholder: %v", e.Message)
		}
	}
}

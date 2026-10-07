package calm

import (
	"strings"
	"testing"
)

// ── review item 3: a guardrail that refuses by TEXT ───────────────────────────────────────────────

func textOpts() Options {
	o := guardOpts()
	o.ControlArgs["mcp-guardrail.refusal"] = "text"
	o.ControlArgs["mcp-guardrail.refused-contains"] = "is restricted"
	o.ControlArgs["mcp-guardrail.allowed"] = "LSE:AAPL"
	return o
}

func TestImport_TextRefusalClaimsTheTextOnDeniedAndTheAllowedSymbolOnAllowedSteps(t *testing.T) {
	r := mustImport(t, guardFiles(arch(relAtoM, guardrailControl)), textOpts())
	for _, c := range checkByKind(r, KindGuardrailRefused) {
		body := fileOf(t, r, c)
		mustBeValid(t, c.File, body)
		contains(t, body, "- step denied: body contains is restricted", c.ID)
		contains(t, body, "- step allowed: result.isError == false", c.ID)
		contains(t, body, "- step allowed: body contains LSE:AAPL", c.ID)
		if strings.Contains(body, "- step denied: result.isError == true") {
			t.Errorf("%s: a text refusal must not claim isError == true:\n%s", c.ID, body)
		}
	}
	ab := fileOf(t, checkedResult(r), checkByKind(r, KindGuardrailAllowed)[0])
	mustBeValid(t, "allowed", ab)
	contains(t, ab, "- step allowed: result.isError == false", "allowed check")
	contains(t, ab, "- step allowed: body contains LSE:AAPL", "allowed check")
}

func checkedResult(r *Result) *Result { return r }

func TestImport_TextRefusalNeedsRefusedContains(t *testing.T) {
	o := textOpts()
	delete(o.ControlArgs, "mcp-guardrail.refused-contains")
	if _, err := run(t, guardFiles(arch(relAtoM, guardrailControl)), o); err == nil || !strings.Contains(err.Error(), "mcp-guardrail.refused-contains") {
		t.Fatalf("refusal=text without refused-contains must be refused by name; got %v", err)
	}
}

// ── review item 4: an argument template ───────────────────────────────────────────────────────────

func argsOpts() Options {
	o := textOpts()
	delete(o.ControlArgs, "mcp-guardrail.arg")
	o.ControlArgs["mcp-guardrail.args"] = `{"filter":"instrument eq '${symbol}'","nextLink":"","size":5}`
	return o
}

func TestImport_ArgsTemplateReplacesSymbol(t *testing.T) {
	r := mustImport(t, guardFiles(arch(relAtoM, guardrailControl)), argsOpts())
	found := false
	for _, c := range checkByKind(r, KindGuardrailRefused) {
		body := fileOf(t, r, c)
		mustBeValid(t, c.File, body)
		if strings.Contains(body, `"filter": "instrument eq 'VOD'"`) {
			found = true
			contains(t, body, `"filter": "instrument eq 'LSE:AAPL'"`, c.ID)
			contains(t, body, `"size": 5`, c.ID)
			contains(t, body, `"nextLink": ""`, c.ID)
		}
	}
	if !found {
		t.Fatalf("no refused check carries the VOD filter")
	}
}

func TestImport_ArgsTemplateEscapesTheSymbol(t *testing.T) {
	o := argsOpts()
	o.ControlArgs["mcp-guardrail.allowed"] = `A"B`
	r := mustImport(t, guardFiles(arch(relAtoM, guardrailControl)), o)
	body := fileOf(t, r, checkByKind(r, KindGuardrailAllowed)[0])
	contains(t, body, `instrument eq 'A\"B'`, "escaped symbol")
}

func TestImport_ArgsAndArgTogetherAreRefusedByName(t *testing.T) {
	o := argsOpts()
	o.ControlArgs["mcp-guardrail.arg"] = "symbol"
	_, err := run(t, guardFiles(arch(relAtoM, guardrailControl)), o)
	if err == nil || !strings.Contains(err.Error(), "mcp-guardrail.arg") || !strings.Contains(err.Error(), "mcp-guardrail.args") {
		t.Fatalf("both .arg and .args must be refused by name; got %v", err)
	}
}

func TestImport_ArgsTemplateMustBeAnObjectWithTheSymbol(t *testing.T) {
	for name, tmpl := range map[string]string{
		"not json": `nope`, "array": `["${symbol}"]`, "no symbol": `{"a":"b"}`,
	} {
		o := argsOpts()
		o.ControlArgs["mcp-guardrail.args"] = tmpl
		if _, err := run(t, guardFiles(arch(relAtoM, guardrailControl)), o); err == nil || !strings.Contains(err.Error(), "mcp-guardrail.args") {
			t.Errorf("%s: want a refusal naming mcp-guardrail.args; got %v", name, err)
		}
	}
}

package scenario

import (
	"strings"
	"testing"
)

// VR12-E10 (V30-002) — A CHAIN'S CLAIMS HAVE EXACTLY ONE HOME.
//
// Before this round a chain carried its assertions inside the TRIGGER JSON while its `## EXPECT`
// was documentation by design. Two places looked like they held the claims and only one did, so
// authors filled the decorative one: measured across the shipped catalogue, chain scenarios carried
// 127 EXPECT bullets, 38 of them correctly-formed `result.isError == …` that nothing executed.
//
// The three guards below are what stop the MOVE from creating a new way to assert nothing.

func chainMD(trigger, expect string) string {
	return strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata",
		"- **ID**: CHN-001",
		"- **Layer**: Permissions",
		"- **Tags**: chain", "",
		"## TRIGGER",
		"POST `chain`", "",
		"```json",
		trigger,
		"```", "",
		"## EXPECT",
		expect,
		"## TIMEOUT", "120s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

const twoSteps = `{"steps":[
  {"type":"mcp","name":"create","tool":"t_create","args":{}},
  {"type":"mcp","name":"read","tool":"t_read","args":{}}
]}`

const twoStepsLegacy = `{"steps":[
  {"type":"mcp","name":"create","tool":"t_create","args":{},"expect":"result.isError == false"},
  {"type":"mcp","name":"read","tool":"t_read","args":{},"expect":"result.isError == false"}
]}`

// TS-E10-1 / TS-E10-2 — the claim parses, and several bullets on one step are ANDed.
func TestParseChainClaims(t *testing.T) {
	claims, errs := ParseChainClaims([]string{
		"- step create: result.isError == false",
		"- step read: result.isError == false",
		"- step read: body has id containing ${saved.docId}",
		"- a correlation id is threaded across every step", // prose — not this parser's business
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got := claims["create"]; len(got) != 1 || got[0] != "result.isError == false" {
		t.Errorf("create = %v", got)
	}
	if got := claims["read"]; len(got) != 2 || got[1] != "body has id containing ${saved.docId}" {
		t.Errorf("read = %v — several bullets on one step are ANDed, in file order", got)
	}
	if len(claims) != 2 {
		t.Errorf("a prose bullet must not become a claim: %v", claims)
	}
}

// An assertion may itself contain a colon — the NAME runs to the FIRST colon only. Without this the
// grammar would be unusable for the commonest JSON assertion there is.
func TestParseChainClaims_AssertionMayContainAColon(t *testing.T) {
	claims, errs := ParseChainClaims([]string{`- step read: body has error containing "code":"missing_token"`})
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if got := claims["read"]; len(got) != 1 || got[0] != `body has error containing "code":"missing_token"` {
		t.Errorf("read = %v", got)
	}
}

// R3-shaped totality: a bullet that CLAIMS to be a chain claim and parses as none is an ERROR, never
// a shrug. This is the same rule the body grammar follows, for the same reason.
func TestParseChainClaims_UnparseableClaimIsAnError(t *testing.T) {
	for _, b := range []string{
		"- step 4 (`read-fixture-doc`) returns `result.isError == false` with extra prose",
		"- step read",
		"- step : result.isError == false",
	} {
		if _, errs := ParseChainClaims([]string{b}); len(errs) == 0 {
			t.Errorf("ParseChainClaims(%q) returned NO error — a bullet that opens with `step` and "+
				"parses as no claim is exactly the silent skip this round removes", b)
		}
	}
}

// TS-E10-4 — THE ORPHAN GUARD. Renaming a step must not silently orphan its claim.
func TestValidate_ChainOrphanClaimIsRefused(t *testing.T) {
	md := chainMD(twoSteps, "### Runnable\n- step create: result.isError == false\n- step reed: result.isError == false\n")
	_, errs := Validate(md)
	if !find(errs, "names no step in the TRIGGER JSON") {
		t.Fatalf("a claim naming no step must be refused; got %v", errs)
	}
	// …and the message names the real step names, so the fix is one glance away.
	var msg string
	for _, e := range errs {
		if strings.Contains(e.Message, "names no step") {
			msg = e.Message
		}
	}
	if !strings.Contains(msg, "create, read") {
		t.Errorf("the orphan message must list the declared step names, got: %s", msg)
	}
	// Case-sensitivity is part of the contract: `Create` is not `create`.
	if _, errs := Validate(chainMD(twoSteps, "### Runnable\n- step Create: x\n- step read: y\n")); !find(errs, "names no step") {
		t.Error("step names are matched CASE-SENSITIVELY")
	}
}

// TS-E10-5 — THE EMPTY-STEP GUARD. A step nobody asserts anything about is a call whose answer is
// never read.
func TestValidate_ChainStepWithNoClaimIsRefused(t *testing.T) {
	_, errs := Validate(chainMD(twoSteps, "### Runnable\n- step create: result.isError == false\n"))
	if !find(errs, `step "read" has no claim`) {
		t.Fatalf("every mcp step needs at least one claim; got %v", errs)
	}
	// Both claimed ⇒ clean.
	_, errs = Validate(chainMD(twoSteps, "### Runnable\n- step create: result.isError == false\n- step read: result.isError == false\n"))
	for _, e := range errs {
		if strings.Contains(e.Message, "has no claim") || strings.Contains(e.Message, "names no step") {
			t.Errorf("a fully-claimed chain must validate clean; got %v", errs)
		}
	}
}

// ⚠ A `ui` STEP NEEDS NO CLAIM. Its verdict is the Playwright exit code and its assertions live in
// the spec file — the same reason VR12-E11 exempts a whole `ui` scenario. Demanding a claim would be
// demanding one in a grammar that cannot express it.
func TestValidate_ChainUIStepNeedsNoClaim(t *testing.T) {
	md := chainMD(`{"steps":[
  {"type":"mcp","name":"create","tool":"t_create","args":{}},
  {"type":"ui","name":"see-it","spec":"tests/live/x.spec.ts"}
]}`, "### Runnable\n- step create: result.isError == false\n")
	_, errs := Validate(md)
	if find(errs, "has no claim") {
		t.Fatalf("a ui step must not require a claim; got %v", errs)
	}
}

// TS-E10-6 — ONE HOME, REWRITTEN BY V31-002.
//
// It used to assert GUARD 1: both shapes together is refused, and the LEGACY shape alone validates
// clean, because "an un-migrated pack is validated as it is written". Both halves were right while
// the old key still RAN. It no longer does — V31-002 removes it from the struct and the runner — so
// a file carrying it would have its author's claims silently ignored. The key is now refused on its
// own, and the un-migrated pack is exactly what must stop validating: that refusal is the only thing
// that tells its author to rewrite it.
func TestValidate_ChainWithBothHomesIsRefused(t *testing.T) {
	_, errs := Validate(chainMD(twoStepsLegacy, "### Runnable\n- step create: result.isError == false\n- step read: result.isError == false\n"))
	if !find(errs, "carries an `expect` key") {
		t.Fatalf("the removed key must be refused by name; got %v", errs)
	}
	if find(errs, "declares its claims in BOTH homes") {
		t.Errorf("GUARD 1's superseded message is still emitted; got %v", errs)
	}

	// The legacy shape ALONE is now refused too — and for the step that has no claim, twice.
	_, errs = Validate(chainMD(twoStepsLegacy, "### Non-runnable\n- prose\n"))
	if !find(errs, "carries an `expect` key") {
		t.Errorf("an un-migrated chain must be refused, so its author is told to rewrite it; got %v", errs)
	}
	if !find(errs, "has no claim") {
		t.Errorf("and the empty-step guard no longer exempts it; got %v", errs)
	}
}

// TS-E10-7 — a NON-chain scenario's EXPECT is untouched by any of this.
func TestValidate_NonChainScenarioIsUnaffected(t *testing.T) {
	md := strings.Replace(chainMD(twoSteps, "### Runnable\n- status=200\n"), "- **Tags**: chain", "- **Tags**: mcp", 1)
	_, errs := Validate(md)
	for _, e := range errs {
		if strings.Contains(e.Message, "step") && strings.Contains(e.Message, "claim") {
			t.Errorf("a non-chain scenario must not be policed by the chain guards; got %v", errs)
		}
	}
}

// find reports whether any validation error carries sub — shared by every case in this package.
func find(errs []Error, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, sub) {
			return true
		}
	}
	return false
}

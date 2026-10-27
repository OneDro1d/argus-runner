package argus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VR10-S2-14 / VR10-S2-15 / acceptance 13: the chain-scenario contract is written down
// (specs/17-chain-scenarios.md) and the scenario-author skill teaches the assertion form —
// the list form, the captured-variable form, and the stable-values-not-sentences rule.
// Four defects came out of this gap before the contract existed; this test keeps it from
// silently disappearing again.

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s must exist: %v", rel, err)
	}
	return string(b)
}

func TestChainContract_SpecCoversTheStepShape(t *testing.T) {
	spec := readRepoFile(t, "specs/17-chain-scenarios.md")
	for _, must := range []string{
		`"mcp"`, `"ui"`, // the two step types
		"save", "${saved.", "${cid}", // capture + binding grammar
		"server_url", "target", // where a step's endpoint comes from
		"body has", "assertions_enforced", // the assertion form + the per-step report marker
		// ⛔ VR12-E10 (V30-002) — THE CLAIM'S ONE HOME. This block used to require `"expect"` and
		// "documentation", asserting the OPPOSITE contract: that `## EXPECT` was decorative and the
		// step key held the claims. It moved with the rewrite, as the design said it must.
		"- step <name>: <assertion>", // the new form, shown
		"case-sensitively",           // the orphan guard's matching rule
		"orphan guard",               // …named, so it cannot be quietly dropped
		"at least one bullet",        // the empty-step guard
		// ⛔ V31-002: it required "DEPRECATED" and "both" — the step key named as deprecated-not-
		// deleted, and carrying both homes refused. The key is now REMOVED and refused BY NAME on
		// its own, so a guard demanding the old words would hold the spec at the state this round
		// exists to leave behind.
		"REMOVED (V31-002)", // the step key is named as removed
		"has one home",      // …and the claim's single home is stated
	} {
		if !strings.Contains(spec, must) {
			t.Errorf("specs/17-chain-scenarios.md must state %q", must)
		}
	}
}

func TestScenarioAuthorSkill_TeachesTheAssertionForm(t *testing.T) {
	skill := readRepoFile(t, "skills/scenario-author/SKILL.md")
	// V29-011: the authoring rule has TWO homes and they must carry the same words — the skill (installed
	// into every agent folder) and spec 17 §Authoring rule. The two phrases below are checked in BOTH.
	spec := readRepoFile(t, "specs/17-chain-scenarios.md")
	for _, must := range []string{"more than one legitimate rendering", "Measure the answer before you assert"} {
		if !strings.Contains(spec, must) {
			t.Errorf("specs/17-chain-scenarios.md must carry the authoring rule phrase %q (V29-011: the skill and the spec must say it in the same words)", must)
		}
	}
	for _, must := range []string{
		// VR12-E10: the skill teaches the `## EXPECT` home first, and names the deprecated key as
		// deprecated. `"expect": [` survives ONLY inside that deprecation note.
		"step write-doc: result.isError == false", // the new form, worked
		"REMOVED (V31-002)",                       // the old key, named as removed (V31-002)
		"${saved.", "body has", "containing",      // the captured-variable form inside an assertion
		"never prose", "error code", // the stable-values-not-sentences rule
		"more than one legitimate rendering", "Measure the answer before you assert", // V29-011: a stable value can still be SPELLED two ways
		"body has id containing", // the worked MCP form (S2-a)
		"specs/17-chain-scenarios.md",
	} {
		if !strings.Contains(skill, must) {
			t.Errorf("skills/scenario-author/SKILL.md must teach %q", must)
		}
	}
	if strings.Contains(strings.ToLower(skill), "onboarding token") {
		t.Error("the skill must say 'author token (minted during onboarding)', never the retired name (CR-2)")
	}
}

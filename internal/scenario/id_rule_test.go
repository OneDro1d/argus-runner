package scenario

import (
	"strings"
	"testing"
)

// VR10-S4-1..4 (V28-006) — THE ID RULE IS A SAFETY RULE, NOT A HOUSE CONVENTION.
//
// The old grammar (uppercase hyphen-separated segments plus a trailing number) rejected lowercase,
// underscores and a missing trailing number ON PURPOSE — and the import then dropped every such file
// in silence (46 of 50 materialized on a real onboard, 2026-09-01). It is not quoted literally
// anywhere in this file: VR10-S4-11 greps the tree for it, and a test is part of the tree. The owner
// relaxed it to the smallest rule that keeps
// the id safe everywhere it is embedded: the correlation id, a QUOTED LogQL line filter, and the
// dashboard's regex alternation. Letters, digits, "_", "-"; starts with a letter or digit; no spaces;
// at most 64 characters; case is the author's.
func TestValidate_IDSafetyRule(t *testing.T) {
	withID := func(id string) string { return strings.Replace(validMD(), "ORD-003", id, 1) }
	idErr := func(errs []Error) (string, bool) {
		for _, e := range errs {
			if strings.HasPrefix(e.Message, "ID ") {
				return e.Message, true
			}
		}
		return "", false
	}

	// ── VR10-S4-1 / S4-4: every existing id stays valid, and the relaxed shapes are accepted ─────
	accept := []string{
		"ORD-001", "ENG-MCP-001", "RACE-001", "NEO-010", // zero renames: today's ids all still pass
		"ENG-mcp-1",             // mixed case, no trailing-number requirement
		"race-001",              // lowercase
		"neo4j_health",          // an underscore and no number at all
		"X",                     // a single character
		"7up",                   // starts with a digit
		strings.Repeat("a", 64), // exactly the maximum
	}
	for _, id := range accept {
		t.Run("accept/"+id, func(t *testing.T) {
			_, errs := Validate(withID(id))
			if msg, bad := idErr(errs); bad {
				t.Fatalf("ID %q must be VALID under the safety rule; got: %s", id, msg)
			}
		})
	}

	// ── VR10-S4-1 / S4-2: the unsafe shapes are refused, and the refusal says WHY ────────────────
	reject := []string{
		"my scenario",           // whitespace
		"a.b",                   // "." is a metacharacter in the dashboard's regex alternation
		"id(1)",                 // parentheses
		"x|y",                   // the alternation operator itself
		"-lead",                 // must start with a letter or digit
		"_lead",                 // ditto
		strings.Repeat("a", 65), // one over the maximum
	}
	for _, id := range reject {
		t.Run("reject/"+id, func(t *testing.T) {
			_, errs := Validate(withID(id))
			msg, bad := idErr(errs)
			if !bad {
				t.Fatalf("ID %q must be REFUSED, but validated clean", id)
			}
			// The message shows the offending id, states the rule, names the REASON (the id is
			// embedded in every correlation id and in the log queries), and gives a LOWERCASE
			// example — so nobody reads the old uppercase convention back into the new rule.
			for _, want := range []string{`"` + id + `"`, "not allowed", "letters, digits", "max 64", "no spaces", "correlation id", "log quer", "race-001"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal for %q lacks %q:\n  %s", id, want, msg)
				}
			}
			if strings.Contains(msg, oldGrammarMarker) || strings.Contains(msg, "ORD-001") {
				t.Errorf("refusal for %q still teaches the OLD grammar:\n  %s", id, msg)
			}
		})
	}

	// ── VR10-S4-3: `**ID**:` stays REQUIRED — the rule is relaxed, the field is not optional ─────
	t.Run("an empty id is still refused", func(t *testing.T) {
		_, errs := Validate(strings.Replace(validMD(), "- **ID**: ORD-003\n", "", 1))
		for _, e := range errs {
			if strings.Contains(e.Message, "missing required Metadata **ID**") {
				return
			}
		}
		t.Fatalf("a scenario without **ID** must be refused; got %v", errs)
	})
}

// oldGrammarMarker is the middle of the retired uppercase pattern, assembled at run time so that the
// pattern itself never appears verbatim in the repository — which is exactly what VR10-S4-11 asserts.
var oldGrammarMarker = "A-Z]" + "[A-Z0-9"

// mustValidate returns every validation error for a shipped example scenario, minus any covered by a
// NAMED exemption. There are no exemptions today (the suite validated 0-errors on 2026-09-10); the
// hook exists so that if one is ever needed it must be written down with its reason rather than
// hidden behind a filter — which is exactly how the "ID "-only filter came to discard everything else.
func mustValidate(text string) []Error {
	_, errs := Validate(text)
	return errs
}

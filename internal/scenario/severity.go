package scenario

import (
	"fmt"
	"regexp"
	"strings"
)

// VR12-E4 / VR12-E5 (V29-020 points 1-4) — THE SEVERITY SPLIT AT AUTHORING TIME.
//
// The owner's rule, in his own words: ⛔ *a bullet in the EXPECT block is either ENFORCED or
// REPORTED. There is no third state. Silence must not be reachable.*
//
//	E4  a `### Runnable` bullet that does not parse        ⇒ ERROR, refused at write
//	    — it CANNOT BE UNDERSTOOD.
//	E5  a `### Non-runnable` bullet that LOOKS like an
//	    assertion                                          ⇒ WARNING, and the write succeeds
//	    — it IS understood; it is merely in the wrong place.
//
// ⚠ E4 IS DELIBERATELY NARROW, and the reason matters. It fires on a bullet that is
// ASSERTION-SHAPED and matches no known form — never on prose. The owner's lock (findings register
// 1515-1518) is that ONLY assertion-shaped bullets are policed and everything else stays prose;
// widening this to "anything the grammars did not consume" would refuse a `### Runnable` section
// that mixes a claim with a sentence explaining it, which is not what anybody writes rules for.

var (
	// assertionShapedRe is the SHAPE, not the grammar: an assertion operator or verb sitting next to
	// a field-ish token (a dotted/indexed path, or a snake_case name). It mirrors the mcp
	// classifier's `assertionShaped`, which is the same question asked on the other side of the
	// package boundary — see the note in expectShapeErrors about why they are not shared.
	assertVerbShapeRe = regexp.MustCompile(`(?i)==|!=|>=|<=|\bmust\b|\bcontains?\b|\bcontaining\b|\bmatch(?:es|ing)?\b|\bequals\b|\bis null\b`)
	fieldishShapeRe   = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_])(?:[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+|\[[0-9]+\])+|[A-Za-z0-9]+_[A-Za-z0-9_]+)(?:[^A-Za-z0-9_]|$)`)
	// The forms a runnable bullet may take, by dispatch. Each is already implemented by its own
	// grammar; this list is what E4 uses to decide "did ANY grammar claim this bullet?".
	statusShapeRe = regexp.MustCompile(`(?i)^\s*status2?\s*[=:]`)
	rowCountRe    = regexp.MustCompile(`(?i)^\s*(row_count|no rows|0 rows|[0-9]+ (row|message|delivery|delivered))`)
)

// looksLikeAnAssertion reports whether a bullet CLAIMS to assert something. It is the gate on both
// tiers: E4 only refuses what it claims, and E5 only warns about what it claims.
func looksLikeAnAssertion(bullet string) bool {
	b := bulletText(bullet)
	if b == "" {
		return false
	}
	switch {
	case ClaimsToBeBodyAssert(b), ClaimsToBeChainClaim(b), statusShapeRe.MatchString(b), rowCountRe.MatchString(b):
		return true
	}
	return assertVerbShapeRe.MatchString(b) && fieldishShapeRe.MatchString(b)
}

// claimedByAGrammar reports whether some grammar in this product actually CONSUMES the bullet.
//
// ⚠ The mcp / chain step grammars live in internal/argus (the plane table, the classifier), which
// this package cannot import — argus imports scenario. They are reached from the author path anyway,
// through toolcore.ValidateAll, which runs argus.ExpectProblems alongside Validate on ALL THREE
// author paths. So E4 here covers the grammars THIS package owns, and the mcp/chain half is covered
// by ExpectProblems with the same severity. Saying that out loud is the point: the alternative —
// a second copy of the classifier here — is the two-sources-of-truth shape this round keeps removing.
func claimedByAGrammar(s *Scenario, bullet string) bool {
	b := bulletText(bullet)

	// ⛔ EVERY grammar is asked, and ANY claim is enough. An early `switch` on the SHAPE was wrong
	// and the catalogue proved it immediately: MSGF-003's `status == pending` is a message-flow
	// COLUMN assertion, but it opens with the word `status`, so a status-first switch handed it to
	// the status reader, which wants `status=<3-digit code>`, which refused it — and E4 then called
	// a perfectly good shipped bullet unexecutable. The grammars are distinguished by FORM, not by
	// first word, so the only correct question is "does any of them take it?".
	if f, sec := DeclaredStatuses([]string{b}); f > 0 || sec > 0 {
		return true
	}
	if ClaimsToBeBodyAssert(b) {
		got, errs := ParseBodyAsserts([]string{b})
		if len(errs) == 0 && len(got) > 0 {
			return true
		}
	}
	if ClaimsToBeChainClaim(b) {
		got, errs := ParseChainClaims([]string{b})
		if len(errs) == 0 && len(got) > 0 {
			return true
		}
	}
	// The DB / content grammar: a bullet it recognises contributes a column, a row count, or the
	// negative form.
	one := ParseDBExpect([]string{b})
	if len(one.Columns) > 0 || one.RowCount >= 0 || !one.HasRows {
		return true
	}
	// An mcp or chain scenario's remaining forms (the plane phrases) are argus's business — see the
	// note above. Do not refuse them here.
	return NativeEngineTag(s) != ""
}

// expectShapeErrors is E4: an assertion-shaped `### Runnable` bullet that NO grammar consumes.
func expectShapeErrors(s *Scenario, text string, expLine int) []Error {
	var errs []Error
	for _, b := range s.RunnableExpect() {
		if !looksLikeAnAssertion(b) || claimedByAGrammar(s, b) {
			continue
		}
		errs = append(errs, Error{
			Line: lineContaining(text, strings.TrimSpace(b), expLine),
			Message: fmt.Sprintf("`%s` is in `### Runnable` and looks like an assertion, but no grammar "+
				"in this product can execute it — so it would be silently ignored. ⛔ A bullet in "+
				"## EXPECT is either ENFORCED or REPORTED; there is no third state. Rewrite it in a "+
				"form the runner understands, or move it to `### Non-runnable`, where prose about "+
				"what this test proves belongs (V29-020)", bulletText(b)),
		})
	}
	return errs
}

// expectShapeWarnings is E5: a `### Non-runnable` bullet that LOOKS like an assertion.
//
// ⚠ A WARNING, never an error, and the distinction is the owner's: such a bullet IS understood — it
// is merely in the wrong place, and refusing the write would stop an author recording something
// true because they put it one heading too low.
func expectShapeWarnings(s *Scenario) []string {
	var out []string
	for _, b := range s.ExpectNonRunnable {
		if !looksLikeAnAssertion(b) {
			continue
		}
		out = append(out, fmt.Sprintf("EXPECT %q is under `### Non-runnable` but looks like an "+
			"assertion the product could execute — if you meant it as a claim, move it to "+
			"`### Runnable`; if you meant it as documentation, it is fine where it is (V29-020)",
			bulletText(b)))
	}
	return out
}

// bodyHasTextWarnings — V31-004 fix 3 (VR13-BF). `body has text containing …` asks for a JSON
// field NAMED `text`, and almost nothing in this estate answers one.
//
// Since V29-017 the word after `body has` is the FIELD NAME (`bodyassert.go`). Before that it was
// discarded, so `body has text containing X` behaved as a raw substring search over the whole
// answer — which is what every example, hint and skill sentence in the estate was written to mean.
// The grammar changed under them: an MCP tool answer carries its text at `content[0].text`, an HTTP
// JSON body carries whatever the SUT named it, and a top-level `text` is rare. So the form the
// product TAUGHT is the one form that cannot pass on those answers, and 15 shipped examples used it.
//
// ⛔ WARNING, NOT ERROR, and the reason is the owner's severity model: E4 refuses a bullet that
// CANNOT BE UNDERSTOOD; this one is understood perfectly — it is simply unlikely to be meant. A SUT
// that really does answer `{"text": …}` exists, and refusing the write would stop its author saying
// something true. The skill GUIDES, validation REFUSES, the run REPORTS: this is guidance.
//
// ⚠ RUNNABLE ONLY, for the same reason every other rule in this file is: a line the author filed
// under `### Non-runnable` is documentation, and E5 has already told them it looks like an
// assertion. Two warnings about one line is how an author learns to ignore warnings.
//
// ⚠ It covers HTTP scenarios too, not only MCP and chain (the owner's ruling **g**, 2026-09-11):
// the body grammar is shared, and http-ingestion's `expect.body` reads it through the same parser.
func bodyHasTextWarnings(s *Scenario) []string {
	var w []string
	for _, b := range s.RunnableExpect() {
		asserts, _ := ParseBodyAsserts([]string{b})
		for _, a := range asserts {
			if !strings.EqualFold(a.Field, "text") {
				continue
			}
			w = append(w, fmt.Sprintf("EXPECT %q asks for a JSON field called `text` — since V29-017 the word after "+
				"`body has` is a FIELD NAME, not a description of where to look. An MCP tool answer carries its text "+
				"at `content[0].text` and an HTTP body carries whatever the SUT named it, so this check fails on any "+
				"answer with no top-level `text`. To search the WHOLE answer write `body contains <value>` (or "+
				"`body matching <regex>`); to check a real field, name it (`body has content[0].text containing "+
				"<value>`). Keep this bullet only if your SUT really answers {\"text\": …}", b))
		}
	}
	return w
}

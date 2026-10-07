package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// V31-004 fix 2a (VR13-BF) — THE WHOLE-ANSWER FORM IS NEVER SWALLOWED BY A PLANE PHRASE.
//
// Gate 2 (VR10-S2-6) closed the case where a plane phrase swallowed a `body has …` assertion that
// rode along on the same bullet. It closed it with a regex that recognises `body has` and nothing
// else — so the moment this row starts TEACHING `body contains …` and `body matching …`, the form
// every hint now recommends is the one that is still swallowed:
//
//   - `- result.isError == false, body contains X` was accepted as a plain success bullet and the
//     content check silently dropped (measured on 0.3.31);
//   - and because the plane table is matched BEFORE the body branch, `- body contains error code 7`
//     was read as a PROTOCOL-plane expectation (measured: plane "protocol") rather than as the body
//     check it plainly is.
//
// Both halves matter in both orders, which is why the reorder carries its own refusal: once the body
// branch runs first, a plane phrase riding on a body bullet would be the swallowed half instead.
func TestGate2_WholeAnswerFormIsNeverSwallowedByAPlanePhrase(t *testing.T) {
	const refusal = "carries a content assertion beside the plane phrase"

	t.Run("plane first, content second", func(t *testing.T) {
		for _, b := range []string{
			"result.isError == false, body contains argus-race",
			"result.isError == true, body matching ^x",
		} {
			_, err := parseExpectPlane([]string{b})
			if err == nil {
				t.Errorf("%q was ACCEPTED — its content assertion is silently dropped", b)
				continue
			}
			if !strings.Contains(err.Error(), refusal) {
				t.Errorf("%q: error = %v, want it to contain %q", b, err, refusal)
			}
		}
	})

	t.Run("body first, plane second", func(t *testing.T) {
		// The mirror of the case above, and the one the REORDER creates: with the body branch first,
		// a plane phrase riding on a body bullet is the half that would be dropped.
		for _, b := range []string{
			"body contains argus-race; result.isError == false",
			"body has id containing x and result.isError == true",
		} {
			_, err := parseExpectPlane([]string{b})
			if err == nil {
				t.Errorf("%q was ACCEPTED — its plane half is silently dropped", b)
			}
		}
	})

	t.Run("a body bullet that merely mentions a plane word is a BODY check", func(t *testing.T) {
		want, err := parseExpectPlane([]string{"body contains error code 7"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want.ErrorPlane != mcp.PlaneNone {
			t.Errorf("ErrorPlane = %q, want %q — the bullet opens with `body`, so it is a content check", want.ErrorPlane, mcp.PlaneNone)
		}
		if len(want.Body) != 1 {
			t.Fatalf("Body = %+v, want exactly one assertion", want.Body)
		}
		if want.Body[0].Op != mcp.BodyContainsOp || want.Body[0].Value != "error code 7" {
			t.Errorf("Body[0] = %+v, want op %q value %q", want.Body[0], mcp.BodyContainsOp, "error code 7")
		}
	})
}

// V31-004 fix 2 (VR13-BF) — the product's own hints teach the form the product executes.
//
// Since V29-017 the word after `body has` is the NAME OF A JSON FIELD. Every hint still said
// `body has text containing <value>`, which on a JSON answer with no field called `text` can only
// fail — so the product was recommending the one form that could not work.
func TestExpectHints_NeverSuggestBodyHasText(t *testing.T) {
	if got := nearestExpectForm(`content[0].text == "x"`); got != "body contains <value>" {
		t.Errorf("nearestExpectForm(content[0].text == \"x\") = %q, want %q", got, "body contains <value>")
	}
	if got := nearestExpectForm("content[0].text matches x"); got != "body matching <regex>" {
		t.Errorf("nearestExpectForm(content[0].text matches x) = %q, want %q", got, "body matching <regex>")
	}

	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected a refusal, got none", what)
		}
		if !strings.Contains(err.Error(), "body contains <value>") {
			t.Errorf("%s: %v — does not offer `body contains <value>`", what, err)
		}
		if strings.Contains(err.Error(), "body has text") {
			t.Errorf("%s: %v — still teaches `body has text`", what, err)
		}
	}
	_, err := classifyExpectBullet("result.isError == false; body contains x")
	check("the `;` refusal", err)
	_, err = classifyExpectBullet("result.isError == false body has id containing x")
	check("the plane-plus-content refusal", err)
	// ⛔ A third case stood here: expectList's own `;` refusal, on a chain step's legacy in-JSON
	// `expect`. V31-002 removed that key and the function with it, so the form now reaches the one
	// classifier above like every other bullet — which is the point of having one classifier.
}

// ─────────────────────────────────────────────────────────────────────────────────────────────────
// ⛔ RESTORED 2026-09-13. The six tests below were the ORIGINAL blind adversary gate 2 (commit
// da33175). V31-004's commit (73da4f3) rewrote this file and deleted them — silently, because the
// round's parity check compared FAILURES and SKIPS and never asked which tests had stopped existing.
// A vanished regression test is invisible to a green suite, which is the same shape of defect this
// whole round is about.
//
// They are unchanged, and they still hold against the new behaviour: the two "swallowed assertion"
// cases are now REFUSED (they log and return, which is the branch they always allowed for), and the
// parsing cases are about the grammar, not about what the documents teach.
// ─────────────────────────────────────────────────────────────────────────────────────────────────

// TestGate2_SemicolonJoinedBulletKeepsItsContentAssertion is the shape the row itself calls "the
// shape an author will try first" (acceptance 10). On a single-call mcp scenario it must not be
// silently accepted with the body half dropped.
func TestGate2_SemicolonJoinedBulletKeepsItsContentAssertion(t *testing.T) {
	bullets := []string{`result.isError == false; body has text containing argus-race`}
	want, err := parseExpectPlane(bullets)
	if err != nil {
		t.Logf("REFUSED (acceptable): %v", err)
		return
	}
	t.Fatalf("ACCEPTED SILENTLY: parseExpectPlane(%q) = %+v — no error, BodyContains=%q. "+
		"The content assertion was dropped and the step degrades to 'did not error'.",
		bullets[0], want, firstContains(want))
}

// TestGate2_PlanePhraseSwallowsAnAppendedAssertion — the same defect without a ";", to show the
// cause is the substring plane table and not the separator.
func TestGate2_PlanePhraseSwallowsAnAppendedAssertion(t *testing.T) {
	for _, b := range []string{
		`result.isError == false and content[0].text == "argus-race"`,
		`result.isError == false, body has text containing argus-race`,
		`jsonrpc error == 0; body has error_code containing PROVIDER_REJECTED_CONTENT`,
	} {
		want, err := parseExpectPlane([]string{b})
		if err != nil {
			t.Logf("refused (good): %q -> %v", b, err)
			continue
		}
		if firstContains(want) == "" && firstMatches(want) == "" {
			t.Errorf("SILENTLY ACCEPTED with no body assertion: %q -> %+v", b, want)
		}
	}
}

// TestGate2_ProseBulletsFromShippedSuitesAreNotRefused — the over-refusal half. A prose bullet
// naming a snake_case field with the word "contains"/"must" in it is ordinary documentation.
func TestGate2_ProseBulletsFromShippedSuitesAreNotRefused(t *testing.T) {
	prose := []string{
		"the response contains a valid post_id",
		"the run must leave the database untouched",
		"the answer contains the created object id",
		"nothing is written to the audit_log",
	}
	for _, p := range prose {
		b, err := classifyExpectBullet(p)
		if err != nil {
			t.Logf("REFUSED prose bullet %q: %v", p, err)
			continue
		}
		if b.kind != expectProse {
			t.Logf("classified %q as kind=%d (not prose)", p, b.kind)
		}
	}
}

// TestGate2_BodyHasWithNoValue — `body has text` with no value. DF-04's bare form means "the FIELD
// NAME must appear in the body", which for MCP means the literal word "text" must appear in
// result.content[*].text. Recorded so the semantic is visible.
func TestGate2_BodyHasWithNoValue(t *testing.T) {
	want, err := parseExpectPlane([]string{"body has text"})
	if err != nil {
		t.Fatalf("`body has text` refused: %v", err)
	}
	t.Logf("`body has text` -> BodyContains=%q BodyMatches=%q", firstContains(want), firstMatches(want))
	// ⚠ VR12-E8 R2 CHANGED THIS CONTRACT DELIBERATELY. `body has text` used to mean "the literal
	// string 'text' appears somewhere in the answer" — the field name was used as a substring, which
	// is why `body has error containing X` and `body has anything containing X` were the same
	// assertion. It now means THE FIELD `text` EXISTS.
	if len(want.Body) != 1 || want.Body[0].Field != "text" || want.Body[0].Op != mcp.BodyExistsOp {
		t.Errorf("the bare form must be an EXISTS on the field, got %+v", want.Body)
	}
}

// TestGate2_UnicodeQuotesAreNotStripped — an author pasting from a document gets curly quotes; the
// value keeps them and the assertion can then never match.
func TestGate2_UnicodeQuotesAreNotStripped(t *testing.T) {
	want, err := parseExpectPlane([]string{"body has text containing “argus-race”"})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if strings.ContainsRune(firstContains(want), '“') {
		t.Logf("BodyContains keeps the curly quotes: %q — the assertion can only match a SUT that emits them", firstContains(want))
	}
}

// TestGate2_TrailingSpacesAndCase
func TestGate2_TrailingSpacesAndCase(t *testing.T) {
	for _, b := range []string{
		"  body has text containing X   ",
		"BODY HAS TEXT CONTAINING X",
		"- body has text containing X",
	} {
		want, err := parseExpectPlane([]string{b})
		if err != nil {
			t.Errorf("refused %q: %v", b, err)
			continue
		}
		if firstContains(want) != "X" {
			t.Errorf("%q -> BodyContains=%q, want \"X\"", b, firstContains(want))
		}
	}
}

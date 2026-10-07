package scenario

import (
	"strings"
	"testing"
)

// VR12-E1..E3 (V29-020 point 5, V30-003 §4) — `## EXPECT` SPLITS INTO `### Runnable` AND
// `### Non-runnable`, AND POSITION DECIDES WHETHER A BULLET IS A CLAIM — NEVER ITS WORDING.
//
// The owner ruled this twice on 2026-09-10; the SECOND ruling supersedes the first and is the one
// built here: a scenario whose `## EXPECT` carries bullets with NO `### ` sub-heading is REFUSED,
// because the product cannot know whether they are claims. `### Non-runnable` ALONE is legal (a
// scenario that asserts nothing is expressible, and honest about it), and a MISSING sub-section only
// WARNS.
//
// ⛔ THE ASYMMETRY THAT MAKES THIS SAFE, and it is the whole design:
//   - Validate() REFUSES the old shape        → the author path, which is where the rule bites.
//   - Parse() STAYS LENIENT and treats an un-subheaded bullet as RUNNABLE → the RUN path, so every
//     catalogue written before this rule keeps running EXACTLY as it does today. Nothing silently
//     stops being enforced anywhere in the world on the day this ships.
//
// (`scenario.Validate` is reached only from internal/control/cloudtools.go:251,
// internal/toolcore/toolcore.go:777 and :834 — all author-path. The run path only Parses.)
//
// ⛔ `Expect` KEEPS BOTH PARTS, in file order. It is what `Failure.Expected` is built from
// (internal/argus/argus.go:580-581) and what `redactExpected` NILs for the product hat
// (internal/toolcore/toolcore.go:141-157). Narrowing it would change the report and the holdout.
// The runnable subset rides ALONGSIDE it, in ExpectRunnable.

// cutExpect replaces a fixture's whole ## EXPECT section with `body` (an empty body removes the
// section entirely). It splices on the SECTION BOUNDARY, never on a literal.
//
// ⚠ This exists because the literal version FAILED SILENTLY during this build: the moment validMD()
// gained its `### Runnable` line, strings.Replace stopped matching, quietly returned the fixture
// unchanged, and every case below started testing the fixture instead of its own input. A test helper
// that no-ops is worse than one that panics — so this one panics.
func cutExpect(md, body string) string {
	start := strings.Index(md, "## EXPECT\n")
	if start < 0 {
		panic("fixture has no ## EXPECT section")
	}
	rel := strings.Index(md[start:], "\n## TIMEOUT")
	if rel < 0 {
		panic("fixture has no ## TIMEOUT after ## EXPECT")
	}
	end := start + rel + 1 // keep the newline that begins "## TIMEOUT"
	if body == "" {
		return md[:start] + md[end:]
	}
	return md[:start] + "## EXPECT\n" + body + md[end:]
}

func expectMD(body string) string { return cutExpect(validMD(), body) }

func TestExpectSplit_Parse(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantAll       []string
		wantRunnable  []string
		wantNonRun    []string
		wantSubheaded bool
	}{
		{
			name:          "both sub-sections",
			body:          "### Runnable\n- status=202\n- status2=409\n\n### Non-runnable\n- the order is visible to the customer\n",
			wantAll:       []string{"status=202", "status2=409", "the order is visible to the customer"},
			wantRunnable:  []string{"status=202", "status2=409"},
			wantNonRun:    []string{"the order is visible to the customer"},
			wantSubheaded: true,
		},
		{
			name:          "runnable only",
			body:          "### Runnable\n- status=202\n",
			wantAll:       []string{"status=202"},
			wantRunnable:  []string{"status=202"},
			wantNonRun:    nil,
			wantSubheaded: true,
		},
		{
			// The owner: "Non-runnable alone is legal." This is what keeps WEBUI-001 and the nine
			// all-prose chain scenarios saveable.
			name:          "non-runnable ALONE is legal and asserts nothing",
			body:          "### Non-runnable\n- the SPA renders (a non-empty document body)\n",
			wantAll:       []string{"the SPA renders (a non-empty document body)"},
			wantRunnable:  nil,
			wantNonRun:    []string{"the SPA renders (a non-empty document body)"},
			wantSubheaded: true,
		},
		{
			// ⛔ WAS "THE BACKWARD-COMPATIBILITY CASE", REWRITTEN BY V31-002 (R1).
			//
			// Parse used to treat an unsplit `## EXPECT` as all-runnable, so the 119 scenarios that
			// carried the old shape kept running unchanged. That leniency is exactly what let an OLD
			// file be half-executed by new strict code, and the owner ruled the old format
			// unsupported: it is now REFUSED when written and reported `error` when run (R10).
			//
			// The bullets are still PARSED — they are in Expect, so the file can be read and
			// explained — but none of them is runnable, which is what makes the file loud.
			name:          "no sub-heading — the bullets are read, and NONE is runnable",
			body:          "- status=202\n- body has order_id matching ^01[A-Z0-9]+$\n",
			wantAll:       []string{"status=202", "body has order_id matching ^01[A-Z0-9]+$"},
			wantRunnable:  nil,
			wantNonRun:    nil,
			wantSubheaded: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Parse(expectMD(c.body))
			if got := strings.Join(s.Expect, "|"); got != strings.Join(c.wantAll, "|") {
				t.Errorf("Expect (BOTH parts, file order — the holdout field)\n  got:  %v\n  want: %v", s.Expect, c.wantAll)
			}
			if got := strings.Join(s.ExpectRunnable, "|"); got != strings.Join(c.wantRunnable, "|") {
				t.Errorf("ExpectRunnable\n  got:  %v\n  want: %v", s.ExpectRunnable, c.wantRunnable)
			}
			if got := strings.Join(s.ExpectNonRunnable, "|"); got != strings.Join(c.wantNonRun, "|") {
				t.Errorf("ExpectNonRunnable\n  got:  %v\n  want: %v", s.ExpectNonRunnable, c.wantNonRun)
			}
			if s.ExpectSubheaded != c.wantSubheaded {
				t.Errorf("ExpectSubheaded = %v, want %v", s.ExpectSubheaded, c.wantSubheaded)
			}
		})
	}
}

func TestExpectSplit_Validate(t *testing.T) {

	t.Run("VR12-E2: bullets with NO sub-heading are REFUSED", func(t *testing.T) {
		_, errs := Validate(expectMD("- status=202\n- the order is visible to the customer\n"))
		if !find(errs, "### Runnable") {
			t.Fatalf("a bullet directly under ## EXPECT must be refused, naming the sub-sections; got %v", errs)
		}
	})

	t.Run("VR12-E1: an unknown ### sub-section is REFUSED", func(t *testing.T) {
		_, errs := Validate(expectMD("### Runnable\n- status=202\n\n### Notes\n- something\n"))
		if !find(errs, "Notes") {
			t.Fatalf("an undefined ### sub-section must be refused BY NAME; got %v", errs)
		}
	})

	t.Run("VR12-E2: a bullet BEFORE the first ### is REFUSED", func(t *testing.T) {
		_, errs := Validate(expectMD("- status=202\n\n### Runnable\n- status2=409\n"))
		if !find(errs, "### Runnable") {
			t.Fatalf("a bullet stranded above the first sub-heading must be refused; got %v", errs)
		}
	})

	t.Run("both sub-sections validate clean", func(t *testing.T) {
		_, errs := Validate(expectMD("### Runnable\n- status=202\n\n### Non-runnable\n- the order is visible\n"))
		for _, e := range errs {
			if strings.Contains(e.Message, "EXPECT") {
				t.Fatalf("a correctly split EXPECT must validate clean; got %v", errs)
			}
		}
	})

	// ⛔ E3 MEETS E6 — the one place two of this round's rulings touch, resolved deliberately.
	//
	// The owner ruled BOTH: *"Non-runnable alone is legal. Just Warn"* (E3), and *"a scenario judged
	// by response code must declare its status"* (E6, V29-016). They meet on a scenario that is BOTH
	// all-prose AND code-judged.
	//
	// They are not in conflict, because E3's ruling was made ABOUT the exempt shapes and only those:
	// its stated beneficiaries are WEBUI-001 (a `ui` scenario) and the nine all-prose `chain`
	// scenarios — every one of them dispatched to a native Go engine that performs NO response-code
	// verdict. E3 says the SPLIT rule does not require a runnable bullet; it does not say no OTHER
	// rule may. E6 keys on the dispatch tag, so on those same scenarios it never fires.
	//
	// So: all-prose is legal wherever nothing judges by response code, and refused where something
	// does — because there, an all-prose EXPECT is a scenario that will be compared against nothing.
	// ⛔ REWRITTEN BY V31-002 (W2). This asserted that `### Non-runnable` ALONE is legal on a
	// natively-run scenario. It was — deliberately — while V30-002 was still migrating the nine
	// all-prose chain scenarios: VR12-E12 counted BULLETS rather than runnable ones precisely to keep
	// them saveable. V30-002 landed in 0.3.31 and migrated them in the same build, so the window is
	// closed: a scenario that asserts nothing proves nothing, and the run now reports it `error`.
	//
	// What survives from E3 is the part that was never about leniency: the SPLIT rule itself does not
	// demand a runnable bullet, and a `ui` scenario still satisfies W2 by naming its own spec file.
	t.Run("VR12-E3: ### Non-runnable ALONE is refused since V31-002", func(t *testing.T) {
		for _, tags := range []string{"chain", "mcp"} {
			_, errs := Validate(withTags(expectMD("### Non-runnable\n- the SPA renders\n"), tags))
			found := false
			for _, e := range errs {
				if strings.Contains(e.Message, "declares no '### Runnable' bullet") {
					found = true
				}
			}
			if !found {
				t.Errorf("a %s scenario asserting nothing must be refused; got %v", tags, errs)
			}
		}
	})

	t.Run("VR12-E6 still bites: all-prose on a CODE-JUDGED scenario is refused", func(t *testing.T) {
		_, errs := Validate(expectMD("### Non-runnable\n- the SPA renders\n")) // the fixture is plain http
		if !find(errs, "must declare the expected status") {
			t.Fatalf("a code-judged scenario that asserts nothing must be refused by E6 — otherwise E3 "+
				"becomes a way to opt out of every claim; got %v", errs)
		}
	})

	t.Run("VR12-E12: the >=1 rule stays a BULLET count, not a runnable count", func(t *testing.T) {
		// A scenario whose only bullets are non-runnable still satisfies "at least one bullet".
		_, errs := Validate(expectMD("### Non-runnable\n- the SPA renders\n"))
		if find(errs, "must have at least one") {
			t.Fatalf("non-runnable bullets must COUNT toward the >=1 rule; got %v", errs)
		}
	})
}

// VR12-E3: a missing sub-section WARNS — it never blocks. The owner: "Just warn if any runnable or
// non-runnable subsection is missing."
func TestExpectSplit_MissingSubsectionWarns(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"no ### Non-runnable", "### Runnable\n- status=202\n", "Non-runnable"},
		{"no ### Runnable", "### Non-runnable\n- the SPA renders\n", "Runnable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := Warnings(expectMD(c.body))
			for _, w := range ws {
				if strings.Contains(w, c.want) {
					return
				}
			}
			t.Fatalf("a missing %s sub-section must WARN; got %v", c.want, ws)
		})
	}
}

// withTags replaces the fixture's **Tags** line, so a case can choose which ENGINE the scenario
// dispatches to. It panics rather than no-op: see cutExpect's note — a silently-unchanged fixture
// makes every case below it test something other than what it claims.
func withTags(md, tags string) string {
	const key = "- **Tags**: "
	i := strings.Index(md, key)
	if i < 0 {
		panic("fixture has no **Tags** line")
	}
	j := strings.Index(md[i:], "\n")
	if j < 0 {
		panic("fixture's **Tags** line is unterminated")
	}
	return md[:i] + key + tags + md[i+j:]
}

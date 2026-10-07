package scenario

import (
	"fmt"
	"regexp"
	"strings"
)

// V29-021 (VR13-UI) — THE `ui` LAYER'S EXPECT GRAMMAR.
//
// ⛔ THE DEFECT IT CLOSES. A Web UI scenario's `## EXPECT` was read by NOTHING. Its real assertions
// lived inside the Playwright spec file, and the verdict was the process's exit code — so an author
// could write three careful bullets, the product would compare none of them, and the run would go
// green on whatever the spec happened to check. `## EXPECT` looked like the contract and was not.
//
// ⚠ IT INVENTS NO SELECTOR LANGUAGE (the owner's U-3). The selector is a Playwright LOCATOR string,
// which is what the spec already speaks: `.order-banner`, `text=Order created`, `[data-test=total]`.
// A grammar of our own would be a second thing to learn and a second thing to get wrong.
//
// ⚠ It is modelled on bodyassert.go — same shape, same conventions — so the two read alike.

// UIAssert is one `### Runnable` bullet of a `ui` scenario.
type UIAssert struct {
	// Bullet is the trimmed source bullet. It TRAVELS with the assert because the outcomes file,
	// the report and AssertionsEnforced all key on it: the spec writes one entry per bullet, and
	// the runner matches them back by this text.
	Bullet string `json:"bullet"`
	// Kind is what is being asserted.
	Kind string `json:"kind"`
	// Selector, Op and Value are the dom kind only.
	Selector string `json:"selector,omitempty"`
	Op       string `json:"op,omitempty"`
	Value    string `json:"value,omitempty"`
}

// The kinds, and the two dom operators.
const (
	UIKindDOM             = "dom"
	UIKindNoBackendErrors = "no_backend_errors"
	UIKindNoConsoleErrors = "no_console_errors"

	UIOpContains = "contains"
	UIOpMatches  = "matches"
)

var (
	// `dom has <selector>` · `… containing <text>` · `… matching <regex>`.
	//
	// ⚠ THE SELECTOR GROUP IS QUOTE-AWARE, and it has to be: a locator contains spaces
	// (`text=Order created`), so an unquoted `(.+?)` would hand half the locator to the value.
	uiDomRe     = regexp.MustCompile("(?i)^\\s*dom\\s+has\\s+(\"[^\"]*\"|`[^`]*`|'[^']*'|\\S+)(?:\\s+(containing|matching)\\s+(.+?))?\\s*$")
	uiNoBackend = regexp.MustCompile(`(?i)^\s*no\s+backend\s+4xx/5xx\s*$`)
	uiNoConsole = regexp.MustCompile(`(?i)^\s*no\s+console\s+errors?\s*$`)

	// uiClaimRe — a bullet OPENING with one of the three forms claims to be a ui assertion, so a
	// malformed one is an ERROR rather than prose (the rule bodyClaimRe already makes for `body`).
	//
	// ⛔ WHAT THIS BUYS, EXACTLY — and it is less than it looks. It catches a CORRECT opener with a
	// malformed tail: `- dom has`, `- no backend`. It does NOT catch a misspelled opener —
	// `- no backedn 4xx/5xx` parses as prose under any opener list, and no regex over a single
	// bullet could know otherwise. The guarantee there is V31-002's: a scenario that declares no
	// runnable check is refused when it is written and reported `error` when it runs.
	uiClaimRe = regexp.MustCompile(`(?i)^\s*(dom\b|no\s+(backend|console)\b)`)
)

// uiUnquote strips one layer of matching quotes — a selector and a value may both carry them.
func uiUnquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		for _, q := range []byte{'"', '\'', '`'} {
			if s[0] == q && s[len(s)-1] == q {
				return s[1 : len(s)-1]
			}
		}
	}
	return s
}

// ParseUIExpect reads a `ui` scenario's runnable bullets. It returns one UIAssert per bullet that IS
// one, and one error per bullet that CLAIMS to be one and is not — never a silent skip, which is the
// moment a product decides it does not understand a bullet and says nothing (V29-020).
func ParseUIExpect(bullets []string) ([]UIAssert, []error) {
	var out []UIAssert
	var errs []error
	for _, raw := range bullets {
		clause := bulletText(raw)
		switch {
		case uiNoBackend.MatchString(clause):
			out = append(out, UIAssert{Bullet: clause, Kind: UIKindNoBackendErrors})
		case uiNoConsole.MatchString(clause):
			out = append(out, UIAssert{Bullet: clause, Kind: UIKindNoConsoleErrors})
		default:
			m := uiDomRe.FindStringSubmatch(clause)
			if m == nil {
				if uiClaimRe.MatchString(clause) {
					errs = append(errs, fmt.Errorf(UIAssertionFormError, clause))
				}
				continue // ordinary prose
			}
			a := UIAssert{Bullet: clause, Kind: UIKindDOM, Selector: uiUnquote(m[1])}
			if m[2] != "" {
				a.Value = uiUnquote(m[3])
				if strings.EqualFold(m[2], "matching") {
					a.Op = UIOpMatches
					// refused HERE, before a browser is ever started: a regex that cannot compile
					// can only fail at run time, and failing then blames the SUT for an authoring
					// mistake (the rule VR10-S2-7 makes for `body … matching`).
					if _, err := regexp.Compile(a.Value); err != nil {
						errs = append(errs, fmt.Errorf("`- %s` has a `matching` regex that does not compile: %v", clause, err))
						continue
					}
				} else {
					a.Op = UIOpContains
				}
			}
			out = append(out, a)
		}
	}
	return out, errs
}

// UIAssertionFormError is THE sentence — the validator's message, ParseUIExpect's error and the
// preflight refusal's Failure.Observed are one string, so an author is never told three things.
const UIAssertionFormError = "`- %s` is not a `ui` assertion — a Web UI scenario's `### Runnable` bullets are " +
	"`dom has <selector>` (with `containing <text>` or `matching <regex>`), `no backend 4xx/5xx` and " +
	"`no console errors` (V29-021)"

// uiExpectErrors is V29-021's G-4: the AUTHOR-path rule, and it is deliberately STRICTER than the
// one the run path applies.
//
// ⛔ THE TWO RULES, AND WHY THEY DIFFER. ParseUIExpect treats a bullet it cannot parse as PROSE
// unless the bullet CLAIMS to be an assertion — that leniency is what lets a catalogue written
// before this grammar keep running. The validator has no such obligation: a bullet under
// `### Runnable` is a claim BY POSITION (VR12-E1), so a claim this product cannot execute is exactly
// what must not be saved, and the author is standing right there to fix it.
//
// The message is the SAME sentence ParseUIExpect and the preflight refusal use, so an author is
// never told three different things about one mistake.
func uiExpectErrors(s *Scenario, text string, expLine int) []Error {
	if !contains(s.Tags, UITag) {
		return nil
	}
	runnable := s.RunnableExpect()
	asserts, perrs := ParseUIExpect(runnable)
	var errs []Error
	for _, e := range perrs {
		errs = append(errs, Error{Line: expLine, Message: e.Error()})
	}
	if len(asserts)+len(perrs) == len(runnable) {
		return errs
	}
	// the bullets that yielded neither an assert nor an error: prose to the run path, a claim here
	for _, b := range runnable {
		one, oneErr := ParseUIExpect([]string{b})
		if len(one) == 0 && len(oneErr) == 0 {
			errs = append(errs, Error{
				Line:    lineContaining(text, bulletText(b), expLine),
				Message: fmt.Sprintf(UIAssertionFormError, bulletText(b)),
			})
		}
	}
	return errs
}

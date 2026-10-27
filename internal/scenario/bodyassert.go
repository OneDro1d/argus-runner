package scenario

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/mcp"
)

// ⛔ WHY THIS GRAMMAR LIVES IN internal/scenario AND NOT internal/argus.
//
// The rule it enforces (R3) has to bite on the AUTHOR path, and there are THREE of those:
//   internal/toolcore.ValidateScenario   -> scenario.Validate + argus.ExpectProblems
//   internal/toolcore.WriteScenario      -> scenario.Validate ONLY
//   internal/control/cloudtools.go:251   -> scenario.Validate ONLY
//
// Two of the three call scenario.Validate ALONE. A rule added to argus.ExpectProblems is therefore
// REPORTED by author_validate_scenario and SILENTLY BYPASSED by author_write_scenario and by the
// control plane's write — an author is told their scenario is invalid and can save it anyway. That
// trap is named in V29-017's own blind audit and in the V31 solution-architect record, and putting
// the grammar here is what makes it structurally impossible rather than a thing to remember.
//
// internal/mcp does not import this package, so scenario -> mcp is acyclic; argus imports scenario
// and calls straight through.

// VR12-E8 (V29-017) — THE BODY-ASSERTION GRAMMAR. ONE parser, every path, every bullet.
//
// Three defects lived in the six lines this replaces, and each of them made a scenario green while
// proving less than its author believed:
//
//  1. FIRST WINS. The old extractBodyAsserts kept the FIRST `containing` and the FIRST `matching` and
//     dropped every later one — its own comment said so. Measured: SYN-MCP-002 declares both
//     `body has serverInfo containing acme-atlassian-mcp-server` AND `body has protocolVersion
//     containing 2024-11-05`; only the first reached the wire, so the second checked nothing.
//  2. THE FIELD NAME WAS DISCARDED. `body has error containing X` became a raw substring search over
//     the whole response, so it was indistinguishable from `body has anything containing X`.
//  3. THE FIELD-LESS FORM MATCHED NOTHING. The regex was anchored on `body has`, so
//     `body contains "code":"missing_token"` set no property at all and the scenario was judged on
//     its status code alone (measured: SYN-MCP-001).
//
// ⛔ AND THE SILENT SKIP IS GONE. The old parser's `if m == nil { continue }` is the exact line
// V29-020 identified as the moment the product decides it does not understand a bullet and says
// nothing. A bullet that looks like a body assertion and parses as none is now an ERROR the caller
// must surface, never a shrug.

// ⚠ THE TYPE LIVES IN internal/mcp, NOT HERE. argus imports mcp and the reverse would cycle, and the
// MCP judge needs the same shape to evaluate it. This file owns the GRAMMAR; mcp owns the JUDGEMENT.
// Aliased so this package reads naturally.
type BodyAssert = mcp.BodyAssert

const (
	BodyContains = mcp.BodyContainsOp
	BodyMatches  = mcp.BodyMatchesOp
	BodyExists   = mcp.BodyExistsOp
	// BodyEquals (P3 #24a) is an EXACT match — added for the amqp consume step's body claim, which
	// needs to say a field or the whole message IS exactly a value, not merely contains it. Not
	// amqp-only: VR12-E8 is ONE parser for every path, so `body equals …` is legal for http/mcp too.
	BodyEquals = mcp.BodyEqualsOp
	// item 25 — numeric comparisons: `body has <field> >|>=|<|<= <number>`. Always field-scoped (a
	// JSON path in the body) — comparing a bare number to "the whole response" is not a form this
	// grammar offers, so these never appear with Field == "".
	BodyGT  = mcp.BodyGTOp
	BodyGTE = mcp.BodyGTEOp
	BodyLT  = mcp.BodyLTOp
	BodyLTE = mcp.BodyLTEOp
	// Numeric equality, tolerance, the negatives, absence. Judged in Go only.
	BodyEQ          = mcp.BodyEQOp
	BodyWithin      = mcp.BodyWithinOp
	BodyNotContains = mcp.BodyNotContainsOp
	BodyNotMatches  = mcp.BodyNotMatchesOp
	BodyNotEquals   = mcp.BodyNotEqualsOp
	BodyAbsent      = mcp.BodyAbsentOp
)

// IsNewClaimForm reports whether a parsed claim is one of the forms (`==`, `within`, a
// negative, `has no`, `every`) — the forms a plain (JMeter-run) scenario refuses at authoring and an
// executor older than the forms cannot judge.
func IsNewClaimForm(a BodyAssert) bool {
	if a.Every != "" {
		return true
	}
	switch a.Op {
	case BodyEQ, BodyWithin, BodyNotContains, BodyNotMatches, BodyNotEquals, BodyAbsent:
		return true
	}
	return false
}

var (
	// `body has <field> [containing|matching|equals <value>]`. The field pattern permits dots and
	// hyphens, so a dotted path (`data.token`, `content.0.text`) needs no grammar change — R4.
	bodyHasRe = regexp.MustCompile(`(?i)^\s*body\s+has\s+([a-zA-Z0-9_.\-]+)(?:\s+(containing|matching|equals)\s+(.+?))?\s*$`)
	// `body contains <value>` / `body matching <regex>` / `body equals <value>` — the whole-response forms.
	bodyWholeRe = regexp.MustCompile(`(?i)^\s*body\s+(contains|containing|matching|matches|equals)\s+(.+?)\s*$`)
	// item 25 — `body has <field> <op> <threshold>`, permissive on BOTH op and threshold so a typo
	// (`=>` for `>=`, a non-numeric threshold) is refused BY NAME rather than falling through to the
	// generic "matches no known form" message. numericOpFor / strconv.ParseFloat do the real
	// validation; this regex only decides "is the author attempting this form at all".
	bodyNumericAttemptRe = regexp.MustCompile(`(?i)^\s*body\s+has\s+([a-zA-Z0-9_.\-]+)\s*(>=|<=|==|=>|=<|>|<)\s*(\S+)\s*$`)
	// Anything that OPENS with the word "body" is claiming to be a body assertion, so if neither
	// form above accepts it, it is an error rather than prose. This is what makes R3 total.
	bodyClaimRe = regexp.MustCompile(`(?i)^\s*body\b`)

	// . `body extracted <form>` — the same forms, about the document an mcp step's `extract` cut out.
	bodyExtractedRe = regexp.MustCompile(`(?i)^\s*body\s+extracted\b\s*(.*?)\s*$`)

	// . `body has no <field>` — absence.
	bodyHasNoRe = regexp.MustCompile(`(?i)^\s*body\s+has\s+no\s+([a-zA-Z0-9_.\-]+)\s*$`)
	// `body has <field> not containing|matching|equals <value>` — the field-scoped negatives.
	bodyHasNotRe = regexp.MustCompile(`(?i)^\s*body\s+has\s+([a-zA-Z0-9_.\-]+)\s+not\s+(containing|matching|equals)\s+(.+?)\s*$`)
	// `body has <field> not <anything else>` — attempting a negative with a word the grammar lacks, refused by name.
	bodyHasNotAttemptRe = regexp.MustCompile(`(?i)^\s*body\s+has\s+([a-zA-Z0-9_.\-]+)\s+not\b\s*(.*?)\s*$`)
	// `body not contains|containing|matching|matches <value>` — the whole-body negatives.
	bodyWholeNotRe = regexp.MustCompile(`(?i)^\s*body\s+not\s+(contains|containing|matching|matches)\s+(.+?)\s*$`)
	// `body has <field> within <tol> of <reference>` — permissive after `within`, so a malformed one is refused by name.
	bodyWithinAttemptRe = regexp.MustCompile(`(?i)^\s*body\s+has\s+([a-zA-Z0-9_.\-]+)\s+within\b\s*(.*?)\s*$`)
	bodyWithinRestRe    = regexp.MustCompile(`(?i)^(\S+)\s+of\s+(\S+)$`)
	// `body every <array-path> has <rest>` — the rest is any field form; `$` is the whole answer.
	bodyEveryRe = regexp.MustCompile(`(?i)^\s*body\s+every\s+(\$|[a-zA-Z0-9_.\-]+)\s+has\s+(.+?)\s*$`)
)

var errNotPlainNumber = errors.New("not a plain number")

// numericOpFor maps a comparison TOKEN to its BodyAssert op. ok=false for anything that is not one
// of the four this grammar supports (`==`, `=>`, `=<` are common typos for `=`/`>=`/`<=`, caught
// here so the author is told which operator is wrong rather than "matches no known form").
func numericOpFor(tok string) (string, bool) {
	switch tok {
	case ">":
		return BodyGT, true
	case ">=":
		return BodyGTE, true
	case "<":
		return BodyLT, true
	case "<=":
		return BodyLTE, true
	case "==":
		return BodyEQ, true // numeric equality (it was refused by name before)
	}
	return "", false
}

// ParseBodyAsserts parses EVERY body assertion from a set of runnable EXPECT bullets, in order.
//
// It returns one error per bullet that CLAIMS to be a body assertion and is not one — R3. A caller
// on the author path turns those into hard validation errors; a caller on the run path reports them
// through tier 3. Neither may discard them.
func ParseBodyAsserts(bullets []string) ([]BodyAssert, []error) {
	return parseBodyAsserts(bullets, false)
}

// ParseMCPStepBodyAsserts is ParseBodyAsserts for the claims of a chain `mcp` step: it also reads
// `body extracted <form>`, the claim about the document the step's `extract` cut out. Every
// other engine calls ParseBodyAsserts, which refuses that spelling by name.
func ParseMCPStepBodyAsserts(bullets []string) ([]BodyAssert, []error) {
	return parseBodyAsserts(bullets, true)
}

// ClaimIsExtracted reports whether a bullet is written `body extracted …`.
func ClaimIsExtracted(bullet string) bool {
	return !baseGrammarOnly && bodyExtractedRe.MatchString(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bullet), "-")))
}

// baseGrammarOnly (test hook) turns off exactly what added to this parser: the `body extracted`
// spelling. With it on, the parser accepts what the grammar at commit 2dec4e02 accepted, which is how
// TestExtractedSpelling_TheBaseGrammarRefusesEveryForm shows that an older executor refuses the chain.
var baseGrammarOnly bool

func parseBodyAsserts(bullets []string, allowExtracted bool) ([]BodyAssert, []error) {
	var out []BodyAssert
	var errs []error
	for _, raw := range bullets {
		clause := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "-"))
		if !bodyClaimRe.MatchString(clause) {
			continue // not claiming to be a body assertion at all — somebody else's bullet
		}
		// `body extracted <form>`: <form> is parsed by this same function as `body <form>`, so every
		// form, its refusals and its compilable-regex check apply unchanged, then each claim is marked as one about the
		// extracted document. The word after `body` is none of the words any other form opens with (has, every, not,
		// contains, containing, matching, matches, equals), so this never takes a claim that belonged to one of them.
		if m := bodyExtractedRe.FindStringSubmatch(clause); m != nil && !baseGrammarOnly {
			if !allowExtracted {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q is a claim about the document an mcp step's `extract` cut out; "+
					"only a chain `mcp` step that declares `extract` has one", clause))
				continue
			}
			inner, ierrs := parseBodyAsserts([]string{"body " + m[1]}, false)
			if len(ierrs) > 0 {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q (the part after `body extracted`): %v", clause, ierrs[0]))
				continue
			}
			for i := range inner {
				inner[i].Extracted = true
			}
			out = append(out, inner...)
			continue
		}
		// `body every <array-path> has <rest>`: <rest> is parsed by this same
		// function as `body has <rest>` (so every field form, its refusals and its compilable-regex
		// check apply unchanged), then pinned to each element of the array.
		if m := bodyEveryRe.FindStringSubmatch(clause); m != nil {
			inner, ierrs := parseBodyAsserts([]string{"body has " + m[2]}, false)
			if len(ierrs) > 0 {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q (the part after `every %s`): %v", clause, m[1], ierrs[0]))
				continue
			}
			if len(inner) != 1 || inner[0].Every != "" {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q: `body every <array-path> has <field> …` takes exactly one field claim "+
					"after `has`", clause))
				continue
			}
			inner[0].Every = m[1]
			out = append(out, inner[0])
			continue
		}
		if m := bodyHasNoRe.FindStringSubmatch(clause); m != nil {
			out = append(out, BodyAssert{Field: m[1], Op: BodyAbsent})
			continue
		}
		if m := bodyHasNotRe.FindStringSubmatch(clause); m != nil {
			field, op, val := m[1], strings.ToLower(m[2]), unquote(m[3])
			switch op {
			case "containing":
				out = append(out, BodyAssert{Field: field, Op: BodyNotContains, Value: val})
			case "matching":
				if err := compilable(val, clause); err != nil {
					errs = append(errs, err)
					continue
				}
				out = append(out, BodyAssert{Field: field, Op: BodyNotMatches, Value: val})
			default:
				out = append(out, BodyAssert{Field: field, Op: BodyNotEquals, Value: val})
			}
			continue
		}
		if m := bodyHasNotAttemptRe.FindStringSubmatch(clause); m != nil && !bodyHasRe.MatchString(clause) {
			switch strings.ToLower(m[2]) {
			case "containing", "matching", "equals":
				errs = append(errs, fmt.Errorf("EXPECT bullet %q matches no known form: `not %s` needs a value after it "+
					"(`body has %s not %s <value>`)", clause, strings.ToLower(m[2]), m[1], strings.ToLower(m[2])))
				continue
			}
			errs = append(errs, fmt.Errorf("EXPECT bullet %q declares a negative field claim with %q after `not`, which this "+
				"grammar does not support — write `body has %s not containing <value>`, `not matching <regex>` or `not equals <value>`",
				clause, m[2], m[1]))
			continue
		}
		if m := bodyWholeNotRe.FindStringSubmatch(clause); m != nil {
			op, val := strings.ToLower(m[1]), unquote(m[2])
			if op == "matching" || op == "matches" {
				if err := compilable(val, clause); err != nil {
					errs = append(errs, err)
					continue
				}
				out = append(out, BodyAssert{Op: BodyNotMatches, Value: val})
			} else {
				out = append(out, BodyAssert{Op: BodyNotContains, Value: val})
			}
			continue
		}
		if m := bodyWithinAttemptRe.FindStringSubmatch(clause); m != nil {
			field := m[1]
			rest := bodyWithinRestRe.FindStringSubmatch(m[2])
			if rest == nil {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q declares a tolerance claim that is not `body has <field> within "+
					"<tolerance> of <number>` — the tolerance is a non-negative number (`0.5`) or a percentage (`2%%`), and the "+
					"reference a number or one whole `${saved.<var>}`", clause))
				continue
			}
			tol, ref := rest[1], unquote(rest[2])
			if strings.HasPrefix(tol, "-") {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q declares the tolerance %q, which is negative — a tolerance is a "+
					"non-negative number (`0.5`) or a percentage (`2%%`)", clause, tol))
				continue
			}
			if !mcp.ValidTolerance(tol) {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q declares the tolerance %q, which is not a number — write a "+
					"non-negative number (`0.5`) or a number followed by `%%` (`2%%`, relative to the reference)", clause, tol))
				continue
			}
			if _, whole := SavedRefWhole(ref); !whole && !mcp.ExactNumber(ref) {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q declares a tolerance claim against %q, which is not a number — the "+
					"reference must be a plain number (e.g. `100` or `-3.5`) or, in a chain, one whole `${saved.<var>}`", clause, ref))
				continue
			}
			out = append(out, BodyAssert{Field: field, Op: BodyWithin, Value: ref, Tol: tol})
			continue
		}
		if m := bodyHasRe.FindStringSubmatch(clause); m != nil {
			field, op, val := m[1], strings.ToLower(m[2]), unquote(m[3])
			switch op {
			case "containing":
				out = append(out, BodyAssert{Field: field, Op: BodyContains, Value: val})
			case "matching":
				if err := compilable(val, clause); err != nil {
					errs = append(errs, err)
					continue
				}
				out = append(out, BodyAssert{Field: field, Op: BodyMatches, Value: val})
			case "equals":
				out = append(out, BodyAssert{Field: field, Op: BodyEquals, Value: val})
			default:
				// bare `body has <field>` — R2: the field EXISTS.
				out = append(out, BodyAssert{Field: field, Op: BodyExists})
			}
			continue
		}
		if m := bodyWholeRe.FindStringSubmatch(clause); m != nil {
			op, val := strings.ToLower(m[1]), unquote(m[2])
			switch op {
			case "matching", "matches":
				if err := compilable(val, clause); err != nil {
					errs = append(errs, err)
					continue
				}
				out = append(out, BodyAssert{Op: BodyMatches, Value: val})
			case "equals":
				out = append(out, BodyAssert{Op: BodyEquals, Value: val})
			default:
				out = append(out, BodyAssert{Op: BodyContains, Value: val})
			}
			continue
		}
		// item 25 — `body has <field> >|>=|<|<= <number>`. Tried BEFORE the generic fallback so a
		// bad operator or a non-numeric threshold gets a SPECIFIC reason (refused by name), never the
		// generic "matches no known form" — bodyHasRe/bodyWholeRe above cannot have matched this
		// clause (their operator vocabularies don't overlap: "containing"/"matching" words vs. these
		// symbols), so trying this third never steals a clause that belonged to one of them.
		if m := bodyNumericAttemptRe.FindStringSubmatch(clause); m != nil {
			field, opTok, val := m[1], m[2], unquote(m[3])
			op, opOK := numericOpFor(opTok)
			if !opOK {
				errs = append(errs, fmt.Errorf("EXPECT bullet %q declares a numeric body comparison with the "+
					"operator %q, which this grammar does not support — use one of `>`, `>=`, `<`, `<=`, `==`",
					clause, opTok))
				continue
			}
			// the threshold is a plain number OR one whole `${saved.<var>}`, bound
			// from the chain's capture store when the step runs (chain.bindBodyAsserts). The
			// placeholder is kept AS WRITTEN in Value.
			if _, whole := SavedRefWhole(val); !whole {
				// `==` compares exactly (mcp.ExactNumber: a plain decimal), so it refuses what ParseFloat
				// would read as a number but is not one (`inf`, `nan`, hex).
				_, perr := strconv.ParseFloat(val, 64)
				if op == BodyEQ && !mcp.ExactNumber(val) {
					perr = errNotPlainNumber
				}
				if perr != nil {
					errs = append(errs, fmt.Errorf("EXPECT bullet %q declares a numeric body comparison against %q, "+
						"which is not a number — the threshold must be a plain number (e.g. `42` or `3.14`) or, in a "+
						"chain, one whole `${saved.<var>}`",
						clause, val))
					continue
				}
			}
			out = append(out, BodyAssert{Field: field, Op: op, Value: val})
			continue
		}
		errs = append(errs, fmt.Errorf("EXPECT bullet %q looks like a body assertion but matches no known form — "+
			"write `body contains <value>` to search the whole response, `body equals <value>` for an exact match, "+
			"`body has <field> containing <value>` / `body has <field> matching <regex>` / `body has <field> "+
			"equals <value>` to pin it to a field, `body has <field>` to require the field exists, or "+
			"`body has <field> >|>=|<|<=|== <number>` for a numeric comparison, `body has <field> within <tolerance> of "+
			"<number>` for a tolerance (`0.5` absolute, `2%%` relative), `body not contains <value>` / `body not matching "+
			"<regex>` and `body has <field> not containing|matching|equals <value>` for the negatives, `body has no "+
			"<field>` to require the field is absent, or `body every <array-path> has <field> …` (any field form above) "+
			"to hold it for every element of an array", clause))
	}
	return out, errs
}

// unquote strips one layer of surrounding quotes or backticks from an asserted value, and nothing
// else. It must not strip INNER quotes: `"code":"missing_token"` is a perfectly ordinary asserted
// substring and mangling it would silently change what the author asked for.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '`' && s[len(s)-1] == '`') ||
			(s[0] == '"' && s[len(s)-1] == '"' && !strings.Contains(s[1:len(s)-1], `"`)) ||
			(s[0] == '\'' && s[len(s)-1] == '\'' && !strings.Contains(s[1:len(s)-1], `'`)) {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// compilable enforces that a `matching` value is a usable regex. An uncompilable regex that reached
// a run would either panic the assertion or, worse, be skipped and pass.
func compilable(val, clause string) error {
	if _, err := regexp.Compile(val); err != nil {
		return fmt.Errorf("EXPECT bullet %q declares a `matching` regex that does not compile (%v) — "+
			"a regex that cannot be compiled can never be enforced", clause, err)
	}
	return nil
}

// ClaimsToBeBodyAssert reports whether a bullet OPENS with the word "body" — i.e. it is claiming to
// be a body assertion, whether or not it parses as one. Callers use it to decide whether a bullet is
// theirs to police; the parser decides whether it is VALID.
func ClaimsToBeBodyAssert(bullet string) bool {
	return bodyClaimRe.MatchString(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bullet), "-")))
}

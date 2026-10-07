package scenario

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Error is a single validation failure with a 1-based line number (VR-A10).
type Error struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

func (e Error) String() string { return fmt.Sprintf("line %d: %s", e.Line, e.Message) }

// idFormat (VR10-S4-1, V28-006) is a SAFETY rule, not a house convention: letters, digits, "_" and
// "-" only; it starts with a letter or a digit; no whitespace; at most 64 characters; the case is the
// author's. The previous grammar (uppercase segments plus a trailing number) rejected lowercase,
// underscores and a missing number ON PURPOSE — and the import then dropped every such file in
// silence (46 of 50 scenarios materialized on a real onboarding run). Why keep a rule at all: the id
// is embedded verbatim in every correlation id (tr-<run>-<ID>-<8hex>), inside a QUOTED LogQL line
// filter, and — unescaped — inside the dashboard's regex alternation. This character set keeps all
// three correct without touching the dashboard; widening it means escaping the ids there first.
var idFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// idRuleMessage names the offending id, states the rule, gives the REASON, and shows a LOWERCASE
// example — so nobody reads the old uppercase convention back into the new rule.
func idRuleMessage(id string) string {
	return fmt.Sprintf("ID %q is not allowed: an id may contain only letters, digits, \"_\" and \"-\" "+
		"(max 64, no spaces, and it must start with a letter or a digit), because the id is embedded in "+
		"every correlation id and in the log queries built from it. Example: race-001", id)
}

// lineContaining returns the 1-based line number of the first line containing
// substr, or fallback if not found.
func lineContaining(text, substr string, fallback int) int {
	for i, line := range strings.Split(text, "\n") {
		if strings.Contains(line, substr) {
			return i + 1
		}
	}
	return fallback
}

func (s *Scenario) sectionStart(name string, fallback int) int {
	if n, ok := s.sectionLine[name]; ok {
		return n
	}
	return fallback
}

func contains(set []string, v string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// Validate parses the markdown and applies the VR-A10 semantic rules, returning
// the parsed scenario plus any errors (empty = valid). This is complete + final
// (not a stub): structural + semantic, with line-level errors.
func Validate(text string) (*Scenario, []Error) {
	s := Parse(text)
	var errs []Error
	metaLine := s.sectionStart("Metadata", 1)

	// ID required + format
	if s.ID == "" {
		errs = append(errs, Error{Line: metaLine, Message: "missing required Metadata **ID**"})
	} else if !idFormat.MatchString(s.ID) {
		errs = append(errs, Error{
			Line:    lineContaining(text, "**ID**", metaLine),
			Message: idRuleMessage(s.ID),
		})
	}

	// Layer required + each must be canonical
	if len(s.Layers) == 0 {
		errs = append(errs, Error{Line: metaLine, Message: "missing required Metadata **Layer**"})
	} else {
		for _, l := range s.Layers {
			if !contains(CanonicalLayers, l) {
				errs = append(errs, Error{
					Line:    lineContaining(text, "**Layer**", metaLine),
					Message: fmt.Sprintf("unknown layer %q (must be one of: %s)", l, strings.Join(CanonicalLayers, ", ")),
				})
			}
		}
	}

	// ⛔ The "Priority must be canonical" rule is GONE with the key (VR12-M2 / V30-003). Declaring
	// `**Priority**` at all is now the error, and metadataErrors says it was REMOVED rather than
	// mistyped — 119 shipped scenarios carried it, so an author will hit this.

	// VR10-S3-5 (config-free half): the **Target** word means one thing per layer — where it has
	// none (Web UI, External Delivery, a chain's Metadata) it is refused here by name, so
	// author__validate_scenario reports it at write time. Whether the NAME exists is checked
	// against the config by validate-config --scenarios and at run preflight.
	if s.Target != "" {
		if _, terr := TargetKind(s); terr != nil {
			errs = append(errs, Error{Line: lineContaining(text, "**Target**", metaLine), Message: terr.Error()})
		}
	}

	// TRIGGER required: a method+URL or a payload body
	if _, ok := s.sectionLine["TRIGGER"]; !ok {
		errs = append(errs, Error{Line: 1, Message: "missing required ## TRIGGER section"})
	} else if s.Trigger.Method == "" && s.Trigger.Payload == "" {
		errs = append(errs, Error{
			Line:    s.sectionStart("TRIGGER", 1),
			Message: "## TRIGGER must contain a METHOD `URL` line or a payload body",
		})
	}

	// Guard (PERM-002 / PERM-007 footgun): parseTriggerHeaders reads `Header: value` lines only
	// BEFORE the first ``` fence. A header buried inside a (languageless) code fence is silently
	// dropped, so the runner falls back to the default valid token and a wrong/empty-token scenario
	// rides that token — reporting e.g. 202 instead of 401 (a false "auth bypass"). Catch it here.
	if trig, ok := splitSections(text)["TRIGGER"]; ok {
		inFence, bareFence := false, false
		for _, raw := range strings.Split(trig.body, "\n") {
			l := strings.TrimSpace(raw)
			if strings.HasPrefix(l, "```") {
				if inFence {
					inFence, bareFence = false, false
				} else {
					inFence, bareFence = true, strings.TrimSpace(strings.TrimPrefix(l, "```")) == ""
				}
				continue
			}
			if inFence && bareFence && !methodRe.MatchString(l) && headerRe.MatchString(l) {
				errs = append(errs, Error{
					Line:    lineContaining(text, l, trig.line),
					Message: fmt.Sprintf("TRIGGER header %q is inside a code fence and will NOT be sent — header lines are read only before the first ```; place them bare, between the METHOD line and the payload fence (cf. MSGF-005)", l),
				})
				break
			}
		}
	}

	// EXPECT required: at least one assertion.
	// ⛔ VR12-E12 is a BULLET count — it covers a `## EXPECT` with nothing in it at all. The RUNNABLE
	// count is E12.a below. (Until V31-002 this was deliberately the only one of the two: counting
	// bullets rather than runnable bullets is what kept WEBUI-001 and the nine all-prose chain
	// scenarios saveable while V30-002 was still migrating them. V30-002 landed in 0.3.31 and those
	// nine were migrated in the same build, so the reason for the leniency is gone.)
	if len(s.Expect) == 0 {
		errs = append(errs, Error{
			Line:    s.sectionStart("EXPECT", 1),
			Message: "## EXPECT must have at least one '- ' assertion",
		})
	}

	// VR12-E12.a (V31-002): a scenario whose engine judges by ## EXPECT must declare at least one RUNNABLE check —
	// a scenario that asserts nothing proves nothing, and from 0.3.32 the run reports it as `error` (R10). The bullet
	// count above stays for a file with no bullets at all. ⚠ A `ui` scenario passes this rule EITHER WAY: with a
	// runnable bullet, or by naming its own spec file, where its assertions live (VR12-E11, and V29-021 gives it the
	// bullets). UINamesItsOwnSpec reads the TRIGGER: a non-empty `spec` field in the payload JSON, or a non-empty
	// TRIGGER url — the same two places internal/argus reads it from (ui_scenario.go:108-113).
	if len(s.Expect) > 0 && len(s.RunnableExpect()) == 0 && !(contains(s.Tags, UITag) && UINamesItsOwnSpec(s)) {
		errs = append(errs, Error{
			Line: s.sectionStart("EXPECT", 1),
			Message: "## EXPECT declares no '### Runnable' bullet — a scenario that asserts nothing proves nothing, " +
				"and the run reports it as `error` (V31-002). Put at least one claim under '### Runnable'",
		})
	}

	// ── VR12-E1/E2 — ## EXPECT is split, and POSITION decides whether a bullet is a claim ──────────
	//
	// The owner ruled this twice on 2026-09-10; the SECOND ruling is built here. A bullet sitting
	// directly under `## EXPECT` is REFUSED, because the product cannot know whether it is a claim —
	// and guessing from its wording is the defect this whole round exists to remove.
	//
	// ⚠ Refused HERE and nowhere else: Validate is author-path only (reached from
	// internal/control/cloudtools.go:251, internal/toolcore/toolcore.go:777 and :834). Parse stays
	// lenient, so every catalogue written before this rule keeps RUNNING untouched — it simply cannot
	// be re-saved until its sub-headings are added.
	if expLine, ok := s.sectionLine["EXPECT"]; ok && len(s.Expect) > 0 {
		if len(s.ExpectUnknownSubsections) > 0 {
			for _, name := range s.ExpectUnknownSubsections {
				errs = append(errs, Error{
					Line: lineContaining(text, "### "+name, expLine),
					Message: fmt.Sprintf("## EXPECT has an undefined sub-section %q — only '### Runnable' and "+
						"'### Non-runnable' are defined (V30-003). A claim goes in '### Runnable'; an expected "+
						"result the product cannot check automatically goes in '### Non-runnable'", name),
				})
			}
		}
		if !s.ExpectSubheaded {
			errs = append(errs, Error{
				Line: expLine,
				Message: "## EXPECT must be split into '### Runnable' and/or '### Non-runnable' — a bullet " +
					"directly under ## EXPECT is refused, because nothing can tell whether it is a claim the " +
					"product must execute or an expected result it cannot check. Put every claim under " +
					"'### Runnable'; put prose describing an expected result under '### Non-runnable'",
			})
		} else if s.ExpectStrandedBullets > 0 {
			errs = append(errs, Error{
				Line: expLine,
				Message: fmt.Sprintf("## EXPECT has %d bullet(s) ABOVE the first '### ' sub-heading — every bullet "+
					"must sit under '### Runnable' or '### Non-runnable'", s.ExpectStrandedBullets),
			})
		}
	}

	// ── VR12-E4 (V29-020) — ENFORCED OR REPORTED, NEVER SILENT ───────────────────────────────────
	//
	// An assertion-shaped `### Runnable` bullet that no grammar can execute is an ERROR: it cannot
	// be understood, so leaving it would put a claim in the file that the run ignores without a word.
	// Its twin, E5, WARNS about the opposite mistake (an assertion under `### Non-runnable`) because
	// that one IS understood — it is merely in the wrong place.
	errs = append(errs, expectShapeErrors(s, text, s.sectionStart("EXPECT", metaLine))...)
	// V31-003: a check may name the run's own id (and, in a chain, a saved variable) and NOTHING
	// else — a check's value is printed in the report, so an environment variable in one is a leak.
	errs = append(errs, checkPlaceholderErrors(s, text, s.sectionStart("EXPECT", metaLine))...)
	// V29-021: a `ui` scenario's runnable bullets must be executable by the ui grammar — position
	// is what makes a bullet a claim, so a claim nothing can run must not be saved.
	errs = append(errs, uiExpectErrors(s, text, s.sectionStart("EXPECT", metaLine))...)

	// ── VR12-E8 R3 — A RUNNABLE BULLET THAT CLAIMS TO BE A BODY ASSERTION MUST PARSE ──────────────
	//
	// The bullet `- body has order_id containing` (no value) used to be accepted, computed into
	// nothing, and the scenario ran green on its status code alone. The parser now returns an error
	// for it, and THIS is where an author is refused.
	//
	// ⛔ IT LIVES IN Validate, NOT IN argus.ExpectProblems. There are three author paths and only ONE
	// of them adds ExpectProblems (toolcore.ValidateScenario); author__write_scenario and the control
	// plane's write call Validate ALONE. A rule added to ExpectProblems is therefore REPORTED by
	// author__validate_scenario and SILENTLY BYPASSED by the two paths that actually SAVE the file —
	// the author is told the scenario is invalid and stores it anyway. Guarded by
	// TestWriteRefusesEverythingValidateRefuses.
	//
	// Scope: `### Runnable` only. A body sentence under `### Non-runnable` is prose by position and
	// this grammar is not its business — that is the entire point of the split.
	bodyAsserts, berrs := ParseBodyAsserts(s.RunnableExpect())
	if len(berrs) > 0 {
		expLine := s.sectionStart("EXPECT", metaLine)
		for _, be := range berrs {
			errs = append(errs, Error{
				Line:    expLine,
				Message: be.Error(),
			})
		}
	}

	// ── item 25 — A NUMERIC BODY COMPARISON ONLY RUNS WHERE IT IS EVALUATED IN GO ─────────────────
	//
	// The numbered body-assert props (`expect.body.N.op`, wired generically for EVERY op by
	// DeriveProps) reach a JMeter template's own JSR223Assertion script, and that script's `op`
	// switch (templates/http-ingestion.jmx) only understands "exists"/"matches"/"contains"/"equals".
	// Since any other op string FAILS the sample by name (`BODY-ASSERT-FAIL:
	// unsupported body-assertion operator '<op>' …`) instead of falling back to `contains` — so a
	// numeric op that reached the template would turn every run red without being evaluated; it is
	// refused here, at write time, instead. A chain `http` step and an `mcp` scenario judge this
	// grammar in Go (mcp.BodyAssert), where the new ops are fully understood, so the rule is keyed on
	// the SAME dispatch question V29-016 already answers — NativeEngineTag, not the layer — exactly
	// like JudgedByResponseCode above.
	if NativeEngineTag(s) == "" {
		for _, a := range bodyAsserts {
			if a.Op == BodyGT || a.Op == BodyGTE || a.Op == BodyLT || a.Op == BodyLTE {
				errs = append(errs, Error{
					Line: s.sectionStart("EXPECT", metaLine),
					Message: "a numeric body comparison (`>`, `>=`, `<`, `<=`) is only evaluated by the " +
						"chain `http` step and the `mcp` engine (both judge it in Go) — this scenario carries " +
						"no `chain`/`mcp`/`ui` tag, so it would reach a JMeter template whose assertion script " +
						"does not understand the operator and would silently fall back to a substring check. " +
						"Move this claim to a chain `http` step or an `mcp` scenario, or use `body has <field> " +
						"containing <value>` instead",
				})
				break
			}
		}
	}

	// ── VR12-E6 (V29-016) — A SCENARIO JUDGED BY RESPONSE CODE MUST DECLARE ITS STATUS ──────────
	//
	// The defect: a scenario whose only bullet was `body has error containing -32601` declared no
	// status, the runner silently substituted `expect.status=202`, the real response was 200, the
	// scenario went red, and the report blamed the body assertion — which had PASSED. Two operators,
	// two days, two machines. The same shape as RO-09 below: an assertion that degrades into
	// something the author never wrote.
	//
	// ⚠ KEYED ON THE TAG, NOT THE LAYER — TEMPORARILY, AND ON PURPOSE. The runner dispatches by tag
	// (argus.go:493-505); a chain/mcp/ui scenario is judged by a native Go engine that performs NO
	// response-code verdict. Measured 2026-09-09 over 119 example scenarios: 116 declare a status
	// LAYER, and 83 of those run natively by tag — a layer-keyed rule would wrongly refuse every one
	// of them. scenario.JudgedByResponseCode asks exactly what the runtime asks.
	//
	// ⛔ WHEN V28-018 LANDS (layer semantics made trustworthy) THIS MUST BE RE-KEYED TO THE LAYER.
	// That re-keying is recorded in V28-018's own definition of done. The layer IS the correct key:
	// specs/06-scenarios-orderservice.md:19-25 defines HTTP Ingestion as "makes an HTTP call and
	// verifies its response", and every one of its scenarios states a status code by construction.
	if JudgedByResponseCode(s) && !DeclaresStatus(s) {
		errs = append(errs, Error{
			Line: s.sectionStart("EXPECT", metaLine),
			Message: "this scenario's verdict is decided by comparing HTTP response codes (it carries no " +
				"`chain`/`mcp`/`ui` tag and its layer is not a content layer), so ## EXPECT must declare " +
				"the expected status: add a bullet `- status=<code>` under '### Runnable'. Without it " +
				"there is nothing to compare against, and no code is invented on your behalf (V29-016)",
		})
	}

	// ── VR12-E10 (V30-002) — A CHAIN'S CLAIMS LIVE IN `## EXPECT`, AND NOWHERE ELSE ───────────────
	//
	// Measured before the change: chain scenarios carried 127 EXPECT bullets, 38 of them
	// `result.isError == …` — correctly-formed assertions, in the decorative half of the file,
	// executed by nothing. The claims move into `### Runnable` as `- step <name>: <assertion>`, the
	// step's own `expect` key is deleted, and the three guards below are what stop the move from
	// creating a new way to assert nothing.
	if contains(s.Tags, ChainTag) {
		expLine := s.sectionStart("EXPECT", metaLine)
		claims, claimErrs := ParseChainClaims(s.RunnableExpect())
		for _, ce := range claimErrs {
			errs = append(errs, Error{Line: expLine, Message: ce.Error()})
		}
		steps, perr := ParseChainSteps(s.Trigger.Payload)
		switch {
		case perr != nil:
			// The TRIGGER is not decodable, so nothing below can be checked against it. Say that, and
			// do not also emit orphan errors that would only be artefacts of the parse failure.
			errs = append(errs, Error{
				Line:    s.sectionStart("TRIGGER", metaLine),
				Message: "a chain scenario's TRIGGER must be a JSON `{\"steps\":[…]}` payload: " + perr.Error(),
			})
		default:
			byName := map[string]ChainStep{}
			for _, st := range steps {
				byName[st.Name] = st
			}

			// ⛔ W1 (V31-002) — A STEP CARRYING `expect` IS REFUSED BY NAME.
			//
			// GUARD 1, "one home", stood here: it refused a file that used BOTH homes and accepted one
			// that used only the old one. That was right while the key still RAN. It no longer does —
			// R2 deletes ChainStep.Expect entirely — so a file carrying it would now simply have its
			// author's claims ignored: it would save, run, and prove nothing. Refusing the key on its
			// own, whether or not `## EXPECT` carries claims, is what makes the removal visible.
			//
			// The struct no longer has the field, so the steps are decoded a SECOND time as raw JSON
			// to ask whether the key is present at all. ParseChainSteps keeps its plain json.Unmarshal,
			// so this is the only place that needs to know the old key's name.
			for i, raw := range rawChainSteps(s.Trigger.Payload) {
				if _, ok := raw["expect"]; !ok {
					continue
				}
				name := chainStepName(raw, i)
				errs = append(errs, Error{
					Line: s.sectionStart("TRIGGER", metaLine),
					Message: fmt.Sprintf("step %q carries an `expect` key in the TRIGGER JSON — the key was REMOVED "+
						"in V31-002. Move each assertion to `## EXPECT` → `### Runnable` as `- step %s: <assertion>`; "+
						"old-format scenarios are not supported and must be rewritten", name, name),
				})
			}

			// GUARD 2 — THE ORPHAN GUARD. An assertion now references its step BY NAME, so renaming a
			// step silently orphans its claim. That would be a NEW way to produce a scenario that
			// asserts nothing — the exact defect class this round closes — so it is part of the design,
			// not a follow-up. Names match exactly and case-sensitively.
			for _, name := range ChainClaimNames(claims) {
				if _, ok := byName[name]; !ok {
					errs = append(errs, Error{
						Line: expLine,
						Message: fmt.Sprintf("`- step %s: …` names no step in the TRIGGER JSON (the names are "+
							"matched exactly and case-sensitively; this chain declares: %s) — an orphaned claim "+
							"is executed by nothing (V30-002)", name, strings.Join(stepNames(steps), ", ")),
					})
				}
			}

			// GUARD 3 — THE EMPTY-STEP GUARD, and VR12-E10.a: for a chain, the "at least one bullet"
			// requirement is one bullet PER STEP, not one per scenario. A step nobody asserts anything
			// about is a call whose answer is never read.
			//
			// ⚠ `mcp` STEPS ONLY. A `ui` step's verdict is the Playwright exit code and its assertions
			// live in the spec file — the same reason VR12-E11 exempts a whole `ui` scenario. Demanding
			// a claim for one would be demanding a claim in a grammar that cannot express it.
			// ⛔ V31-002: the `if len(legacy) == 0 {` wrapper is GONE. It exempted a chain whose steps
			// carried the old key from being asked for claims at all — so such a file was never told
			// its steps assert nothing. A step carrying the key AND no claim now gets both errors,
			// because two things really are wrong with it.
			{
				for _, st := range steps {
					// AC-D20: an http step needs at least one claim too — there is nothing else that
					// decides pass/fail for it (no error plane to fall back on, unlike ui). Its own
					// `poll` check below fires a MORE SPECIFIC message when the step also polls, so
					// this generic one is skipped for that case rather than saying the same thing
					// twice.
					if st.Type == "http" && st.Poll != nil {
						continue
					}
					// AC-D18b: an amqp step too — its only verdict is its `broker …` claim.
					if (st.Type != "mcp" && st.Type != "http" && st.Type != "amqp") || len(claims[st.Name]) > 0 {
						continue
					}
					minimum := "result.isError == false"
					if st.Type == "amqp" {
						minimum = "broker accepts` or `- step " + st.Name + ": broker refuses with <code>"
					}
					errs = append(errs, Error{
						Line: expLine,
						Message: fmt.Sprintf("step %q has no claim — every `%s` step needs at least one "+
							"`- step %s: <assertion>` bullet under '### Runnable', or its answer is never read "+
							"(V30-002). The minimum is `- step %s: %s`", st.Name, st.Type, st.Name, st.Name, minimum),
					})
				}
			}

			// AC-D20 — HTTP STEP SHAPE. Refused BY NAME, never silently accepted and left to fail
			// unhelpfully at preflight: a missing method/url, a poll with nothing to check on each
			// attempt, a poll timeout too large to be a sane bound on a run, and any field this
			// step's TYPE does not carry (the struct is shared across mcp/ui/http, so a stray `tool`
			// on an http step would otherwise be accepted and silently ignored).
			httpAllowedFields := map[string]bool{
				"type": true, "name": true, "method": true, "url": true, "headers": true,
				"body": true, "save": true, "poll": true, "always": true,
			}
			rawSteps := rawChainSteps(s.Trigger.Payload)

			// AC-D18b — AMQP STEP SHAPE AND CLAIMS (amqpstep.go): the one-publish rule (no `poll`, no
			// `always`), url_env as a NAME, the per-op fields, and exactly the two `broker …` claim
			// forms — plus a `broker …` claim on any other step type, refused.
			errs = append(errs, amqpStepErrors(steps, rawSteps, claims, s.sectionStart("TRIGGER", metaLine), expLine)...)

			// P4 — every `save` entry is judged once (SaveProblem), on whatever step carries one.
			for _, st := range steps {
				for _, why := range SaveProblems(st.Save) {
					errs = append(errs, Error{
						Line:    s.sectionStart("TRIGGER", metaLine),
						Message: fmt.Sprintf("step %q: %s", st.Name, why),
					})
				}
			}
			// — the `unreachable` claim's own shape rules.
			errs = append(errs, unreachableStepErrors(steps, claims, s.sectionStart("TRIGGER", metaLine))...)
			// P2 — a numeric threshold `${saved.<var>}` must be saved by an EARLIER step.
			for _, why := range savedThresholdProblems(steps, claims) {
				errs = append(errs, Error{Line: expLine, Message: why})
			}

			for i, st := range steps {
				if st.Type != "http" {
					continue
				}
				// — an http step's claims are parsed HERE, by the parser the executor
				// uses (HTTPStepClaims), so a claim the executor refuses at preflight is refused at
				// write time with the same reason.
				if _, _, cerr := HTTPStepClaims(claims[st.Name]); cerr != nil {
					errs = append(errs, Error{
						Line:    expLine,
						Message: fmt.Sprintf("step %q: %s", st.Name, cerr.Error()),
					})
				}
				if strings.TrimSpace(st.Method) == "" || strings.TrimSpace(st.URL) == "" {
					errs = append(errs, Error{
						Line: s.sectionStart("TRIGGER", metaLine),
						Message: fmt.Sprintf("step %q is an `http` step but declares no `method`/`url` — "+
							"both are required (AC-D20)", st.Name),
					})
				}
				// Headers is decoded leniently (ChainStep.Headers, shared with the amqp `publish`
				// op's custom headers — P3 #24b) precisely so a bad value is refused BY NAME instead
				// of failing the step's whole decode with a generic Go type error.
				hdrs, bad := HeaderValues(st.Headers)
				for _, k := range bad {
					errs = append(errs, Error{
						Line: s.sectionStart("TRIGGER", metaLine),
						Message: fmt.Sprintf("step %q: header %q must be a string value — a header is always "+
							"sent as text (AC-D20)", st.Name, k),
					})
				}
				// #606: the same two reserved names as ## TRIGGER (V29-018), on a chain http step's own
				// headers, refused BY NAME. Sorted so the errors come out in one order.
				var reserved []string
				for k := range hdrs {
					if ReservedHeader(k) != "" {
						reserved = append(reserved, k)
					}
				}
				sort.Strings(reserved)
				for _, k := range reserved {
					errs = append(errs, Error{
						Line: s.sectionStart("TRIGGER", metaLine),
						Message: fmt.Sprintf("step %q declares the header %q, which a scenario may not set: %s. "+
							"Remove it (V29-018)", st.Name, k, ReservedHeader(k)),
					})
				}
				// item 25 — the SAME basic-auth helper form, and the SAME by-name refusal, on a chain
				// http step's own `headers` (independent of the scenario's TRIGGER-level Authorization,
				// which the earlier check above already polices).
				if v, ok := hdrs["Authorization"]; ok {
					if why := ValidateBasicAuthHeader(v); why != "" {
						errs = append(errs, Error{
							Line:    s.sectionStart("TRIGGER", metaLine),
							Message: fmt.Sprintf("step %q's Authorization header %s", st.Name, why),
						})
					}
				}
				if st.Poll != nil {
					if len(claims[st.Name]) == 0 {
						errs = append(errs, Error{
							Line: expLine,
							Message: fmt.Sprintf("step %q declares `poll` but has no claim — a poll re-sends "+
								"the request until the step's claims pass, so with none it would just spin for "+
								"the whole timeout every run; add `- step %s: <assertion>` under '### Runnable' "+
								"(e.g. `status=200`)", st.Name, st.Name),
						})
					}
					if to, terr := time.ParseDuration(st.Poll.Timeout); terr != nil {
						errs = append(errs, Error{
							Line: s.sectionStart("TRIGGER", metaLine),
							Message: fmt.Sprintf("step %q declares `poll.timeout` %q, which is not a valid "+
								"duration (Go syntax: \"180s\", \"3m\") (AC-D20)", st.Name, st.Poll.Timeout),
						})
					} else if to > 10*time.Minute {
						errs = append(errs, Error{
							Line: s.sectionStart("TRIGGER", metaLine),
							Message: fmt.Sprintf("step %q declares `poll.timeout` of %s, which is more than the "+
								"10m bound on a chain step's poll (AC-D20)", st.Name, to),
						})
					}
					if st.Poll.Interval != "" {
						if _, ierr := time.ParseDuration(st.Poll.Interval); ierr != nil {
							errs = append(errs, Error{
								Line: s.sectionStart("TRIGGER", metaLine),
								Message: fmt.Sprintf("step %q declares `poll.interval` %q, which is not a valid "+
									"duration (Go syntax: \"5s\") (AC-D20)", st.Name, st.Poll.Interval),
							})
						}
					}
				}
				if i < len(rawSteps) {
					for k := range rawSteps[i] {
						if !httpAllowedFields[k] {
							errs = append(errs, Error{
								Line: s.sectionStart("TRIGGER", metaLine),
								Message: fmt.Sprintf("step %q is an `http` step and carries the field %q, which "+
									"an http step does not accept (AC-D20)", st.Name, k),
							})
						}
					}
				}
			}
		}
	}

	// ── VR12-C1 / VR12-C3 (V29-015) — `## CLEANUP` IS A CONTRACT ─────────────────────────────────
	//
	// It was parsed and read by NOTHING while four documents told authors it was enforced. The
	// validation half and the execution half ship together on purpose: requiring a runnable command
	// while nothing runs it would force authors to write deletion code guaranteed never to execute —
	// "turning quiet prose into confident lies, strictly worse than today".
	//
	// Measured blast radius over the 119 shipped scenario files: 73 pass as they stand (70 `N/A` +
	// justification, 3 runnable) and 46 are refused (43 prose, 3 with no section). ⚠ WHAT MUST NOT
	// CHANGE: a proper `N/A — <reason>` keeps validating EXACTLY as today — 70 of 119 are in that
	// shape and are correct as they stand; this rule must not generate 70 new authoring errors.
	cleanLine := s.sectionStart("CLEANUP", metaLine)
	switch {
	case !s.CleanupDeclared:
		errs = append(errs, Error{
			Line: cleanLine,
			Message: "## CLEANUP is required — add a ```sql or ```bash block that removes what this " +
				"scenario creates, or `N/A — <why nothing needs cleaning up>`. A scenario that leaves " +
				"data behind on every run is how a suite poisons the system it tests (V29-015)",
		})
	case len(s.CleanupUnknownFences) > 0:
		// VR12-C3 — refused BY NAME. The old parser dropped these silently, which is how an author's
		// deletion code could sit in a file for months doing nothing.
		errs = append(errs, Error{
			Line: cleanLine,
			Message: fmt.Sprintf("## CLEANUP declares a block this runner cannot execute (%s) — only "+
				"```sql and ```bash/```sh run. Rewrite it in one of those, or state `N/A — <reason>` "+
				"(V29-015)", strings.Join(s.CleanupUnknownFences, ", ")),
		})
	case len(s.Cleanup.Blocks) == 0:
		errs = append(errs, Error{
			Line: cleanLine,
			Message: "## CLEANUP is present but says nothing this runner can act on — it must be a " +
				"```sql or ```bash block, or text beginning `N/A` followed by a justification. Prose " +
				"describing what someone ought to delete is not a cleanup (V29-015)",
		})
	case s.Cleanup.IsNA() && !cleanupJustified(s.Cleanup.Blocks[0].Body):
		// The escape hatch is accepted deliberately — no validator can tell an honest N/A from a lazy
		// one — but `N/A` alone is not a reason, and the written justification IS the record.
		errs = append(errs, Error{
			Line: cleanLine,
			Message: "## CLEANUP says `N/A` with no justification — write `N/A — <why this scenario " +
				"leaves nothing behind>`. The justification is the whole value of the exemption (V29-015)",
		})
	}

	// ── VR12-T3.a / T3.b (V29-018) — TWO HEADER NAMES AN AUTHOR MAY NOT SET ──────────────────────
	//
	// Refused BY NAME, never silently ignored. Silently ignoring an author's header is exactly the
	// defect this row is about; doing it deliberately for two names, without saying so, would be the
	// same defect wearing a justification.
	for name := range s.Trigger.Headers {
		if why := ReservedHeader(name); why != "" {
			errs = append(errs, Error{
				Line: lineContaining(text, name+":", s.sectionStart("TRIGGER", metaLine)),
				Message: fmt.Sprintf("## TRIGGER declares the header %q, which a scenario may not set: %s. "+
					"Remove the line (V29-018)", name, why),
			})
		}
	}

	// item 25 — a malformed basic-auth helper value is refused BY NAME, the same way a malformed
	// body assertion is (R3 above): `Basic ${basic_auth:...}` that does not parse would otherwise be
	// sent to the SUT as a literal, useless Authorization header, and nothing would say why auth
	// keeps failing.
	if v, ok := s.Trigger.Headers["Authorization"]; ok {
		if why := ValidateBasicAuthHeader(v); why != "" {
			errs = append(errs, Error{
				Line:    lineContaining(text, "Authorization:", s.sectionStart("TRIGGER", metaLine)),
				Message: fmt.Sprintf("## TRIGGER's Authorization header %s", why),
			})
		}
	}

	// ── V30-003 — THE SECTION CONTRACT (VR12-S1/S2, M1-M4, T1/T2/T4, VF1/VF3/VF10, TO1-TO3) ─────
	//
	// The closed lists come from schemas/scenario.schema.yaml, loaded once at init (schema.go), so
	// there is ONE source of truth rather than a list in Go and a list in a schema nothing read.
	errs = append(errs, sectionErrors(s, text)...)
	errs = append(errs, metadataErrors(s, text, metaLine)...)
	errs = append(errs, triggerErrors(s, text, metaLine)...)
	errs = append(errs, verifyErrors(s, text, metaLine)...)
	errs = append(errs, timeoutErrors(s, text, metaLine)...)

	// RO-09 (A1/A2): a Database State VERIFY must be EXECUTABLE. A prose-only VERIFY
	// (no ```sql block) silently degrades to the runner's `SELECT 1` default — a tautology
	// that returns one row regardless of SUT state → a FALSE GREEN (ORDE-007). Reject it so
	// it never reaches a run (DESIGN.md principle #6 "executable artifacts only"; spec 02
	// strict validation = Fail).
	if contains(s.Layers, "Database State") && strings.TrimSpace(s.Verify.SQL) == "" {
		errs = append(errs, Error{
			Line:    s.sectionStart("VERIFY", metaLine),
			Message: "Database State VERIFY must contain an executable query (```sql block) — a prose-only VERIFY degrades to `SELECT 1` (a tautology) → false green (RO-09)",
		})
	}

	errs = append(errs, loadErrors(s)...)
	errs = append(errs, amqpLoadErrors(s, text, metaLine)...)
	errs = append(errs, httpLoadErrors(s, text, metaLine)...)
	errs = append(errs, compareErrors(s, text, metaLine)...)

	return s, errs
}

// loadFieldBounds is the "positive, bounded" rule for every `## LOAD` field (AC-11): min/max
// inclusive, and whether 0 is allowed (only DurationSeconds may be — "no scheduler cutoff").
var loadFieldBounds = map[string]struct {
	min, max float64
	zeroOK   bool
}{
	"users":            {1, 10000, false},
	"ramp_seconds":     {0, 86400, true},
	"duration_seconds": {0, 86400, true},
	"target_p95_ms":    {1, 600000, false},
	"max_error_rate":   {0.000001, 1, false},
}

// loadErrors validates a declared `## LOAD` block (AC-11): every field present, numeric, positive
// and bounded — refused BY NAME, WITH THE LINE NUMBER, the same convention as every other rule in
// this file. Absent section = no errors = the one-thread/one-loop default (nothing to validate).
func loadErrors(s *Scenario) []Error {
	if !s.LoadDeclared || isAMQPLoad(s) || IsHTTPLoad(s) { // a ramp block is judged by amqpLoadErrors / httpLoadErrors
		return nil
	}
	loadLine := s.sectionStart("LOAD", 1)
	var errs []Error
	for _, f := range loadFieldNames {
		raw, present := s.loadRaw[f.key]
		if !present {
			errs = append(errs, Error{
				Line:    loadLine,
				Message: fmt.Sprintf("## LOAD is missing **%s**", f.label),
			})
			continue
		}
		bounds := loadFieldBounds[f.key]
		v, err := strconv.ParseFloat(raw, 64)
		switch {
		case err != nil:
			errs = append(errs, Error{
				Line:    loadLine,
				Message: fmt.Sprintf("## LOAD **%s** is not a number: %q", f.label, raw),
			})
		case v == 0 && !bounds.zeroOK:
			errs = append(errs, Error{
				Line:    loadLine,
				Message: fmt.Sprintf("## LOAD **%s** must be positive, got 0", f.label),
			})
		case v < 0:
			errs = append(errs, Error{
				Line:    loadLine,
				Message: fmt.Sprintf("## LOAD **%s** must not be negative, got %v", f.label, v),
			})
		case v < bounds.min || v > bounds.max:
			errs = append(errs, Error{
				Line:    loadLine,
				Message: fmt.Sprintf("## LOAD **%s** = %v is out of bounds [%v, %v]", f.label, v, bounds.min, bounds.max),
			})
		}
	}
	return errs
}

// Valid reports whether the markdown passes VR-A10.
func Valid(text string) bool {
	_, errs := Validate(text)
	return len(errs) == 0
}

// CheckID applies the id rule (VR10-S4-1) to one id and returns the refusal text, or "" when the id is allowed —
// for callers that walk already-parsed scenarios (validate-config) and must report the same rule the import
// applies (gate-2 F3: the validator an operator runs BEFORE the import stayed silent about it).
func CheckID(id string) string {
	if id == "" {
		return "missing required Metadata **ID**"
	}
	if !idFormat.MatchString(id) {
		return idRuleMessage(id)
	}
	return ""
}

// stepNames lists a chain's declared step names, for the orphan guard's error message. Naming the
// real names is what turns "that step does not exist" into a one-glance fix.
func stepNames(steps []ChainStep) []string {
	out := make([]string, 0, len(steps))
	for _, st := range steps {
		out = append(out, st.Name)
	}
	return out
}

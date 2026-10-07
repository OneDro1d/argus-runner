package argus

import (
	"fmt"
	"strings"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-E13 — TIER 3, THE RUN-REPORT BACKSTOP.
//
// The owner's model has three tiers, not two:
//
//	1. the authoring skill GUIDES   2. validation REFUSES   3. the run REPORTS
//
// Tier 2 is author-path only — `scenario.Validate` is reached from internal/control/cloudtools.go:251
// and internal/toolcore/toolcore.go:777/:834, never from the run path, which only Parses. So every
// catalogue written before these rules existed keeps running untouched, and WITHOUT TIER 3 an
// unexecutable claim inside one of them stays silently ignored for ever: the exact defect this round
// exists to remove, surviving its own fix.
//
// ⛔ NEVER CHANGES A VERDICT. A scenario is not failed for carrying an unexecuted bullet. Deciding a
// verdict from this would make tier 3 a third enforcement path, which is precisely what "either
// enforced or reported, there is no third state" forbids.
//
// ⛔ WHAT IT REPORTS IS THE TEST'S EXPECTED VALUE, so it is holdout material — redactExpected NILs it
// for the product hat (VR12-E14).

// TemplateReads names, PER TEMPLATE, the `expect.*` properties that template actually reads.
//
// ⛔ IT REPLACES bodyReadingTemplate + dbReadingTemplate (V30-004). Those were two boolean sets —
// "does this template read the body properties" and "…the column properties" — which could only
// answer the two questions someone had thought to ask. DeriveProps computes EVERY expect.* property
// for EVERY layer, so a property is silently unread whenever a template does not consume it, and a
// boolean per family cannot say which.
//
// ⭐ RE-MEASURED 2026-09-13, AND THE OLD COMMENT'S METHOD WAS WRONG. It said the set was found by
// grepping the templates for `__P(expect.` — which matches NOTHING in any template. The `expect.*`
// family is read inside the Groovy post-processors as `props.get("expect.<name>")`; `__P(…)` is how
// the *request* properties (host, port, auth.header, correlation.id) are interpolated. Measured with
// the right needle:
//
//	http-ingestion.jmx    expect.body_contains expect.body_matches expect.body.*
//	database-state.jmx    expect.columns expect.has_rows expect.row_count
//	http-idempotency.jmx  (none)
//	external-delivery.jmx expect.columns expect.has_rows expect.row_count   ← F-2 added them
//	message-flow.jmx      expect.columns expect.row_count (F-3, the tap) + expect.has_rows (F-4)
//	                       + expect.refusal_code (AC-D16 — see the AC-D16 note below)
//
// ⭐ AC-D16: `expect.refusal_code` is a Message Flow content check like the two above it — a
// `- broker refuses with <code>` bullet — but it is not read by a Groovy `props.get(...)` the way
// the tap/negative families are. It configures the AMQP Java Request sampler's own
// `expected_refusal_code` argument (`${__P(expect.refusal_code,)}`), and THAT sampler decides
// pass/fail (jmeter-plugins/amqp's AmqpPublishSampler, unit-tested there). A parity
// `props.get("expect.refusal_code")` read still exists in message-flow.jmx (logged, not judged) so
// TestTemplateReads_IsGroundedInTheTemplateFiles's grounding check — every entry backed by a real
// props.get(...) call — holds for this entry the same way it holds for the rest of the table.
//
// ⚠ `expect.has_rows` on message-flow is read by F-4's element — the negative `- no rows` case —
// and by nothing else. If F-4 is ever dropped, this entry goes with it, or the DoD's gate test
// goes green while nothing reads the property.
//
//	saga-presence.jmx     (none)
//	cleanup-sql.jmx       (none)
//
// ⚠ `expect.status` / `expect.status2` are NOT in this table, though the previous one listed them.
// They appear in http-idempotency.jmx only inside a COMMENT: the status verdict is made in Go
// (judge(codes, expectedCodes, …)), never by the template. A table that claimed otherwise would let
// a status bullet look template-backed when it is not.
//
// ⚠ MATCHED BY PREFIX, because the body family is numbered (`expect.body.1.value`). A template that
// reads `expect.body.` reads the whole family.
//
// ⛔ The keys are the basenames of templates/*.jmx, asserted against the tree by
// TestTemplateReads_CoversExactlyTheShippedTemplates; and every entry is asserted against the FILE
// by TestTemplateReads_IsGroundedInTheTemplateFiles, so the map can never promise a read the
// template does not perform — which would hide this row's own defect behind its fix.
var TemplateReads = map[string][]string{
	"http-ingestion":    {"expect.body_contains", "expect.body_matches", "expect.body."},
	"database-state":    {"expect.columns", "expect.has_rows", "expect.row_count"},
	"http-idempotency":  nil,
	"message-flow":      {"expect.columns", "expect.has_rows", "expect.row_count", "expect.refusal_code"},
	"external-delivery": {"expect.columns", "expect.has_rows", "expect.row_count"},
	"saga-presence":     nil,
	"cleanup-sql":       nil,
	"amqp-load":         nil, // reads no expect.* property: its claims are the ramp's verdict (internal/amqpload)
}

// templateReadsProperty reports whether the template reads a property, by PREFIX.
func templateReadsProperty(base, prop string) bool {
	for _, p := range TemplateReads[base] {
		if strings.HasPrefix(prop, p) {
			return true
		}
	}
	return false
}

// UnexecutedAfterRun is UnexecutedRunnable for a row that has run, and it is what RunAll attaches.
// The one difference is a Web UI scenario (AC-D27): its declared checks are executed by the spec, so
// only a check the spec wrote no outcome for is listed, which a scenario's shape alone cannot know.
func UnexecutedAfterRun(s *scenario.Scenario, res report.ScenarioResult) []report.Unexecuted {
	enforced := res.AssertionsEnforced
	if enforced == nil {
		enforced = []string{} // it ran and enforced nothing (a harness failure) — not "before a run"
	}
	return unexecutedRunnable(s, enforced)
}

// UnexecutedRunnable returns what the shape of a scenario alone proves is not executed; for a Web UI
// scenario that is its bullets that are not declared checks.
func UnexecutedRunnable(s *scenario.Scenario) []report.Unexecuted {
	return unexecutedRunnable(s, nil)
}

// UnexecutedRunnable returns the bullets in a scenario's `### Runnable` that reach a run and are NOT
// executed, each with the reason. It is the source of report.ScenarioResult.Unexecuted.
//
// It deliberately reports only what it can PROVE is not executed from the shape of the scenario and
// the template that will run it — never a guess. A bullet it cannot classify is reported as exactly
// that, which is the honest answer and the one an author can act on.
// enforced is the row's assertions_enforced after a run, and nil before one.
func unexecutedRunnable(s *scenario.Scenario, enforced []string) []report.Unexecuted {
	if s == nil {
		return nil
	}
	var out []report.Unexecuted
	add := func(b, why string) { out = append(out, report.Unexecuted{Bullet: b, Reason: why}) }

	// V29-016 (c): a scenario whose verdict comes from the response code, and which declares no
	// status to compare against, is NAMED here. It is the one tier-3 entry that reports something
	// ABSENT rather than something written — which is the point: the defect was invisible precisely
	// because nothing in the file pointed at it. The run refuses such a scenario as `error`; this
	// row is what tells the author WHY, in the same list as every other unexecuted claim.
	if scenario.JudgedByResponseCode(s) && !scenario.DeclaresStatus(s) {
		add("(no `status=` bullet)", "this scenario's verdict is decided by comparing HTTP response "+
			"codes, and it declares no expected status — nothing was compared, and no code was "+
			"invented on its behalf (V29-016). Add a `status=<code>` bullet under `### Runnable`")
	}

	runnable := s.RunnableExpect()
	if len(runnable) == 0 {
		return out
	}

	switch {
	case contains(s.Tags, ChainTag):
		// ⛔ V31-002 (R5): this listed EVERY runnable bullet as "not executed (V30-002 moves the
		// claims here)" — false since V30-002 landed in 0.3.31, and the owner's rule is that the
		// report holds ONLY what was not run. Two cases really are unexecuted in a VALID file, and
		// they are the ones reported now.
		//
		// ⚠ The steps are parsed HERE, from the raw payload — never through resolveVars: the claim
		// names are literal. If the payload does not decode, tier 3 reports NOTHING: the run already
		// stopped at preflight and quoted it, and a second copy would double-report one fault.
		steps, perr := scenario.ParseChainSteps(s.Trigger.Payload)
		if perr != nil {
			return out
		}
		kind := map[string]string{}
		for _, st := range steps {
			kind[st.Name] = st.Type
		}
		claims, _ := scenario.ParseChainClaims(runnable)
		named := map[string]bool{}
		for name := range claims {
			named[name] = true
		}
		for _, b := range runnable {
			name, _, ok := scenario.SplitChainClaim(b)
			switch {
			case !ok:
				// severity.go accepts any assertion shape on a chain, and ParseChainClaims skips a
				// bullet with no `step` prefix — so it is declared as a check and judged by nobody.
				add(b, "not executed: a chain's runnable bullet must name a step — \"- step <name>: <assertion>\" (V31-002)")
			case kind[name] != "mcp" && kind[name] != "http" && kind[name] != "amqp":
				// the orphan guard only checks that the NAME exists; ExpectProblems skips non-mcp
				// steps and the runner's ui branch ignores claims.
				//
				// AC-D23: an http step's claims ARE judged (httpStepExpect → chain.HTTPStep, recorded in
				// the step's assertions_enforced), and a claim in neither http form is refused at
				// preflight, so none reaches the run unjudged. Listing them here reported a real pass
				// as hollow.
				//
				// AC-D18b: an amqp step's `broker …` claim is judged the same way (chain.AMQPStep, recorded in
				// assertions_enforced); any other form is refused at seed time and again at preflight.
				add(b, fmt.Sprintf("not executed: step %q is not an mcp, http or amqp step, so its claim is not evaluated (V31-002)", name))
			}
		}
		return out

	case contains(s.Tags, UITag):
		// VR12-E11 / V29-021 / AC-D27: a DECLARED check (`dom …`, `no backend …`, `no console …`) is
		// evaluated by testkit/ui/tests/live/argus-declared.spec.ts, which writes one outcome per
		// check, and the row records it in assertions_enforced. So a declared check is unexecuted only
		// when a run wrote no outcome for it. Any other runnable bullet is legal but inert, and saying
		// so is the point.
		ran := map[string]bool{}
		for _, b := range enforced {
			ran[b] = true
		}
		for _, b := range runnable {
			declared, _ := scenario.ParseUIExpect([]string{b})
			switch {
			case len(declared) == 0:
				add(b, "not a declared Web UI check (`dom …`, `no backend …`, `no console …`): a Web UI "+
					"scenario's verdict comes from the Playwright exit code and its declared checks, so "+
					"this bullet is not executed (V29-021)")
			case enforced != nil && !ran[declared[0].Bullet]:
				add(b, "a declared Web UI check the spec wrote no outcome for: it was not evaluated in "+
					"this run (V29-021)")
			}
		}
		return out

	case contains(s.Tags, MCPTag):
		// The MCP path never JUDGES a bullet outside `### Runnable` (V31-006) — the failure record
		// still quotes them all (VR12-E14). It evaluates a `### Runnable` body check on an expected
		// error too (V31-005), so an error step's checks are not dropped here either.
		//
		// ⛔ V31-002 (R5): it used to `return nil`, so the one real gap on this path was SILENT.
		// Measured 2026-09-12: `- result.isError == false` + `- the answer comes back quickly` gave
		// 2 runnable bullets, tier 3 [], ExpectProblems [] — the second bullet was declared as a
		// check, not run, and not reported. That was the last third state on this path (V29-020):
		// an assertion-shaped bullet in no known form is already refused at preflight, and an
		// executable one is executed, so PROSE is what remains.
		for _, b := range runnable {
			if cb, err := classifyExpectBullet(b); err == nil && cb.kind == expectProse {
				add(b, "not executed: this bullet declares no check the MCP engine can evaluate (V31-002)")
			}
		}
		return out
	}

	// ── the JMeter path ───────────────────────────────────────────────────────────────────────
	layer := PrimaryLayer(s)
	// an AMQP Load scenario's claims are the closed load vocabulary, which the ramp's
	// verdict enforces EVERY time (internal/amqpload.Verdict) and the validator refuses anything else.
	// The DB/body grammars below know nothing of it and would call it unexecutable.
	if layer == scenario.AMQPLoadLayer || layer == scenario.HTTPLoadLayer { // the HTTP ramp's vocabulary, httpload.Verdict
		return out
	}
	// ⛔ V30-004 T-A: the TEMPLATE THAT WILL RUN, not the layer's default. A saga-presence scenario's
	// layer may say database-state while saga-presence.jmx is what runs, and reporting against the
	// wrong template would name a capability the run never had.
	base := RuntimeTemplateBase(s)
	dbx := scenario.ParseDBExpect(runnable)

	for _, b := range runnable {
		switch {
		case scenario.ClaimsToBeBodyAssert(b):
			if !templateReadsProperty(base, "expect.body_contains") {
				add(b, fmt.Sprintf("a body assertion is only evaluated by a template that reads the body "+
					"properties; this scenario's primary layer %q runs on %s.jmx, which does not read "+
					"expect.body_contains / expect.body_matches", layer, base))
			}
		case isDBFormBullet(dbx, b):
			if !templateReadsProperty(base, "expect.columns") {
				add(b, fmt.Sprintf("a column / row-count assertion is only evaluated by a template that "+
					"reads the column properties; this scenario's primary layer %q runs on %s.jmx, which "+
					"does not read expect.columns / expect.row_count / expect.has_rows", layer, base))
			}
		}
	}
	// AC-D31: a row-existence bullet (`row_count == N` / `no rows`) over a VERIFY that always returns
	// exactly one row is RUN but judges nothing — the template counts rows, and an ungrouped aggregate
	// has one whatever it counts. VF14 refuses it at write time; this names it for every catalogue
	// written before VF14 existed. The verdict is untouched, as everywhere in tier 3.
	if templateReadsProperty(base, "expect.row_count") && scenario.SingleRowAggregate(s.Verify.SQL) {
		for _, b := range runnable {
			if one := scenario.ParseDBExpect([]string{b}); one.RowCount >= 0 || !one.HasRows {
				add(b, "judged on the SHAPE of the answer, not its value: the VERIFY query is an aggregate "+
					"with no GROUP BY, so it returns exactly one row whatever it counts — `row_count == N` "+
					"cannot fail and `no rows` cannot pass. Alias the aggregate and assert its value "+
					"(`n == 0`) instead (VF14, AC-D31)")
			}
		}
	}
	// Anything the DB grammar could not execute is already collected by ParseDBExpect, and it is a
	// per-clause answer rather than a per-bullet one — report it verbatim with its own reason.
	for _, u := range dbx.Uncoverable {
		add(u, "the content grammar cannot execute this form (a range, a JSONB path, a cast, an "+
			"unsupported operator, or a column assertion that did not parse)")
	}
	return out
}

// isDBFormBullet reports whether a bullet contributed a column / row-count / negative assertion —
// i.e. whether the DB grammar recognised it at all. Recognition is what makes its inertness
// reportable: a bullet the grammar never claimed is somebody else's business.
func isDBFormBullet(dbx scenario.DBExpect, bullet string) bool {
	one := scenario.ParseDBExpect([]string{bullet})
	return len(one.Columns) > 0 || one.RowCount >= 0 || !one.HasRows
}

// unreadablePropertyProblems is V30-004's F-1: a content assertion whose property NO template can
// read is refused when the scenario is WRITTEN.
//
// The class exists because DeriveProps computes a property set per LAYER while the template is
// chosen per layer too, and the two lists were never tied together — so a column assertion on a
// Message Flow scenario was computed into `expect.columns`, read by nothing, and the scenario passed
// on the correlation-id "a message exists" proxy alone.
//
// ⛔ It keys on RuntimeTemplateBase, never on the layer (T-A): `status2=` on HTTP Ingestion switches
// the run to http-idempotency.jmx, which reads no content property at all, and the saga tag switches
// it to saga-presence.jmx. A rule keyed on the layer would miss both.
//
// ⛔ SCOPE, and each exclusion is a rule rather than an outcome:
//   - the JMeter path ONLY. `mcp` and `chain` scenarios are judged in Go, and a `ui` scenario runs
//     Playwright — saying it "runs ui.jmx, which does not read …" would be false, and V29-021 owns
//     that layer's grammar.
//   - the two CONTENT classes only — the body family and the DB form. `expect.status` is emitted for
//     every scenario and read by no template (the verdict is made in Go), so a rule keyed on
//     "emitted but unread" would refuse `- status=202` on every layer.
func unreadablePropertyProblems(s *scenario.Scenario) []ExpectProblem {
	if s == nil || contains(s.Tags, MCPTag) || contains(s.Tags, ChainTag) || contains(s.Tags, UITag) ||
		PrimaryLayer(s) == scenario.AMQPLoadLayer || PrimaryLayer(s) == scenario.HTTPLoadLayer { // its claims are the load vocabulary (amqpload / httpload Verdict)
		return nil
	}
	runnable := s.RunnableExpect()
	if len(runnable) == 0 {
		return nil
	}
	base := RuntimeTemplateBase(s)
	layer := PrimaryLayer(s)
	dbx := scenario.ParseDBExpect(runnable)

	var out []ExpectProblem
	add := func(bullet, prop string) {
		out = append(out, ExpectProblem{Bullet: bullet, Message: fmt.Sprintf(
			"`- %s` cannot be evaluated on this scenario's layer: `%s` runs `%s.jmx`, which does not read `%s`. "+
				"Put this assertion on a layer that evaluates it (`Database State` for a row, `Message Flow` for a "+
				"published message, `External Delivery` for a delivered payload), or move the sentence to "+
				"`### Non-runnable`", bullet, layer, base, prop)})
	}
	for _, b := range runnable {
		switch {
		case scenario.ClaimsToBeBodyAssert(b):
			if !templateReadsProperty(base, "expect.body_contains") {
				add(b, "expect.body_contains / expect.body_matches")
			}
		case isDBFormBullet(dbx, b):
			if !templateReadsProperty(base, "expect.columns") {
				add(b, "expect.columns / expect.row_count / expect.has_rows")
			}
		}
	}
	return out
}

// secondRequestProblems is AC-D30 (#171), option 2: a declared SECOND status that the template which
// will run cannot answer is refused when the scenario is WRITTEN.
//
// The judge honours `status2=` on every layer, but only two templates send a second request:
// http-idempotency.jmx always, and database-state.jmx under trigger.second (its `<id>-trigger2`).
// Anywhere else the second response cannot exist, so the scenario is red against every SUT, healthy
// or not — ORDE-013 was, on every Path A onboarding, until database-state learned the second request.
// RuntimeTemplateBase's comment promised this refusal ("miss an illegal idempotency one"); it did
// not exist.
//
// ⛔ Same scope as unreadablePropertyProblems, for the same reasons: the JMeter path only, and keyed
// on the template that will RUN (T-A), never on the layer — the saga tag moves an HTTP Ingestion
// scenario onto saga-presence.jmx, which sends one request.
func secondRequestProblems(s *scenario.Scenario) []ExpectProblem {
	if s == nil || contains(s.Tags, MCPTag) || contains(s.Tags, ChainTag) || contains(s.Tags, UITag) {
		return nil
	}
	runnable := s.RunnableExpect()
	if _, second := scenario.DeclaredStatuses(runnable); second == 0 {
		return nil
	}
	base := RuntimeTemplateBase(s)
	if templateSendsASecondRequest(base) {
		return nil
	}
	bullet := ""
	for _, b := range runnable {
		if _, second := scenario.DeclaredStatuses([]string{b}); second > 0 {
			bullet = strings.TrimSpace(b)
			break
		}
	}
	return []ExpectProblem{{Bullet: bullet, Message: fmt.Sprintf(
		"AC-D30: `- %s` declares a SECOND response, but this scenario's layer `%s` runs `%s.jmx`, which sends "+
			"one request, so the second response can never arrive and the scenario would be red against any "+
			"system. Only `http-idempotency.jmx` (a second status on `HTTP Ingestion`) and `database-state.jmx` "+
			"(on `Database State`) send a second request: declare it on one of those layers, or drop `status2=`",
		bullet, PrimaryLayer(s), base)}}
}

// templateSendsASecondRequest names the templates that can answer a declared `status2=`: they send a
// second request to the SUT under the same Idempotency-Key. TestACD30_SecondRequestTemplatesAreGroundedInTheFiles
// ties it to templates/*.jmx, the way TemplateReads is tied, so the two cannot drift apart.
func templateSendsASecondRequest(base string) bool {
	switch base {
	case "http-idempotency", "database-state":
		return true
	}
	return false
}

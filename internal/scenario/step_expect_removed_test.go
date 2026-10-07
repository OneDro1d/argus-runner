package scenario

import (
	"strings"
	"testing"
)

// V31-002 (VR13-OF) — THE OLD FORMAT IS REFUSED WHEN A SCENARIO IS WRITTEN, AND NOWHERE ELSE.
//
// The owner, 2026-09-11: old-format scenarios are not supported and must be rewritten. And
// 2026-09-12, on where that is enforced: *"Refuse is a job of Validation and Writing, not running."*
// So every rule in this file is an AUTHOR-PATH rule. The run keeps no copy of them, which is what
// stops a rule change from re-judging a catalogue that already runs.
//
// ⚠ Assert by CONTAINS on each named message, never by count: these fixtures legitimately collect
// several errors (a file can be wrong in more than one way), and a count test breaks on the next
// rule anyone adds — for the right reason, which is the worst kind of red.

func hasMessage(errs []Error, needle string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, needle) {
			return true
		}
	}
	return false
}

// W1 — a step carrying `expect` is refused BY NAME. R2 deletes the key from the struct, so without
// this the author's claims would simply be ignored: the file would save, run, and prove nothing.
func TestValidate_StepExpectKeyIsRefusedByName(t *testing.T) {
	const trigger = `{"steps":[{"type":"mcp","name":"create","tool":"t","args":{},"expect":"result.isError == false"},{"type":"mcp","name":"read","tool":"t","args":{}}]}`

	t.Run("with claims in the new home too", func(t *testing.T) {
		md := chainMD(trigger, strings.Join([]string{
			"### Runnable",
			"- step create: result.isError == false",
			"- step read: result.isError == false", "",
		}, "\n"))
		_, errs := Validate(md)
		if !hasMessage(errs, "step \"create\" carries an `expect` key") {
			t.Errorf("the `expect` key was not refused by name: %v", errs)
		}
		// ⛔ GUARD 1's "both homes" message is GONE: the key is refused on its own now, whether or
		// not `## EXPECT` carries claims. Keeping both would tell an author the problem is that they
		// wrote it twice, when the problem is that one of the two homes no longer exists.
		if hasMessage(errs, "BOTH homes") {
			t.Errorf("the superseded \"BOTH homes\" message is still emitted: %v", errs)
		}
	})

	t.Run("with no claims at all, both faults are named", func(t *testing.T) {
		md := chainMD(trigger, "### Non-runnable\n- prose about the test\n")
		_, errs := Validate(md)
		if !hasMessage(errs, "step \"create\" carries an `expect` key") {
			t.Errorf("W1 did not fire: %v", errs)
		}
		// GUARD 3's exemption for legacy steps is deleted, so a step carrying the key AND no claim
		// gets both errors — two things really are wrong with it.
		if !hasMessage(errs, `step "create" has no claim`) {
			t.Errorf("GUARD 3 did not fire for the step carrying the key: %v", errs)
		}
		if !hasMessage(errs, `step "read" has no claim`) {
			t.Errorf("GUARD 3 did not fire for the other step: %v", errs)
		}
	})
}

// W2 — a scenario must declare at least one RUNNABLE check. Not a new rule: the closing of an
// old-format window. VR12-E12 deliberately counted BULLETS rather than runnable ones, to keep the
// nine all-prose chain scenarios saveable until V30-002 migrated them. It did, in 0.3.31.
func TestValidate_AScenarioMustDeclareARunnableCheck(t *testing.T) {
	const needle = "## EXPECT declares no '### Runnable' bullet"

	t.Run("a runnable bullet — no error", func(t *testing.T) {
		if _, errs := Validate(fullMD(nil)); hasMessage(errs, needle) {
			t.Errorf("a scenario WITH a runnable check was refused: %v", errs)
		}
	})

	t.Run("only `### Non-runnable` bullets — refused", func(t *testing.T) {
		md := fullMD(func(m string) string {
			return strings.Replace(m, "### Runnable\n- status=202", "### Non-runnable\n- the order is created", 1)
		})
		_, errs := Validate(md)
		if !hasMessage(errs, needle) {
			t.Errorf("a scenario that asserts NOTHING was accepted: %v", errs)
		}
	})

	t.Run("a `ui` scenario naming its own spec — no error", func(t *testing.T) {
		// DEC-3, the owner's ruling: a ui scenario's assertions live in its spec file, so naming one
		// satisfies the rule. (From V29-021 it may declare bullets instead; both are accepted.)
		md := strings.Join([]string{
			"# Scenario: u", "",
			"## Metadata", "- **ID**: UI-001", "- **Layer**: Web UI", "- **Tags**: ui", "",
			"## TRIGGER",
			"POST \x60tests/live/x.spec.ts\x60", "",
			"```json",
			`{"spec":"tests/live/x.spec.ts","app_url":"http://sut.invalid"}`,
			"```", "",
			"## EXPECT",
			"### Non-runnable", "- the dashboard renders", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		if _, errs := Validate(md); hasMessage(errs, needle) {
			t.Errorf("a ui scenario naming its own spec was refused: %v", errs)
		}
	})

	t.Run("a `ui` scenario that names NOTHING", func(t *testing.T) {
		// ⚠ MEASURED, and it changes what this case can assert: a `ui` scenario with no `spec` field
		// AND no TRIGGER url is not reachable through the validator — `## TRIGGER` is required and its
		// url is what the ui path falls back to, so every saveable ui scenario names a spec one way or
		// the other. W2's ui branch is therefore a rule with no failing case TODAY; it exists because
		// R10 asks the same question at run time about files written before these rules, and the two
		// must answer identically (one definition, UINamesItsOwnSpec).
		//
		// What IS assertable is the predicate itself, which is what both paths actually consult.
		md := strings.Join([]string{
			"# Scenario: u", "",
			"## Metadata", "- **ID**: UI-002", "- **Layer**: Web UI", "- **Tags**: ui", "",
			"## TRIGGER",
			"POST `tests/live/x.spec.ts`", "",
			"## EXPECT",
			"### Non-runnable", "- the dashboard renders", "",
			"## TIMEOUT", "30s", "",
			"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
		}, "\n")
		sc := Parse(md)
		if !UINamesItsOwnSpec(sc) {
			t.Error("a TRIGGER url alone must satisfy the rule — it is what the ui path falls back to")
		}
		sc.Trigger.URL, sc.Trigger.Payload = "", ""
		if UINamesItsOwnSpec(sc) {
			t.Error("a ui scenario naming neither a spec nor a url must NOT satisfy the rule")
		}
		sc.Trigger.Payload = `{"spec":"tests/live/x.spec.ts"}`
		if !UINamesItsOwnSpec(sc) {
			t.Error("a `spec` payload field must satisfy the rule")
		}
		sc.Trigger.Payload = "not json"
		if UINamesItsOwnSpec(sc) {
			t.Error("a payload that is not JSON must answer false, never panic")
		}
	})

	t.Run("an EMPTY `## EXPECT` keeps today's bullet-count error", func(t *testing.T) {
		md := fullMD(func(m string) string {
			return strings.Replace(m, "### Runnable\n- status=202\n", "", 1)
		})
		_, errs := Validate(md)
		if hasMessage(errs, needle) {
			t.Errorf("W2 fired on a file with no bullets at all — the bullet count already covers it: %v", errs)
		}
		if len(errs) == 0 {
			t.Error("a file with an empty ## EXPECT must still be refused")
		}
	})
}

// GUARD (W3) — the two refusals that already do this work must not regress while R1 removes the
// run-time leniency that used to hide them.
func TestValidate_TheOldShapesStayRefused(t *testing.T) {
	t.Run("a bullet directly under ## EXPECT", func(t *testing.T) {
		md := fullMD(func(m string) string { return strings.Replace(m, "### Runnable\n", "", 1) })
		if _, errs := Validate(md); !hasMessage(errs, "must be split") {
			t.Errorf("a flat ## EXPECT was accepted: %v", errs)
		}
	})
	t.Run("a missing ## TIMEOUT", func(t *testing.T) {
		md := fullMD(func(m string) string { return strings.Replace(m, "## TIMEOUT\n15s\n\n", "", 1) })
		if _, errs := Validate(md); !hasMessage(errs, "## TIMEOUT is required") {
			t.Errorf("a missing ## TIMEOUT was accepted: %v", errs)
		}
	})
}

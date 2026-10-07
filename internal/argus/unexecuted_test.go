package argus

import (
	"strings"
	"testing"

	"github.com/OneDro1d/argus-runner/internal/report"
	"github.com/OneDro1d/argus-runner/internal/scenario"
)

// VR12-E13 / VR12-E14 — tier 3: what reached a run and was NOT executed, and the holdout that
// covers it.

func unexecFor(t *testing.T, md string) []report.Unexecuted {
	t.Helper()
	return UnexecutedRunnable(scenario.Parse(md))
}

func scenTagged(layer, tags, expectBody string) string {
	return strings.Join([]string{
		"# Scenario: t", "",
		"## Metadata",
		"- **ID**: T-001",
		"- **Layer**: " + layer,
		"- **Tags**: http, " + tags, "",
		"## TRIGGER",
		"POST `${INGESTION_URL}/api/v1/orders`", "",
		"## EXPECT",
		expectBody,
		"## TIMEOUT", "30s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
}

func TestVR12E13_ReportsWhatWasNotExecuted(t *testing.T) {
	t.Run("a body assertion on a layer whose template cannot read it", func(t *testing.T) {
		// ⛔ THE CASE PERM-001 SHIPPED UNTIL 0.3.32 (V31-004 removed its body bullet). Its primary
		// layer is Database State, so it runs on
		// database-state.jmx — which reads expect.columns/has_rows/row_count and NOT the body
		// properties. DeriveProps sets them for every layer regardless, so this assertion has been
		// computed and never evaluated.
		got := unexecFor(t, scenTagged("Permissions -> Database State", "order, permissions",
			"### Runnable\n- status=401\n- body has error containing \"missing or invalid bearer token\"\n- no rows\n"))
		if len(got) != 1 {
			t.Fatalf("want exactly one unexecuted bullet, got %d: %+v", len(got), got)
		}
		if !strings.Contains(got[0].Bullet, "missing or invalid bearer token") {
			t.Errorf("the bullet must be quoted VERBATIM so the author can find the line: %q", got[0].Bullet)
		}
		for _, want := range []string{"body assertion", "database-state.jmx"} {
			if !strings.Contains(got[0].Reason, want) {
				t.Errorf("reason must name %q: %s", want, got[0].Reason)
			}
		}
	})

	// ⭐ INVERTED BY V30-004 (T-H), now that message-flow.jmx actually reads the column family.
	//
	// It asserted that BOTH column assertions on a Message Flow scenario are reported unexecuted —
	// true, and this row's own evidence: the template read no `expect.*` property at all, so
	// FINDING-7's replacement of the correlation-id "a message exists" proxy landed in
	// database-state.jmx and was never carried across. F-3's tap carries it across, so the same
	// fixture must now report NOTHING: the report holds only what was not run, and these are run.
	t.Run("⭐ a column assertion on Message Flow is EXECUTED since V30-004", func(t *testing.T) {
		got := unexecFor(t, scenTagged("HTTP Ingestion -> Message Flow", "order, message-flow",
			"### Runnable\n- status=202\n- event == OrderCreated\n- currency == EUR\n"))
		if len(got) != 0 {
			t.Fatalf("message-flow.jmx reads the column properties now — nothing may be reported: %+v", got)
		}
	})

	t.Run("…and a BODY assertion on Message Flow is still reported", func(t *testing.T) {
		// The row carried across the COLUMN family only. message-flow.jmx still reads no
		// expect.body_* property, so a `body has` bullet there is computed and never read.
		got := unexecFor(t, scenTagged("HTTP Ingestion -> Message Flow", "order, message-flow",
			"### Runnable\n- status=202\n- body has order_id containing 01\n"))
		if len(got) != 1 || !strings.Contains(got[0].Reason, "message-flow.jmx") {
			t.Fatalf("a body assertion on Message Flow must still be reported: %+v", got)
		}
	})

	// ⛔ REPLACED BY V31-002 (R5). This asserted that a chain's own `## EXPECT` is reported as not
	// executed, "judged per STEP from the step's own `expect`". That was true before V30-002 and
	// false after it landed in 0.3.31 — the claims ARE the chain's EXPECT now — so tier 3 was
	// reporting every runnable bullet of every chain as unexecuted. The owner's rule: the report
	// holds only what was NOT run. Two shapes really are unexecuted in a file the validator accepts,
	// and they are what the two cases below pin.
	t.Run("a chain bullet that names no step is reported", func(t *testing.T) {
		got := chainUnexec(t, `{"steps":[{"type":"mcp","name":"a","tool":"t","args":{}}]}`,
			"### Runnable"+"\n"+"- step a: result.isError == false"+"\n"+"- result.isError == false"+"\n")
		if len(got) != 1 || !strings.Contains(got[0].Reason, "must name a step") {
			t.Fatalf("a bullet with no `step` prefix is judged by nobody and must be reported: %+v", got)
		}
	})

	t.Run("a claim naming a non-mcp step is reported", func(t *testing.T) {
		got := chainUnexec(t, `{"steps":[{"type":"mcp","name":"a","tool":"t","args":{}},{"type":"ui","name":"b","spec":"x.spec.ts"}]}`,
			"### Runnable"+"\n"+"- step a: result.isError == false"+"\n"+"- step b: result.isError == false"+"\n")
		if len(got) != 1 || !strings.Contains(got[0].Reason, "is not an mcp, http or amqp step") {
			t.Fatalf("a claim on a ui step is evaluated by nobody and must be reported: %+v", got)
		}
	})

	// AC-D23: AC-D20 made http a chain step type whose claims ARE judged (httpStepExpect →
	// chain.HTTPStep, recorded per step in assertions_enforced), but this list still called every
	// non-mcp claim "not executed". Measured 2026-09-18 on coder-home CHM-008: 13 claims enforced
	// and passed at step level, all 13 listed here as not executed — a pass reported as hollow.
	t.Run("a claim on an http step IS executed, so it is not reported", func(t *testing.T) {
		got := chainUnexec(t, `{"steps":[{"type":"http","name":"create","method":"POST","url":"http://sut/api"},{"type":"ui","name":"b","spec":"x.spec.ts"}]}`,
			"### Runnable"+"\n"+"- step create: status=201"+"\n"+"- step create: body has name containing argus-"+"\n"+"- step b: result.isError == false"+"\n")
		if len(got) != 1 || got[0].Bullet != "step b: result.isError == false" {
			t.Fatalf("only the ui-step claim is unexecuted; the http-step claims are judged: %+v", got)
		}
	})

	// AC-D18b: amqp joins mcp and http — its `broker …` claim is judged by chain.AMQPStep (recorded
	// in assertions_enforced), and any other form is refused at seed time and again at preflight.
	t.Run("a claim on an amqp step IS executed, so it is not reported", func(t *testing.T) {
		got := chainUnexec(t, `{"steps":[{"type":"amqp","name":"spoof","op":"declare_queue","url_env":"X","queue":"q"},{"type":"ui","name":"b","spec":"x.spec.ts"}]}`,
			"### Runnable"+"\n"+"- step spoof: broker refuses with 403"+"\n"+"- step b: result.isError == false"+"\n")
		if len(got) != 1 || got[0].Bullet != "step b: result.isError == false" {
			t.Fatalf("only the ui-step claim is unexecuted; the amqp-step claim is judged: %+v", got)
		}
	})

	t.Run("a well-formed chain reports NOTHING", func(t *testing.T) {
		got := chainUnexec(t, `{"steps":[{"type":"mcp","name":"a","tool":"t","args":{}}]}`,
			"### Runnable"+"\n"+"- step a: result.isError == false"+"\n")
		if len(got) != 0 {
			t.Fatalf("the report must hold ONLY what was not run: %+v", got)
		}
	})

	t.Run("a ui scenario needs no runnable bullet, and one it has is inert", func(t *testing.T) {
		got := unexecFor(t, scenTagged("Web UI", "ui", "### Runnable\n- status=200\n"))
		if len(got) != 1 || !strings.Contains(got[0].Reason, "Playwright exit code") {
			t.Fatalf("a ui runnable bullet must be reported as not executed: %+v", got)
		}
	})

	t.Run("nothing is reported when everything IS executed", func(t *testing.T) {
		got := unexecFor(t, scenTagged("HTTP Ingestion", "order",
			"### Runnable\n- status=202\n- body has order_id containing 01\n"))
		if len(got) != 0 {
			t.Fatalf("a fully executable scenario must report nothing: %+v", got)
		}
	})

	// Measured 2026-09-18 on hub-dev ARG-SYN-005 (run 20260918T132528316): the whole-body form
	// `body contains …` was listed as "the content grammar cannot execute this form" although the body
	// grammar parses it and the template reads expect.body_contains. The DB grammar's loose-operator
	// rule read `body contains` as a column named `body` with an unsupported operator — so every
	// whole-body check on the JMeter path was reported as not run while it ran.
	t.Run("a whole-body `body contains` bullet IS executed, so it is not reported", func(t *testing.T) {
		for _, b := range []string{
			`body contains hello`,
			`body contains "anthropic/requiresUserInteraction":true`,
			"body matching ^\\{",
		} {
			got := unexecFor(t, scenTagged("HTTP Ingestion", "order", "### Runnable\n- status=200\n- "+b+"\n"))
			if len(got) != 0 {
				t.Errorf("%q is judged by the body grammar — nothing may be reported: %+v", b, got)
			}
		}
	})

	t.Run("a NON-runnable bullet is never reported — it was never meant to be executed", func(t *testing.T) {
		got := unexecFor(t, scenTagged("Message Flow", "order",
			"### Runnable\n- status=202\n\n### Non-runnable\n- event == OrderCreated\n"))
		if len(got) != 0 {
			t.Fatalf("tier 3's population is ### Runnable ONLY: %+v", got)
		}
	})
}

// chainUnexec builds a chain scenario with a real TRIGGER payload — tier 3 parses the steps itself
// to tell an mcp step from a ui one (V31-002 R5), so the payload is part of the fixture.
func chainUnexec(t *testing.T, trigger, expectBody string) []report.Unexecuted {
	t.Helper()
	md := strings.Join([]string{
		"# Scenario: c", "",
		"## Metadata", "- **ID**: CHN-U", "- **Layer**: Permissions", "- **Tags**: chain", "",
		"## TRIGGER", "POST `chain`", "",
		"```json", trigger, "```", "",
		"## EXPECT", expectBody,
		"### Non-runnable", "- a fixture for tier 3", "",
		"## TIMEOUT", "60s", "",
		"## CLEANUP", "N/A — a unit-test fixture; it creates nothing.", "",
	}, "\n")
	return UnexecutedRunnable(scenario.Parse(md))
}

// V31-002 (R5), the MCP half — the LAST silent third state on this path.
//
// Measured 2026-09-12: `### Runnable` / `- result.isError == false` / `- the answer comes back
// quickly` gave 2 runnable bullets, tier 3 `[]` and ExpectProblems `[]`. The second bullet was
// declared as a check, run by nobody, and reported by nobody. An assertion-shaped bullet in no known
// form is already refused at preflight and an executable one is executed, so PROSE is what was left.
func TestVR12E13_MCPProseBulletIsReported(t *testing.T) {
	got := unexecFor(t, scenTagged("HTTP Ingestion", "mcp",
		"### Runnable\n- result.isError == false\n- the answer comes back quickly\n"))
	if len(got) != 1 {
		t.Fatalf("exactly the prose bullet must be reported, got %+v", got)
	}
	if got[0].Bullet != "the answer comes back quickly" {
		t.Errorf("the wrong bullet was reported: %+v", got[0])
	}
	if !strings.Contains(got[0].Reason, "declares no check the MCP engine can evaluate") {
		t.Errorf("reason = %q", got[0].Reason)
	}

	// …and a scenario whose runnable bullets are ALL executable reports nothing.
	if g := unexecFor(t, scenTagged("HTTP Ingestion", "mcp",
		"### Runnable\n- result.isError == false\n- body contains x\n")); len(g) != 0 {
		t.Errorf("the report must hold ONLY what was not run: %+v", g)
	}
}
